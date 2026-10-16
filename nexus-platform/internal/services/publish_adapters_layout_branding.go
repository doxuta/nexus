package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

// Draft-layer document types of specs/063 S7-3. A layout's key is "entity:view"; the branding
// document's key is the tenant id (and the adapter never writes by it — see BrandingDocAdapter).
const (
	DocTypeLayout   = "layout"
	DocTypeBranding = "branding"
)

// ---------------------------------------------------------------------------------------------
// layout
// ---------------------------------------------------------------------------------------------

// layoutDoc is the canonical form of a layout: the screen it belongs to and its JSON. No id and no
// timestamps, so a draft saved identical to the live row hashes identical to it (the layout JSON is
// compared through metaversion.Canonical, so MySQL's key re-ordering never matters).
type layoutDoc struct {
	EntityName string          `json:"entityName"`
	ViewType   string          `json:"viewType"`
	Layout     json.RawMessage `json:"layout"`
}

func (d layoutDoc) marshal() (json.RawMessage, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("services.layoutDoc.marshal: %w", err)
	}
	return b, nil
}

// LayoutDocAdapter binds the "layout" document type to sys_layouts.
type LayoutDocAdapter struct{}

// NewLayoutDocAdapter returns the adapter (stateless).
func NewLayoutDocAdapter() *LayoutDocAdapter { return &LayoutDocAdapter{} }

// Type implements metaversion.DocAdapter.
func (*LayoutDocAdapter) Type() string { return DocTypeLayout }

// LoadLive implements metaversion.DocAdapter.
func (*LayoutDocAdapter) LoadLive(ctx context.Context, q sqlx.QueryerContext, tenantID, key string, forUpdate bool) (json.RawMessage, bool, error) {
	entity, view, ok := splitLayoutKey(key)
	if !ok {
		return nil, false, nil
	}
	query := `SELECT CAST(layout_json AS CHAR) FROM sys_layouts
		WHERE tenant_id = ? AND entity_name = ? AND view_type = ? AND deleted_at IS NULL`
	if forUpdate {
		query += " FOR UPDATE"
	}
	var js sql.NullString
	if err := sqlx.GetContext(ctx, q, &js, query, tenantID, entity, view); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("services.LayoutDocAdapter.LoadLive: %w", err)
	}
	b, err := layoutDoc{EntityName: entity, ViewType: view, Layout: rawOrNull([]byte(js.String))}.marshal()
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// ListLiveKeys implements metaversion.DocAdapter.
func (*LayoutDocAdapter) ListLiveKeys(ctx context.Context, q sqlx.QueryerContext, tenantID string) ([]metaversion.DocRef, error) {
	var rows []struct {
		EntityName string `db:"entity_name"`
		ViewType   string `db:"view_type"`
	}
	if err := sqlx.SelectContext(ctx, q, &rows, `SELECT entity_name, view_type FROM sys_layouts
		WHERE tenant_id = ? AND deleted_at IS NULL ORDER BY entity_name, view_type`, tenantID); err != nil {
		return nil, fmt.Errorf("services.LayoutDocAdapter.ListLiveKeys: %w", err)
	}
	out := make([]metaversion.DocRef, len(rows))
	for i, r := range rows {
		out[i] = metaversion.DocRef{Key: layoutDocKey(r.EntityName, r.ViewType), Label: layoutDraftLabel(r.EntityName, r.ViewType), EntityName: r.EntityName}
	}
	return out, nil
}

