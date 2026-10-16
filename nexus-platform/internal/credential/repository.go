package credential

import (
	"context"
	"errors"
	"fmt"

	gomysql "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrNotFound: no live credential with that id in the tenant (absent, deleted, or another
// tenant's — indistinguishable on purpose, so existence never leaks across tenants).
var ErrNotFound = errors.New("credential: not found")

// ErrNameTaken: the tenant already has a live credential with that name (uq_named_cred_name).
var ErrNameTaken = errors.New("credential: name already taken")

// mysqlDuplicateEntry is MySQL's ER_DUP_ENTRY — the unique live-name index.
const mysqlDuplicateEntry = 1062

// Usage is one outbound_webhook action pointing at a credential.
type Usage struct {
	WorkflowID   string `json:"workflowId"`
	WorkflowName string `json:"workflowName"`
	ActionIndex  int    `json:"actionIndex"`
}

// Repository is the GORM store of sys_named_credentials. It never seals or opens — it moves
// envelopes. Every statement is pinned to tenant_id; values always travel as placeholders.
type Repository struct{ db *gorm.DB }

// NewRepository wires the store over the shared GORM connection.
func NewRepository(db *gorm.DB) *Repository { return &Repository{db: db} }

// live pins a query to one tenant's non-deleted rows.
func (r *Repository) live(ctx context.Context, tenantID string) *gorm.DB {
	return r.db.WithContext(ctx).Model(&Credential{}).Where("tenant_id = ? AND deleted_at IS NULL", tenantID)
}

// Create inserts c (c.ID and c.TenantID set by the caller — the AAD needs the id before sealing).
func (r *Repository) Create(ctx context.Context, c *Credential) error {
	if err := r.db.WithContext(ctx).Create(c).Error; err != nil {
		if isDuplicateEntry(err) {
			return fmt.Errorf("credential.Repository.Create: %w", ErrNameTaken)
		}
		return fmt.Errorf("credential.Repository.Create: %w", err)
	}
	return nil
}

// Get returns one live credential of the tenant.
func (r *Repository) Get(ctx context.Context, tenantID, id string) (*Credential, error) {
	var c Credential
	err := r.live(ctx, tenantID).Where("id = ?", id).Take(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("credential.Repository.Get: %w", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("credential.Repository.Get: %w", err)
	}
	return &c, nil
}

// List returns every live credential of the tenant, by name.
func (r *Repository) List(ctx context.Context, tenantID string) ([]Credential, error) {
	var rows []Credential
	if err := r.live(ctx, tenantID).Order("name ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("credential.Repository.List: %w", err)
	}
	return rows, nil
}

// UpdateMetadata rewrites the NON-secret columns of a live credential (label, base URL, header
// name/prefix, username). Sealed material changes only through rotation, never here.
// Contract: FULL replace of those five columns (base_url written unconditionally), so the caller
// must load the live row, apply only the fields the client sent, and refuse an origin change
// (SameOrigin) BEFORE calling — services.CredentialService.Update does (plan D-12, queue #222 LOW 4).
func (r *Repository) UpdateMetadata(ctx context.Context, tenantID string, c *Credential) error {
	res := r.live(ctx, tenantID).Where("id = ?", c.ID).Updates(map[string]any{
		"label":         c.Label,
		"base_url":      c.BaseURL,
		"header_name":   c.HeaderName,
		"header_prefix": c.HeaderPrefix,
		"username":      c.Username,
		"updated_by":    c.UpdatedBy,
	})
	if res.Error != nil {
		return fmt.Errorf("credential.Repository.UpdateMetadata: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		// MySQL counts CHANGED rows: a same-value update reports 0 — tell it apart from "absent".
		var n int64
		if err := r.live(ctx, tenantID).Where("id = ?", c.ID).Count(&n).Error; err != nil {
			return fmt.Errorf("credential.Repository.UpdateMetadata: recheck: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("credential.Repository.UpdateMetadata: %w", ErrNotFound)
		}
	}
	return nil
}

// rotationColumns are the columns a rotation rewrites — every sealed envelope with its hint, expiry
// and master-key id, plus the host and username a rotation may move. Nothing else.
var rotationColumns = []string{"secret_enc", "secret_hint", "prev_secret_enc", "prev_expires_at",
	"extra_headers_enc", "extra_header_names", "key_id", "base_url", "username", "rotated_at", "updated_by"}

// WithTxForUpdate runs fn on the live credential inside a transaction that holds its row lock
// (SELECT … FOR UPDATE, pinned to the tenant): two rotations of one key queue instead of both
// sealing "the previous key" from the same stale read (plan D-13). fn edits the loaded row in place
// and returns nil to have the rotation columns written and committed; any error rolls back.
func (r *Repository) WithTxForUpdate(ctx context.Context, tenantID, id string, fn func(c *Credential) error) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var c Credential
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND deleted_at IS NULL AND id = ?", tenantID, id).Take(&c).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("credential.Repository.WithTxForUpdate: %w", ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("credential.Repository.WithTxForUpdate: lock: %w", err)
		}
		if err := fn(&c); err != nil {
			return err
		}
		if err := tx.Model(&c).Where("tenant_id = ?", tenantID).Select(rotationColumns).Updates(&c).Error; err != nil {
			return fmt.Errorf("credential.Repository.WithTxForUpdate: write: %w", err)
		}
		return nil
	})
}

