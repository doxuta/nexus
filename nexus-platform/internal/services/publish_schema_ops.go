package services

// publish_schema_ops.go — specs/063-draft-publish S7-9: the deferred delete of a field or a table.
//
// A builder's delete on a delivered workspace is a draft (op=delete). Publish commits every document
// first (FR-065) and records, in the SAME tx, one version doc per delete with apply_status "pending" and
// a meta document {sweptAt, fieldDef | entityDef, relationships}; only AFTER the commit does it call the
// existing FieldService.DeleteField / EntityService.SoftDeleteEntity under WithPublishApply, so the
// delete runs through the one path that already owns the pivot, relationship, cache and audit cleanup.
// Publish itself never issues DDL (FR-068): no DropTable, no ALTER - those services are DML-only.
//
// sweptAt is chosen and written BEFORE the sweep and a retry re-reads it, so every row of one table
// delete carries the same deleted_at no matter how many attempts it took (FR-066).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/ddl"
	"nexus/platform/internal/metaversion"
)

// schemaFieldDeleter / schemaEntityDeleter are the two existing delete paths Publish calls back.
// *FieldService and *EntityService satisfy them.
type schemaFieldDeleter interface {
	DeleteField(ctx context.Context, tenantID string, in DeleteFieldInput) error
}

type schemaEntityDeleter interface {
	SoftDeleteEntity(ctx context.Context, tenantID, entityName string) error
}

// WithSchemaOps wires the delete paths of S7-9. Without them a publish carrying a delete leaves the
// op "failed" (and Thử lại works once they are wired). Chainable.
func (s *PublishService) WithSchemaOps(f schemaFieldDeleter, e schemaEntityDeleter) *PublishService {
	s.fieldDel, s.entityDel = f, e
	return s
}

// SchemaOpResult is one deferred delete as the publish / retry answers show it.
type SchemaOpResult struct {
	DocType     string `json:"docType"`
	DocKey      string `json:"docKey"`
	Label       string `json:"label"`
	ApplyStatus string `json:"applyStatus"` // applied | pending | failed
	Error       string `json:"error,omitempty"`
}

// SchemaRetryResult is the answer of POST /admin/publish/versions/:id/retry-schema.
type SchemaRetryResult struct {
	SchemaStatus string           `json:"schemaStatus"`
	SchemaOps    []SchemaOpResult `json:"schemaOps"`
}

// isSchemaDelete: a change that is the deferred delete of a field or a table.
func isSchemaDelete(c metaversion.Change) bool {
	return c.Op == metaversion.OpDelete && (c.DocType == DocTypeField || c.DocType == DocTypeEntity)
}