// ApplyTx implements metaversion.DocAdapter: ONE change through the same statements the layout
// repository uses, on the publish transaction. The row's updated_at becomes the instant the draft was
// last saved — never the publish instant — so a field created after that save lands in "Mới thêm"
// and one the builder deliberately left off the form stays off.
func (*LayoutDocAdapter) ApplyTx(ctx context.Context, tx *sqlx.Tx, tenantID string, c metaversion.Change) error {
	entity, view, ok := splitLayoutKey(c.DocKey)
	if !ok {
		return fmt.Errorf("services.LayoutDocAdapter.ApplyTx: malformed key %q", c.DocKey)
	}
	switch c.Op {
	case metaversion.OpDelete:
		if _, err := metadata.DeleteLayoutByViewExec(ctx, tx, tenantID, entity, view); err != nil {
			return fmt.Errorf("services.LayoutDocAdapter.ApplyTx: delete %s: %w", c.DocKey, err)
		}
		return nil
	case metaversion.OpUpsert:
		var d layoutDoc
		if err := json.Unmarshal(c.After, &d); err != nil {
			return fmt.Errorf("services.LayoutDocAdapter.ApplyTx: decode draft %s: %w", c.DocKey, err)
		}
		if d.EntityName != entity || d.ViewType != view {
			return fmt.Errorf("services.LayoutDocAdapter.ApplyTx: draft %s names %s/%s", c.DocKey, d.EntityName, d.ViewType)
		}
		id := c.DraftID
		if id == "" {
			id = uuid.NewString()
		}
		l := &metadata.LayoutDefinition{ID: id, TenantID: tenantID, EntityName: entity, ViewType: view, LayoutJSON: string(d.Layout)}
		// Truncate: the column has second precision and rounds; a rounded-up mark could hide a field
		// born in the half-second after the save.
		if err := metadata.UpsertLayoutExec(ctx, tx, l, c.DraftUpdatedAt.Truncate(time.Second)); err != nil {
			return fmt.Errorf("services.LayoutDocAdapter.ApplyTx: upsert %s: %w", c.DocKey, err)
		}
		return nil
	}
	return fmt.Errorf("services.LayoutDocAdapter.ApplyTx: unknown op %q", c.Op)
}

// ValidateFinal implements metaversion.DocAdapter: every screen that will be live belongs to a table
// that still exists. Fields are not re-checked: serve-time tolerance drops a dead one.
func (*LayoutDocAdapter) ValidateFinal(ctx context.Context, f metaversion.FinalState) []metaversion.Problem {
	var problems []metaversion.Problem
	checked := map[string]bool{}
	for _, c := range f.Changes {
		if c.DocType != DocTypeLayout || c.Op != metaversion.OpUpsert {
			continue
		}
		entity, _, ok := splitLayoutKey(c.DocKey)
		if !ok {
			problems = append(problems, metaversion.Problem{DocType: DocTypeLayout, DocKey: c.DocKey, Code: "LAYOUT_DRAFT_UNREADABLE", Message: "bản nháp của màn này không đọc được"})
			continue
		}
		if checked[entity] {
			continue
		}
		checked[entity] = true
		if !entityExistsTx(ctx, f, entity) {
			problems = append(problems, metaversion.Problem{DocType: DocTypeLayout, DocKey: c.DocKey, Code: "ENTITY_NOT_FOUND",
				Message: fmt.Sprintf("màn của bảng %q không xuất bản được vì bảng không còn tồn tại", entity)})
		}
	}
	return problems
}

// Redact implements metaversion.DocAdapter: a layout holds no secret.
func (*LayoutDocAdapter) Redact(doc json.RawMessage) json.RawMessage { return doc }

// Entities implements metaversion.DocAdapter: layouts have no engine cache to drop.
func (*LayoutDocAdapter) Entities(json.RawMessage) []string { return nil }

// ---------------------------------------------------------------------------------------------
// branding
// ---------------------------------------------------------------------------------------------

// BrandingDocAdapter binds the "branding" document type to tenants.branding. The document is the
// tenant's white-label document; its key is the tenant id, but the key is DATA in a table, not an
// authority: ApplyTx writes the row of the tenant the publish runs for and refuses a key that is not
// that tenant.
type BrandingDocAdapter struct{}

// NewBrandingDocAdapter returns the adapter (stateless).
func NewBrandingDocAdapter() *BrandingDocAdapter { return &BrandingDocAdapter{} }

// Type implements metaversion.DocAdapter.
func (*BrandingDocAdapter) Type() string { return DocTypeBranding }

// brandingDocLabel is what the change list calls the document.
const brandingDocLabel = "Thương hiệu"

