//go:build integration

package handlers

// CURSOR-BOOTSTRAP (queue #42, specs/039-cursor-bootstrap) against real MySQL through the
// production route registration: a keyset walk that STARTS with ?paginate=cursor (no after token)
// visits every pre-existing row exactly once while new rows are being created between pages.
// created_at is second-precision TIMESTAMP, so rows inserted back to back share a created_at and
// the walk crosses tie blocks — the (col IS NULL, col, id) keyset has to absorb them via `id`.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

const cursorBootEntity = "it_cursor_boot"

type cursorPageBody struct {
	Data []struct {
		ID      string `json:"id"`
		OwnerID string `json:"ownerId"`
	} `json:"data"`
	Pagination map[string]any `json:"pagination"`
}

// reassignedOwner is written straight into owner_id for a few seeded rows so the walk also covers
// the COALESCE(owner_id, created_by) branch, not only the created_by fallback.
const reassignedOwner = "0000000a-0000-0000-0000-0000000000aa"

func TestList_PaginateCursor_WalkFromFirstPage_Integration(t *testing.T) {
	r, sx := newFullRouter(t)
	cleanupNamed(sx, cursorBootEntity)
	t.Cleanup(func() { cleanupNamed(sx, cursorBootEntity) })

	if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", map[string]any{
		"name": cursorBootEntity, "label": "Cursor boot",
		"fields": []map[string]any{{"name": "title", "label": "Title", "fieldType": "text"}},
	}); w.Code != http.StatusCreated {
		t.Fatalf("create entity: %d %s", w.Code, w.Body.String())
	}

	create := func(title string) string {
		t.Helper()
		w := do(t, r, http.MethodPost, "/api/v1/"+cursorBootEntity, map[string]any{"title": title})
		if w.Code != http.StatusCreated {
			t.Fatalf("create record: %d %s", w.Code, w.Body.String())
		}
		var out struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Data.ID == "" {
			t.Fatalf("decode create: %v (%s)", err, w.Body.String())
		}
		return out.Data.ID
	}

	const seeded = 90
	pre := make(map[string]bool, seeded)
	reassigned := map[string]bool{}
	for i := 0; i < seeded; i++ {
		id := create(fmt.Sprintf("seed-%03d", i))
		pre[id] = true
		if i%20 == 0 {
			reassigned[id] = true
			if _, err := sx.Exec("UPDATE `obj_"+cursorBootEntity+"` SET `owner_id` = ? WHERE `tenant_id` = ? AND `id` = ?",
				reassignedOwner, testTenant, id); err != nil {
				t.Fatalf("reassign owner: %v", err)
			}
		}
	}

	for _, order := range []string{"desc", "asc"} {
		t.Run(order, func(t *testing.T) {
			seen := map[string]int{}
			owners := map[string]string{} // id -> ownerId as the cursor walk returned it
			base := "/api/v1/" + cursorBootEntity + "?paginate=cursor&sort=created_at&order=" + order + "&limit=25"
			path := base
			pages := 0
			for {
				w := do(t, r, http.MethodGet, path, nil)
				if w.Code != http.StatusOK {
					t.Fatalf("page %d: %d %s", pages+1, w.Code, w.Body.String())
				}
				var body cursorPageBody
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode page %d: %v", pages+1, err)
				}
				pages++
				if _, hasTotal := body.Pagination["total"]; hasTotal {
					t.Fatalf("page %d carries total — keyset pages must not COUNT(*): %v", pages, body.Pagination)
				}
				for _, rec := range body.Data {
					seen[rec.ID]++
					owners[rec.ID] = rec.OwnerID
				}
				next, _ := body.Pagination["nextCursor"].(string)
				if pages == 1 && next == "" {
					t.Fatalf("FIRST page has no nextCursor (%v) — the chain cannot start", body.Pagination)
				}
				if next == "" {
					break
				}
				if pages > 40 {
					t.Fatalf("walk did not terminate after %d pages", pages)
				}
				// Someone keeps typing while the sync job pages through.
				for i := 0; i < 3; i++ {
					create(fmt.Sprintf("live-%s-%d-%d", order, pages, i))
				}
				path = base + "&after=" + url.QueryEscape(next)
			}
			for id, n := range seen {
				if n != 1 {
					t.Errorf("id %s returned %d times; want exactly once", id, n)
				}
			}
			for id := range pre {
				if seen[id] != 1 {
					t.Errorf("pre-existing id %s seen %d times; want exactly once", id, seen[id])
				}
			}
			// A cursor row must have the SAME shape as an offset row: a sync job upserting by owner
			// (and the list grid's owner column) reads ownerId off every row.
			offsetOwners := offsetOwnerIDs(t, r, cursorBootEntity)
			for id, got := range owners {
				want, ok := offsetOwners[id]
				if !ok {
					t.Fatalf("id %s from the cursor walk is missing from the offset walk", id)
				}
				if got == "" || got != want {
					t.Errorf("id %s: cursor ownerId = %q; offset ownerId = %q", id, got, want)
				}
			}
			for id := range reassigned {
				if owners[id] != reassignedOwner {
					t.Errorf("reassigned id %s: cursor ownerId = %q; want %q", id, owners[id], reassignedOwner)
				}
			}
		})
	}
}

