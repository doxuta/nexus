//go:build integration

// W9b — HTTP-level proof of the recycle-bin RESTORE COLLISION trap (playbook §5.3): a soft-deleted
// row frees its unique slot (the generated column is CASE WHEN deleted_at IS NULL … ELSE NULL), a
// NEW live row takes the same value, and restoring the old row recomputes the column → MySQL 1062.
// This asserts the FULL service→handler mapping to 409 RECORD_DUPLICATE + details.field — the exact
// "map to 409 with details.field" the task calls out. (The ACL/ownership/lifecycle/404 cases live in
// w9b_recycle_views_integration_test.go; the dynrepo-level collision in
// ../dynrepo/w9b_recycle_integration_test.go — this file is the end-to-end HTTP contract for the
// duplicate mapping only, so coverage is not duplicated.)
//
// Run: go test -tags=integration ./internal/handlers/ -run TestW9b_RestoreUniqueCollision
package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

// w9bCreateRecord POSTs a record as the shared admin token and returns its id.
func w9bCreateRecord(t *testing.T, r *gin.Engine, entity string, body map[string]any) string {
	t.Helper()
	w := do(t, r, http.MethodPost, "/api/v1/"+entity, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create record: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.Data.ID == "" {
		t.Fatal("created record id empty")
	}
	return created.Data.ID
}

// TestW9b_RestoreUniqueCollision is the exact trap: a soft-deleted row frees its unique slot, a NEW
// live row takes the same value, and the restore recomputes the column back → MySQL 1062 → must
// surface as 409 RECORD_DUPLICATE with details.field, NOT an opaque 500.
func TestW9b_RestoreUniqueCollision(t *testing.T) {
	r, sx := newFullRouter(t)
	const entity = "it_w9b_collide"
	cleanupNamed(sx, entity)
	t.Cleanup(func() { cleanupNamed(sx, entity) })

	createBody := map[string]any{
		"name": entity, "label": "Collide",
		"fields": []map[string]any{
			{"name": "code", "label": "Code", "fieldType": "text", "isUnique": true},
			{"name": "title", "label": "Title", "fieldType": "text"},
		},
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", createBody); w.Code != http.StatusCreated {
		t.Fatalf("create entity: %d %s", w.Code, w.Body.String())
	}

	// A: live with code=DUP, then soft-deleted (frees the unique slot).
	aID := w9bCreateRecord(t, r, entity, map[string]any{"code": "DUP", "title": "A"})
	if w := do(t, r, http.MethodDelete, "/api/v1/"+entity+"/"+aID, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete A: %d %s", w.Code, w.Body.String())
	}
	// B: a NEW live row grabs code=DUP — allowed, because A's gen-column is NULL while deleted.
	if w := do(t, r, http.MethodPost, "/api/v1/"+entity, map[string]any{"code": "DUP", "title": "B"}); w.Code != http.StatusCreated {
		t.Fatalf("create B with reused code should succeed while A is soft-deleted: %d %s", w.Code, w.Body.String())
	}
	// Restore A → collides with live B → 409 RECORD_DUPLICATE + details.field=code.
	w := do(t, r, http.MethodPost, "/api/v1/"+entity+"/"+aID+"/restore", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("restore into collision = %d; want 409\n%s", w.Code, w.Body.String())
	}
	var errResp struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.Code != "RECORD_DUPLICATE" {
		t.Fatalf("restore collision code = %q; want RECORD_DUPLICATE", errResp.Code)
	}
	if errResp.Details["field"] != "code" {
		t.Fatalf("restore collision details.field = %v; want code", errResp.Details["field"])
	}
}