// canonicalBranding re-encodes a stored document through Branding (omitempty), so "no branding yet"
// and "{}" are one form and field order never matters.
func canonicalBranding(raw []byte) (json.RawMessage, error) {
	var b Branding
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("decode branding: %w", err)
		}
	}
	out, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("encode branding: %w", err)
	}
	return out, nil
}

// LoadLive implements metaversion.DocAdapter.
func (*BrandingDocAdapter) LoadLive(ctx context.Context, q sqlx.QueryerContext, tenantID, key string, forUpdate bool) (json.RawMessage, bool, error) {
	if key != tenantID { // the only branding document a tenant has is its own
		return nil, false, nil
	}
	query := `SELECT CAST(branding AS CHAR) FROM tenants WHERE id = ? AND deleted_at IS NULL`
	if forUpdate {
		query += " FOR UPDATE"
	}
	var js sql.NullString
	if err := sqlx.GetContext(ctx, q, &js, query, tenantID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("services.BrandingDocAdapter.LoadLive: %w", err)
	}
	doc, err := canonicalBranding([]byte(js.String))
	if err != nil {
		return nil, false, fmt.Errorf("services.BrandingDocAdapter.LoadLive: %w", err)
	}
	return doc, true, nil
}

// ListLiveKeys implements metaversion.DocAdapter.
func (*BrandingDocAdapter) ListLiveKeys(_ context.Context, _ sqlx.QueryerContext, tenantID string) ([]metaversion.DocRef, error) {
	return []metaversion.DocRef{{Key: tenantID, Label: brandingDocLabel}}, nil
}

// ApplyTx implements metaversion.DocAdapter. An empty document stores NULL (default branding).
func (*BrandingDocAdapter) ApplyTx(ctx context.Context, tx *sqlx.Tx, tenantID string, c metaversion.Change) error {
	if c.DocKey != tenantID {
		return fmt.Errorf("services.BrandingDocAdapter.ApplyTx: key %q is not tenant %q", c.DocKey, tenantID)
	}
	if c.Op != metaversion.OpUpsert {
		return fmt.Errorf("services.BrandingDocAdapter.ApplyTx: unsupported op %q (branding cannot be deleted)", c.Op)
	}
	var b Branding
	if err := json.Unmarshal(c.After, &b); err != nil {
		return fmt.Errorf("services.BrandingDocAdapter.ApplyTx: decode draft: %w", err)
	}
	raw, err := b.stored()
	if err != nil {
		return fmt.Errorf("services.BrandingDocAdapter.ApplyTx: %w", err)
	}
	if _, err := auth.SetBrandingExec(ctx, tx, tenantID, raw); err != nil {
		return fmt.Errorf("services.BrandingDocAdapter.ApplyTx: %w", err)
	}
	return nil
}

// ValidateFinal implements metaversion.DocAdapter: the document passes the same validator as a live
// save (a logo URL scheme allowlist is a stored-XSS guard; it must hold for what Publish writes).
func (*BrandingDocAdapter) ValidateFinal(_ context.Context, f metaversion.FinalState) []metaversion.Problem {
	var problems []metaversion.Problem
	for _, c := range f.Changes {
		if c.DocType != DocTypeBranding || c.Op != metaversion.OpUpsert {
			continue
		}
		var b Branding
		if err := json.Unmarshal(c.After, &b); err != nil {
			problems = append(problems, metaversion.Problem{DocType: DocTypeBranding, DocKey: c.DocKey, Code: "BRANDING_DRAFT_UNREADABLE", Message: "bản nháp thương hiệu không đọc được"})
			continue
		}
		if err := ValidateBranding(&b); err != nil {
			problems = append(problems, metaversion.Problem{DocType: DocTypeBranding, DocKey: c.DocKey, Code: "BRANDING_INVALID", Message: err.Error()})
		}
	}
	return problems
}

// Redact implements metaversion.DocAdapter: branding is shown on the public login page, no secret.
func (*BrandingDocAdapter) Redact(doc json.RawMessage) json.RawMessage { return doc }

// Entities implements metaversion.DocAdapter: branding has no engine cache to drop.
func (*BrandingDocAdapter) Entities(json.RawMessage) []string { return nil }