// offsetOwnerIDs walks the offset list (the reference row shape) and returns id -> ownerId.
func offsetOwnerIDs(t *testing.T, r *gin.Engine, entity string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for page := 1; ; page++ {
		w := do(t, r, http.MethodGet, fmt.Sprintf("/api/v1/%s?page=%d&limit=100", entity, page), nil)
		if w.Code != http.StatusOK {
			t.Fatalf("offset page %d: %d %s", page, w.Code, w.Body.String())
		}
		var body cursorPageBody
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode offset page %d: %v", page, err)
		}
		for _, rec := range body.Data {
			out[rec.ID] = rec.OwnerID
		}
		if len(body.Data) < 100 {
			return out
		}
	}
}

// A cursor is bound to the sort column and direction it was minted under. Replaying a
// server-emitted nextCursor under another sort/order used to be 200 with a wrong page — or, when
// MySQL coerced the boundary value to the other column's type (a uuid/email against a TIMESTAMP,
// warning 1292), 200 `{"data":[],"nextCursor":null}`: a sync job reads that as "done" and stops
// early without an error. It must be a named 400 INVALID_CURSOR instead, while a chain that keeps
// its sort/order (including the no-sort default) still pages on.
func TestList_PaginateCursor_BoundToSortAndOrder_Integration(t *testing.T) {
	const entity = "it_cursor_bind"
	r, sx := newFullRouter(t)
	cleanupNamed(sx, entity)
	t.Cleanup(func() { cleanupNamed(sx, entity) })

	if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", map[string]any{
		"name": entity, "label": "Cursor bind",
		"fields": []map[string]any{{"name": "title", "label": "Title", "fieldType": "text"}},
	}); w.Code != http.StatusCreated {
		t.Fatalf("create entity: %d %s", w.Code, w.Body.String())
	}
	for i := 0; i < 12; i++ {
		if w := do(t, r, http.MethodPost, "/api/v1/"+entity, map[string]any{"title": fmt.Sprintf("r-%02d", i)}); w.Code != http.StatusCreated {
			t.Fatalf("create record: %d %s", w.Code, w.Body.String())
		}
	}

	get := func(query string) (int, cursorPageBody, string) {
		t.Helper()
		w := do(t, r, http.MethodGet, "/api/v1/"+entity+"?"+query, nil)
		var body cursorPageBody
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body, w.Body.String()
	}
	firstCursor := func(query string) string {
		t.Helper()
		code, body, raw := get(query)
		next, _ := body.Pagination["nextCursor"].(string)
		if code != http.StatusOK || next == "" {
			t.Fatalf("first page %q: %d %s", query, code, raw)
		}
		return next
	}

	byID := firstCursor("paginate=cursor&sort=id&order=asc&limit=5")
	createdDesc := firstCursor("paginate=cursor&sort=created_at&order=desc&limit=5")
	noSort := firstCursor("paginate=cursor&limit=5")

	rejected := []struct{ name, query string }{
		{"id asc cursor replayed as created_at desc", "sort=created_at&order=desc&limit=5&after=" + url.QueryEscape(byID)},
		{"desc cursor replayed as asc", "sort=created_at&order=asc&limit=5&after=" + url.QueryEscape(createdDesc)},
		{"created_at cursor replayed as id", "sort=id&order=desc&limit=5&after=" + url.QueryEscape(createdDesc)},
		{"no-sort cursor replayed with order=asc", "order=asc&limit=5&after=" + url.QueryEscape(noSort)},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			code, _, raw := get(tc.query)
			if code != http.StatusBadRequest || !strings.Contains(raw, `"INVALID_CURSOR"`) {
				t.Fatalf("status = %d body = %s; want 400 INVALID_CURSOR", code, raw)
			}
		})
	}

	kept := []struct{ name, query string }{
		{"id asc chain", "paginate=cursor&sort=id&order=asc&limit=5&after=" + url.QueryEscape(byID)},
		{"created_at desc chain", "sort=created_at&order=desc&limit=5&after=" + url.QueryEscape(createdDesc)},
		{"no-sort chain", "paginate=cursor&limit=5&after=" + url.QueryEscape(noSort)},
		{"no-sort chain spelled out as the default", "sort=created_at&order=desc&limit=5&after=" + url.QueryEscape(noSort)},
	}
	for _, tc := range kept {
		t.Run(tc.name, func(t *testing.T) {
			code, body, raw := get(tc.query)
			if code != http.StatusOK || len(body.Data) != 5 {
				t.Fatalf("status = %d rows = %d body = %s; want 200 with the next 5 rows", code, len(body.Data), raw)
			}
		})
	}
}
