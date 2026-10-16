package metaversion

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// HistoryDoc is one version doc as the rollback planner reads it: which document, what happened to it,
// in which version (seq, kind) and who did it. AfterJSON is filled only by TargetDocs.
type HistoryDoc struct {
	DocType    string
	DocKey     string
	Label      string
	EntityName string
	Op         string
	Seq        uint
	Kind       string
	AfterJSON  json.RawMessage
	CreatedBy  string
	ByLabel    string
	At         time.Time
}

type historyRow struct {
	DocType    string         `db:"doc_type"`
	DocKey     string         `db:"doc_key"`
	Label      string         `db:"label"`
	EntityName sql.NullString `db:"entity_name"`
	Op         string         `db:"op"`
	Seq        uint           `db:"seq"`
	Kind       string         `db:"kind"`
	AfterJSON  []byte         `db:"after_json"`
	CreatedBy  sql.NullString `db:"created_by"`
	ByLabel    string         `db:"created_by_label"`
	At         time.Time      `db:"created_at"`
}

// TargetDocs returns, for every (type, key) of the given types, the newest version doc with seq <= upTo
// (snapshot, create, update, delete, direct entries included) with its after body: the state a
// rollback to that point aims at. Every statement pins tenant_id BEFORE the seq is used (FR-081).
func (r *VersionRepo) TargetDocs(ctx context.Context, q sqlx.QueryerContext, tenantID string, upTo uint, types []string) ([]HistoryDoc, error) {
	if len(types) == 0 {
		return nil, nil
	}
	query, args, err := sqlx.In(`SELECT d.doc_type, d.doc_key, d.label, d.entity_name, d.op, d.seq, v.kind, d.after_json, d.created_by, v.created_by_label, v.created_at
		FROM sys_meta_version_docs d
		JOIN sys_meta_versions v ON v.id = d.version_id AND v.tenant_id = d.tenant_id
		JOIN (SELECT doc_type, doc_key, MAX(seq) AS mseq FROM sys_meta_version_docs
			WHERE tenant_id = ? AND seq <= ? AND doc_type IN (?) AND deleted_at IS NULL GROUP BY doc_type, doc_key) m
			ON m.doc_type = d.doc_type AND m.doc_key = d.doc_key AND m.mseq = d.seq
		WHERE d.tenant_id = ? AND d.deleted_at IS NULL ORDER BY d.doc_type, d.doc_key, (d.op <> 'snapshot')`, tenantID, upTo, types, tenantID) // snapshot first: a same-seq update/delete is the newer state
	if err != nil {
		return nil, fmt.Errorf("metaversion.VersionRepo.TargetDocs: %w", err)
	}
	return selectHistory(ctx, q, query, args, "TargetDocs")
}

// DocsAfter lists the version docs with seq > after of the given types, oldest first, WITHOUT bodies
// and without snapshot rows (a snapshot of a type added later is the state before a publish, not a change).
func (r *VersionRepo) DocsAfter(ctx context.Context, q sqlx.QueryerContext, tenantID string, after uint, types []string) ([]HistoryDoc, error) {
	if len(types) == 0 {
		return nil, nil
	}
	query, args, err := sqlx.In(`SELECT d.doc_type, d.doc_key, d.label, d.entity_name, d.op, d.seq, v.kind, NULL AS after_json, d.created_by, v.created_by_label, v.created_at
		FROM sys_meta_version_docs d
		JOIN sys_meta_versions v ON v.id = d.version_id AND v.tenant_id = d.tenant_id
		WHERE d.tenant_id = ? AND d.seq > ? AND d.doc_type IN (?) AND d.op <> 'snapshot' AND d.deleted_at IS NULL
		ORDER BY d.seq, d.doc_type, d.doc_key`, tenantID, after, types)
	if err != nil {
		return nil, fmt.Errorf("metaversion.VersionRepo.DocsAfter: %w", err)
	}
	return selectHistory(ctx, q, query, args, "DocsAfter")
}

// LatestDeleteMeta returns the Meta ({sweptAt, fieldDef | entityDef, relationships, sweptCount}) the
// newest published delete of a field / table recorded (specs/063 S7-10): everything a rollback needs
// to bring it back. found=false when no delete with a Meta was ever recorded for the key (a document
// that went away outside the version history). Tenant-pinned before the key is used (FR-081).
func (r *VersionRepo) LatestDeleteMeta(ctx context.Context, q sqlx.QueryerContext, tenantID, docType, docKey string) (json.RawMessage, bool, error) {
	var meta []byte
	err := sqlx.GetContext(ctx, q, &meta, `SELECT CAST(meta AS CHAR) FROM sys_meta_version_docs
		WHERE tenant_id = ? AND doc_type = ? AND doc_key = ? AND op = 'delete' AND meta IS NOT NULL AND deleted_at IS NULL
		ORDER BY seq DESC LIMIT 1`, tenantID, docType, docKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("metaversion.VersionRepo.LatestDeleteMeta: %w", err)
	}
	return meta, true, nil
}

func selectHistory(ctx context.Context, q sqlx.QueryerContext, query string, args []any, op string) ([]HistoryDoc, error) {
	var rows []historyRow
	if err := sqlx.SelectContext(ctx, q, &rows, query, args...); err != nil {
		return nil, fmt.Errorf("metaversion.VersionRepo.%s: %w", op, err)
	}
	out := make([]HistoryDoc, len(rows))
	for i, x := range rows {
		out[i] = HistoryDoc{DocType: x.DocType, DocKey: x.DocKey, Label: x.Label, EntityName: x.EntityName.String, Op: x.Op, Seq: x.Seq,
			Kind: x.Kind, AfterJSON: x.AfterJSON, CreatedBy: x.CreatedBy.String, ByLabel: x.ByLabel, At: x.At}
	}
	return out, nil
}
