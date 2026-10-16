package metaversion

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jmoiron/sqlx"
)

// Operation of a change on one document.
const (
	OpUpsert = "upsert"
	OpDelete = "delete"
)

// DocRef names one live document.
type DocRef struct {
	Key        string
	Label      string
	EntityName string
}

// Change is one document of a publish: the draft's op and body (After, nil for a delete), the live
// state read under row lock (Before, nil when the document is absent or soft-deleted) and the
// draft's presentation fields.
type Change struct {
	DocType    string
	DocKey     string
	Op         string
	Before     json.RawMessage
	After      json.RawMessage
	Label      string
	EntityName string
	UserID     string
	// DraftUpdatedAt is when the draft was last saved. A layout publishes it as its updated_at so the
	// "Mới thêm" cutoff is the builder's save, not the publish (specs/063 S7-3).
	DraftUpdatedAt time.Time
	// DraftID is the id of the draft row. A layout that did not exist before publishes under it, so the
	// id the builder's screen held while it was a draft keeps working after Publish (FR-007).
	DraftID string
	// Restore is the Meta recorded by the published delete of a field / table (specs/063 S7-10). A
	// rollback sets it on the upsert that brings a deleted field / table back; the adapter then
	// un-deletes the row, its relationships and its records instead of updating a live row.
	Restore json.RawMessage
}

// FinalState is what ValidateFinal checks: ALL the changes about to be applied (every type; an adapter picks its own by DocType), on the open tx (so the
// check reads the live state AFTER the tenant lock, where a customer-admin write cannot slip in).
type FinalState struct {
	TenantID string
	Q        sqlx.QueryerContext
	Changes  []Change
}

// Problem is one reason a publish's final state is not consistent (PUBLISH_INVALID).
type Problem struct {
	DocType string `json:"docType"`
	DocKey  string `json:"docKey"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// DocAdapter binds one document type to its live table. Everything that varies per type lives
// here, so publish / baseline / (later) rollback are written once.
type DocAdapter interface {
	Type() string
	// LoadLive reads the canonical form of one live document. ok=false: absent or soft-deleted.
	// forUpdate takes a row lock (the publish conflict check).
	LoadLive(ctx context.Context, q sqlx.QueryerContext, tenantID, key string, forUpdate bool) (doc json.RawMessage, ok bool, err error)
	// ListLiveKeys lists every live document (baseline snapshot).
	ListLiveKeys(ctx context.Context, q sqlx.QueryerContext, tenantID string) ([]DocRef, error)
	// ApplyTx writes one change on the publish tx: upsert (insert, update, or restore a soft-deleted
	// row of the same id) or delete.
	ApplyTx(ctx context.Context, tx *sqlx.Tx, tenantID string, c Change) error
	// ValidateFinal checks cross-document consistency of the final state.
	ValidateFinal(ctx context.Context, f FinalState) []Problem
	// Redact masks secrets of a document before it leaves through the changes / versions APIs.
	Redact(doc json.RawMessage) json.RawMessage
	// Entities lists the entity names a document touches, to drop the engine caches after commit.
	Entities(doc json.RawMessage) []string
}

// Deriver is implemented by an adapter whose documents are changed BY OTHER documents' changes in the
// same publish (an app's menu loses the dashboard a publish deletes). Publish calls it with the whole
// change list after the drafts are loaded and before ValidateFinal; it returns the list to apply, with
// the affected documents rewritten or added as ordinary Changes - so version_docs records them.
type Deriver interface {
	Derive(ctx context.Context, q sqlx.QueryerContext, tenantID string, changes []Change) ([]Change, error)
}

// Registry is the whitelist of document types: the only values that ever reach sys_meta_drafts.doc_type.
type Registry struct {
	byType map[string]DocAdapter
	order  []string
}

// NewRegistry builds a registry; types keep the order given (the apply order of a publish).
func NewRegistry(adapters ...DocAdapter) *Registry {
	r := &Registry{byType: make(map[string]DocAdapter, len(adapters))}
	for _, a := range adapters {
		r.byType[a.Type()] = a
		r.order = append(r.order, a.Type())
	}
	return r
}

// Get returns the adapter of a type.
func (r *Registry) Get(docType string) (DocAdapter, bool) {
	a, ok := r.byType[docType]
	return a, ok
}

// Must returns the adapter or an error naming the unknown type — checked BEFORE any SQL.
func (r *Registry) Must(docType string) (DocAdapter, error) {
	a, ok := r.byType[docType]
	if !ok {
		return nil, fmt.Errorf("metaversion.Registry: unknown doc type %q", docType)
	}
	return a, nil
}

// Types lists the registered types in apply order.
func (r *Registry) Types() []string { return append([]string(nil), r.order...) }

// Rank is a type's position in the apply order (unknown types sort last).
func (r *Registry) Rank(docType string) int {
	for i, t := range r.order {
		if t == docType {
			return i
		}
	}
	return len(r.order)
}

// SortChanges orders changes for apply: by type order, upserts before deletes, then by key.
func (r *Registry) SortChanges(cs []Change) {
	sort.SliceStable(cs, func(i, j int) bool {
		ri, rj := r.Rank(cs[i].DocType), r.Rank(cs[j].DocType)
		if ri != rj {
			return ri < rj
		}
		if (cs[i].Op == OpDelete) != (cs[j].Op == OpDelete) {
			return cs[j].Op == OpDelete
		}
		return cs[i].DocKey < cs[j].DocKey
	})
}