// TenantIDsSealedUnderOtherKey lists the tenants that still own a live row sealed under a master
// key other than activeKeyID — the boot reseal's worklist. The only statement here that is not
// pinned to one tenant: it names tenants, never rows or secrets.
func (r *Repository) TenantIDsSealedUnderOtherKey(ctx context.Context, activeKeyID string) ([]string, error) {
	var ids []string
	err := r.db.WithContext(ctx).Model(&Credential{}).
		Where("deleted_at IS NULL AND key_id IS NOT NULL AND key_id <> ?", activeKeyID).
		Distinct().Order("tenant_id").Pluck("tenant_id", &ids).Error
	if err != nil {
		return nil, fmt.Errorf("credential.Repository.TenantIDsSealedUnderOtherKey: %w", err)
	}
	return ids, nil
}

// ListSealedUnderOtherKey returns the tenant's live rows sealed under a key other than activeKeyID.
func (r *Repository) ListSealedUnderOtherKey(ctx context.Context, tenantID, activeKeyID string) ([]Credential, error) {
	var rows []Credential
	err := r.live(ctx, tenantID).Where("key_id IS NOT NULL AND key_id <> ?", activeKeyID).Order("id").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("credential.Repository.ListSealedUnderOtherKey: %w", err)
	}
	return rows, nil
}

// Reseal writes the three envelopes and key id of c while the row still carries key_id = fromKeyID
// (compare-and-swap: a rotation that sealed the row under the active key in between wins, and this
// writes nothing). A raw statement on purpose — a re-seal changes no content, so updated_at stays.
func (r *Repository) Reseal(ctx context.Context, tenantID, fromKeyID string, c *Credential) (bool, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE sys_named_credentials SET secret_enc = ?, prev_secret_enc = ?, extra_headers_enc = ?, key_id = ? "+
			"WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL AND key_id = ?",
		c.SecretEnc, c.PrevSecretEnc, c.ExtraHeadersEnc, c.KeyID, tenantID, c.ID, fromKeyID)
	if res.Error != nil {
		return false, fmt.Errorf("credential.Repository.Reseal: %w", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// SoftDeleteShred tombstones a live credential AND destroys its sealed material in ONE UPDATE, so
// no crash window leaves a deleted row still holding secrets. Names stay for the audit trail.
func (r *Repository) SoftDeleteShred(ctx context.Context, tenantID, id, deletedBy string) error {
	res := r.live(ctx, tenantID).Where("id = ?", id).Updates(map[string]any{
		"deleted_at":        gorm.Expr("NOW(3)"),
		"secret_enc":        nil,
		"secret_hint":       nil,
		"prev_secret_enc":   nil,
		"prev_expires_at":   nil,
		"extra_headers_enc": nil,
		"key_id":            nil, // nothing sealed remains, so no master key is needed for this row
		"updated_by":        deletedBy,
	})
	if res.Error != nil {
		return fmt.Errorf("credential.Repository.SoftDeleteShred: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("credential.Repository.SoftDeleteShred: %w", ErrNotFound)
	}
	return nil
}

// ponytail: usages are read out of sys_workflows.actions with JSON_TABLE — one scan of the
// tenant's rules per call, no reference table to keep in sync. Index (or a reference table)
// only if a tenant grows past ~5k rules.

// usagesSQL lists every outbound action of a live rule (enabled or not — a disabled rule can be
// re-enabled) that points at one credential; FOR ORDINALITY is 1-based.
const usagesSQL = `SELECT w.id AS workflow_id, w.name AS workflow_name, jt.idx - 1 AS action_index
FROM sys_workflows w,
     JSON_TABLE(w.actions, '$[*]' COLUMNS (idx FOR ORDINALITY, cid VARCHAR(64) PATH '$.credentialId')) AS jt
WHERE w.tenant_id = ? AND w.deleted_at IS NULL AND jt.cid = ?
ORDER BY w.name, w.id, jt.idx`

// Usages lists the rules of the tenant whose actions reference credential id.
func (r *Repository) Usages(ctx context.Context, tenantID, id string) ([]Usage, error) {
	var rows []Usage
	if err := r.db.WithContext(ctx).Raw(usagesSQL, tenantID, id).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("credential.Repository.Usages: %w", err)
	}
	return rows, nil
}

// countUsagesSQL counts, per credential, the live rules that reference it at least once.
const countUsagesSQL = `SELECT jt.cid AS credential_id, COUNT(DISTINCT w.id) AS rules
FROM sys_workflows w,
     JSON_TABLE(w.actions, '$[*]' COLUMNS (cid VARCHAR(64) PATH '$.credentialId')) AS jt
WHERE w.tenant_id = ? AND w.deleted_at IS NULL AND jt.cid IS NOT NULL
GROUP BY jt.cid`

// CountUsagesByTenant maps credential id → number of live rules using it (the list's usageCount).
func (r *Repository) CountUsagesByTenant(ctx context.Context, tenantID string) (map[string]int, error) {
	var rows []struct {
		CredentialID string
		Rules        int
	}
	if err := r.db.WithContext(ctx).Raw(countUsagesSQL, tenantID).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("credential.Repository.CountUsagesByTenant: %w", err)
	}
	out := make(map[string]int, len(rows))
	for _, row := range rows {
		out[row.CredentialID] = row.Rules
	}
	return out, nil
}

func isDuplicateEntry(err error) bool {
	var me *gomysql.MySQLError
	return errors.As(err, &me) && me.Number == mysqlDuplicateEntry
}