// rowsAsJSON reads rows as generic maps (bytes become strings) so a deleted definition can be kept
// whole in the version, whatever columns the table grows.
func rowsAsJSON(ctx context.Context, q sqlx.QueryerContext, query string, args ...any) ([]map[string]any, error) {
	rows, err := q.QueryxContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("services.rowsAsJSON: %w", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		m := map[string]any{}
		if err := rows.MapScan(m); err != nil {
			return nil, fmt.Errorf("services.rowsAsJSON: scan: %w", err)
		}
		for k, v := range m {
			if b, ok := v.([]byte); ok {
				m[k] = string(b)
			}
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("services.rowsAsJSON: %w", err)
	}
	return out, nil
}

// schemaDeleteMeta builds the meta of a delete version doc, read on the publish tx under the tenant
// lock: the sweep instant, the full definition being deleted and the relationship rows that go with it
// (what S7-10 needs to restore them).
func schemaDeleteMeta(ctx context.Context, q sqlx.QueryerContext, tenantID string, c metaversion.Change, sweptAt time.Time) (json.RawMessage, error) {
	m := map[string]any{"sweptAt": sweptAt.UTC().Format(time.RFC3339)}
	switch c.DocType {
	case DocTypeField:
		rows, err := rowsAsJSON(ctx, q, `SELECT * FROM sys_fields WHERE tenant_id = ? AND id = ?`, tenantID, c.DocKey)
		if err != nil {
			return nil, err
		}
		if len(rows) == 1 {
			m["fieldDef"] = rows[0]
			// The delete drops the field's sys_field_acl rows (default-allow table): keep them, or a restore
			// would show a hidden field to every role again.
			fname, _ := rows[0]["name"].(string)
			acl, err := rowsAsJSON(ctx, q, `SELECT role_id, entity_name, field_name, can_view, can_edit FROM sys_field_acl WHERE tenant_id = ? AND entity_name = ? AND field_name = ?`,
				tenantID, c.EntityName, fname)
			if err != nil {
				return nil, err
			}
			m["fieldAcl"] = acl
			if rows[0]["field_type"] == "relation" {
				name, _ := rows[0]["name"].(string)
				rels, err := rowsAsJSON(ctx, q, `SELECT * FROM sys_relationships WHERE tenant_id = ? AND child_entity_name = ? AND child_fk_column = ? AND deleted_at IS NULL`,
					tenantID, c.EntityName, name+"_id")
				if err != nil {
					return nil, err
				}
				m["relationships"] = rels
			}
		}
	case DocTypeEntity:
		rows, err := rowsAsJSON(ctx, q, `SELECT * FROM sys_entities WHERE tenant_id = ? AND id = ?`, tenantID, c.DocKey)
		if err != nil {
			return nil, err
		}
		if len(rows) == 1 {
			m["entityDef"] = rows[0]
			name, _ := rows[0]["name"].(string)
			acl, err := rowsAsJSON(ctx, q, `SELECT role_id, entity_name, field_name, can_view, can_edit FROM sys_field_acl WHERE tenant_id = ? AND entity_name = ?`, tenantID, name)
			if err != nil {
				return nil, err
			}
			m["fieldAcl"] = acl
			// A rollback restores "deleted_at = sweptAt": a record the staff binned within the same second would be
			// brought back with the rest. Move the instant forward until no record of this tenant carries it.
			at, err := freeSweepInstant(ctx, q, tenantID, name, sweptAt)
			if err != nil {
				return nil, err
			}
			m["sweptAt"] = at.UTC().Format(time.RFC3339)
			rels, err := rowsAsJSON(ctx, q, `SELECT * FROM sys_relationships WHERE tenant_id = ? AND (parent_entity_name = ? OR child_entity_name = ?) AND deleted_at IS NULL`,
				tenantID, name, name)
			if err != nil {
				return nil, err
			}
			m["relationships"] = rels
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("services.schemaDeleteMeta: %w", err)
	}
	return b, nil
}

// freeSweepInstant returns the first second at or after `at` at which no record of the tenant sits in the
// bin of the table (specs/063 FR-066). The table name is sanitized before it joins the statement, the
// tenant and the instant are bound. A bin row that appears between this read and the sweep (same second)
// is the residual race; the tenant lock does not cover record writes.
func freeSweepInstant(ctx context.Context, q sqlx.QueryerContext, tenantID, entityName string, at time.Time) (time.Time, error) {
	table, err := ddl.SanitizeIdentifier(entityName)
	if err != nil {
		return at, fmt.Errorf("services.freeSweepInstant: %w", err)
	}
	// One read of the distinct bin seconds from `at` on, then the first gap in memory (no cap on busy seconds).
	const maxBusy = 100000
	var busy []time.Time
	if err := sqlx.SelectContext(ctx, q, &busy, fmt.Sprintf("SELECT DISTINCT `deleted_at` FROM `obj_%s` WHERE `tenant_id` = ? AND `deleted_at` >= ? ORDER BY `deleted_at` LIMIT %d", table, maxBusy), tenantID, at.UTC()); err != nil {
		return at, fmt.Errorf("services.freeSweepInstant: %w", err)
	}
	if len(busy) >= maxBusy {
		return at, fmt.Errorf("services.freeSweepInstant: %d consecutive bin seconds in %s, no free instant", maxBusy, table)
	}
	taken := make(map[int64]bool, len(busy))
	for _, b := range busy {
		taken[b.Unix()] = true
	}
	for taken[at.Unix()] {
		at = at.Add(time.Second)
	}
	return at, nil
}

// truncateRunes cuts s to n runes (apply_error is VARCHAR(500)).
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// runOneSchemaOp runs a single deferred delete and returns its outcome and the meta to store. An
// object that is already gone (a previous attempt got as far as the tombstone, or another table of the
// same name lives now) counts as applied: that is what makes a retry safe to repeat.
func (s *PublishService) runOneSchemaOp(ctx context.Context, tenantID string, d metaversion.SchemaDoc) (status, errText string, meta json.RawMessage) {
	m := map[string]any{}
	if len(d.Meta) > 0 {
		if err := json.Unmarshal(d.Meta, &m); err != nil {
			return metaversion.StatusFailed, "meta của thao tác xoá không đọc được", nil
		}
	}
	rawAt, _ := m["sweptAt"].(string)
	sweptAt, err := time.Parse(time.RFC3339, rawAt)
	if err != nil {
		return metaversion.StatusFailed, "thiếu mốc quét đã ghi cho thao tác xoá", nil
	}
	ad, err := s.reg.Must(d.DocType)
	if err != nil {
		return metaversion.StatusFailed, truncateRunes(err.Error(), 500), nil
	}
	live, exists, err := ad.LoadLive(ctx, s.db, tenantID, d.DocKey, false)
	if err != nil {
		return metaversion.StatusFailed, truncateRunes(err.Error(), 500), nil
	}
	if !exists {
		return metaversion.StatusApplied, "", nil
	}
	actx := metaversion.WithSweepAt(ctx, sweptAt)
	actx, swept := metaversion.WithSweepCount(actx)
	switch d.DocType {
	case DocTypeField:
		if s.fieldDel == nil {
			return metaversion.StatusFailed, "chưa nối đường xoá trường", nil
		}
		err = s.fieldDel.DeleteField(actx, tenantID, DeleteFieldInput{FieldID: d.DocKey})
	case DocTypeEntity:
		if s.entityDel == nil {
			return metaversion.StatusFailed, "chưa nối đường xoá bảng", nil
		}
		var doc entityDoc
		if jerr := json.Unmarshal(live, &doc); jerr != nil || doc.Name == "" {
			return metaversion.StatusFailed, "không đọc được tên bảng cần xoá", nil
		}
		err = s.entityDel.SoftDeleteEntity(actx, tenantID, doc.Name)
	}
	// The sweep commits BEFORE the tombstone (SoftDeleteEntity), so a table delete that fails part-way has
	// already moved rows. Record the count on every outcome: the retry sweeps `deleted_at IS NULL` = 0 rows
	// and would otherwise overwrite the real number with 0 (FR-066 "ghi số hồ sơ vào phiên bản").
	if d.DocType == DocTypeEntity {
		prev, _ := m["sweptCount"].(float64) // a retry adds to what an earlier attempt already moved
		m["sweptCount"] = int64(prev) + swept.N.Load()
		b, merr := json.Marshal(m)
		if merr != nil {
			return metaversion.StatusFailed, "không ghi được số hồ sơ đã xoá", nil
		}
		meta = b
	}
	var ve *ValidationError
	switch {
	case err == nil:
	case errors.As(err, &ve) && (ve.Code == "FIELD_NOT_FOUND" || ve.Code == "ENTITY_NOT_FOUND"):
		return metaversion.StatusApplied, "", meta
	case errors.As(err, &ve):
		return metaversion.StatusFailed, truncateRunes(ve.Msg, 500), meta
	default:
		s.logger.Error("services.PublishService.runOneSchemaOp", zap.String("tenant_id", tenantID),
			zap.String("doc_type", d.DocType), zap.String("doc_key", d.DocKey), zap.Error(err))
		return metaversion.StatusFailed, truncateRunes(err.Error(), 500), meta
	}
	return metaversion.StatusApplied, "", meta
}

// runSchemaOps runs every not-yet-applied delete of the version, records each outcome and moves the
// version from "pending" to done / failed. The caller owns the "pending" claim (Publish just inserted
// the version so; RetrySchema took it with ClaimSchemaRetry).
func (s *PublishService) runSchemaOps(ctx context.Context, caller metaversion.Caller, tenantID, versionID string) ([]SchemaOpResult, string, error) {
	docs, err := s.versions.ListSchemaDocs(ctx, tenantID, versionID)
	if err != nil {
		return nil, "", err
	}
	// The publish already committed: a client that hung up must not abort the deletes halfway.
	ctx = metaversion.WithPublishApply(audit.WithActor(context.WithoutCancel(ctx), caller.UserID))
	results := make([]SchemaOpResult, 0, len(docs))
	failed := false
	for _, d := range docs {
		r := SchemaOpResult{DocType: d.DocType, DocKey: d.DocKey, Label: d.Label, ApplyStatus: d.ApplyStatus, Error: d.ApplyError.String}
		if d.ApplyStatus != metaversion.StatusApplied {
			status, errText, meta := s.runOneSchemaOp(ctx, tenantID, d)
			r.ApplyStatus, r.Error = status, errText
			if err := s.versions.SetDocApply(ctx, tenantID, d.ID, status, errText, meta); err != nil {
				// The op ran but its record did not land: leave the version "failed" - a retry finds the
				// object gone and records it as applied.
				s.logger.Error("services.PublishService.runSchemaOps: record outcome", zap.String("tenant_id", tenantID),
					zap.String("version_id", versionID), zap.String("doc_key", d.DocKey), zap.Error(err))
				failed = true
			}
		}
		if r.ApplyStatus != metaversion.StatusApplied {
			failed = true
		}
		results = append(results, r)
	}
	final := metaversion.StatusDone
	if failed {
		final = metaversion.StatusFailed
	}
	if _, err := s.versions.SetSchemaStatus(ctx, tenantID, versionID, metaversion.StatusPending, final); err != nil {
		return results, final, err
	}
	return results, final, nil
}

// schemaRetryRef reads the newest version with unfinished deletes and the ops still not applied; nil
// when there is none.
func (s *PublishService) schemaRetryRef(ctx context.Context, tenantID string) (*SchemaRetryRef, error) {
	v, err := s.versions.LatestRetryableSchemaVersion(ctx, tenantID)
	if err != nil || v == nil || v.VersionNo == nil {
		return nil, err
	}
	docs, err := s.versions.ListSchemaDocs(ctx, tenantID, v.ID)
	if err != nil {
		return nil, err
	}
	ref := &SchemaRetryRef{VersionID: v.ID, VersionNo: *v.VersionNo, Ops: []SchemaOpResult{}}
	for _, d := range docs {
		if d.ApplyStatus != metaversion.StatusApplied {
			ref.Ops = append(ref.Ops, SchemaOpResult{DocType: d.DocType, DocKey: d.DocKey, Label: d.Label, ApplyStatus: d.ApplyStatus, Error: d.ApplyError.String})
		}
	}
	return ref, nil
}

// RetrySchema re-runs the deferred deletes of a version whose schema_status is "failed" (or whose
// "pending" has been abandoned), reusing the sweep instant recorded at publish (FR-065, FR-066).
// A version of another tenant is the same VERSION_NOT_FOUND as an unknown one (FR-081).
func (s *PublishService) RetrySchema(ctx context.Context, tenantID, versionID string) (*SchemaRetryResult, error) {
	caller, err := s.builder(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if _, err := s.versions.GetVersion(ctx, tenantID, versionID); errors.Is(err, metaversion.ErrVersionNotFound) {
		return nil, invalid("VERSION_NOT_FOUND", "Không tìm thấy phiên bản này")
	} else if err != nil {
		return nil, fmt.Errorf("services.PublishService.RetrySchema: %w", err)
	}
	claimed, err := s.versions.ClaimSchemaRetry(ctx, tenantID, versionID)
	if err != nil {
		return nil, fmt.Errorf("services.PublishService.RetrySchema: %w", err)
	}
	if !claimed {
		return nil, invalid("SCHEMA_RETRY_NOT_NEEDED", "Phiên bản này không có thao tác xoá nào cần thử lại")
	}
	ops, status, err := s.runSchemaOps(ctx, caller, tenantID, versionID)
	if err != nil {
		return nil, fmt.Errorf("services.PublishService.RetrySchema: %w", err)
	}
	s.logger.Info("services.PublishService.RetrySchema: retried", zap.String("tenant_id", tenantID),
		zap.String("user_id", caller.UserID), zap.String("version_id", versionID), zap.String("schema_status", status))
	return &SchemaRetryResult{SchemaStatus: status, SchemaOps: ops}, nil
}

// likeEscape escapes the LIKE wildcards of a value used inside a pattern (ESCAPE '\').
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

const maxSchemaWarnings = 20

// schemaWarnings lists what a pending delete will leave dangling: reports that use the field (or read
// the table). A warning, never a block (plan §6 step 3b): the builder decides. ponytail: a LIKE over the
// report config text - builder-only path, a handful of reports per table; a structured field walk
// belongs with the report adapter if it ever gets slow.
func (s *PublishService) schemaWarnings(ctx context.Context, tenantID string, d metaversion.DraftRow) ([]string, error) {
	if d.Op != metaversion.OpDelete {
		return nil, nil
	}
	var names []string
	switch d.DocType {
	case DocTypeField:
		var f struct {
			Name   string `db:"fname"`
			Entity string `db:"ename"`
		}
		if err := s.db.GetContext(ctx, &f, `SELECT f.name AS fname, e.name AS ename FROM sys_fields f
			JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id
			WHERE f.tenant_id = ? AND f.id = ? AND f.deleted_at IS NULL`, tenantID, d.DocKey); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil // the field is gone already: the change list shows the conflict, not a warning
			}
			return nil, fmt.Errorf("services.PublishService.schemaWarnings: resolve field: %w", err)
		}
		if err := s.db.SelectContext(ctx, &names, `SELECT name FROM sys_reports WHERE tenant_id = ? AND entity_name = ? AND deleted_at IS NULL
			AND CAST(config AS CHAR) LIKE ? ESCAPE '\\' ORDER BY name LIMIT ?`,
			tenantID, f.Entity, `%"`+likeEscape(f.Name)+`"%`, maxSchemaWarnings); err != nil {
			return nil, fmt.Errorf("services.PublishService.schemaWarnings: %w", err)
		}
		return reportWarnings(names, "trường", f.Name), nil
	case DocTypeEntity:
		if d.EntityName == nil {
			return nil, nil
		}
		if err := s.db.SelectContext(ctx, &names, `SELECT name FROM sys_reports WHERE tenant_id = ? AND entity_name = ? AND deleted_at IS NULL
			ORDER BY name LIMIT ?`, tenantID, *d.EntityName, maxSchemaWarnings); err != nil {
			return nil, fmt.Errorf("services.PublishService.schemaWarnings: %w", err)
		}
		return reportWarnings(names, "bảng", *d.EntityName), nil
	}
	return nil, nil
}

func reportWarnings(reports []string, kind, name string) []string {
	out := make([]string, len(reports))
	for i, r := range reports {
		out[i] = fmt.Sprintf("Báo cáo %q dùng %s %q", r, kind, name)
	}
	return out
}
