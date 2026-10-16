package services

// record_massop_test.go — ENG-MASSOP unit suite: UpdateBulk / DeleteBulk loop the single-record
// Update/Delete path per id with partial semantics (a failing row never aborts siblings), results
// in submitted order after dedupe, tenant pinned on every store call, per-row audit emission,
// request-level guards 422, infra errors bubble as top-level.

import (
	"context"
	"errors"
	"os"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// massOpStore is a fakeRecordStore whose UpdateRecord/SoftDeleteRecord can be failed per id and
// which records the call order + tenant, so tests can assert order, partial semantics and
// tenant-pinning without a DB.
type massOpStore struct {
	fakeRecordStore
	failUpdate  map[string]error
	failDelete  map[string]error
	updateCalls []string
	deleteCalls []string
	tenants     map[string]struct{} // every tenantID seen on a write call
}

func (f *massOpStore) seeTenant(tenantID string) {
	if f.tenants == nil {
		f.tenants = map[string]struct{}{}
	}
	f.tenants[tenantID] = struct{}{}
}

func (f *massOpStore) UpdateRecord(_ context.Context, _, tenantID, id, _ string, _ dynrepo.RowScope, _ map[string]any, _ *int, _ []dynrepo.UniqueFieldSpec) error {
	f.seeTenant(tenantID)
	f.updateCalls = append(f.updateCalls, id)
	return f.failUpdate[id]
}

func (f *massOpStore) SoftDeleteRecord(_ context.Context, _, tenantID, id string, _ dynrepo.RowScope) error {
	f.seeTenant(tenantID)
	f.deleteCalls = append(f.deleteCalls, id)
	return f.failDelete[id]
}

// The per-row audit assertions reuse the package-shared captureAuditor fixture
// (field_acl_admin_test.go) — it records every audit event for later inspection.

func newMassOpService(store *massOpStore) *RecordService {
	cat := &fakeRecordCatalog{
		entity: courseEntityFixture(),
		fields: append(courseFieldDefs(), metadata.FieldDefinition{Name: "total", FieldType: "formula"}),
	}
	return NewRecordService(cat, store)
}

func massScope() RecordScope {
	return RecordScope{UserID: "u1", AccessLevel: "all", RoleID: "role-1"}
}

// Dedupe preserves first-seen order; every surviving id runs exactly one single-record update;
// every store write is pinned to the caller's tenant.
func TestUpdateBulk_OrderDedupeAndTenantPin(t *testing.T) {
	store := &massOpStore{}
	svc := newMassOpService(store)

	res, err := svc.UpdateBulk(context.Background(), massScope(), "tenant-1", "courses",
		[]string{"a", "b", "a"}, map[string]any{"title": "Go 201"})
	if err != nil {
		t.Fatalf("UpdateBulk: %v", err)
	}
	if res.Updated != 2 || res.Failed != 0 || len(res.Results) != 2 {
		t.Fatalf("got updated=%d failed=%d len=%d; want 2/0/2", res.Updated, res.Failed, len(res.Results))
	}
	if res.Results[0].ID != "a" || res.Results[1].ID != "b" {
		t.Fatalf("result order = %q,%q; want a,b", res.Results[0].ID, res.Results[1].ID)
	}
	if !res.Results[0].OK || !res.Results[1].OK {
		t.Fatalf("rows not ok: %+v", res.Results)
	}
	if len(store.updateCalls) != 2 || store.updateCalls[0] != "a" || store.updateCalls[1] != "b" {
		t.Fatalf("store update calls = %v; want [a b]", store.updateCalls)
	}
	if len(store.tenants) != 1 {
		t.Fatalf("store saw tenants %v; want exactly {tenant-1}", store.tenants)
	}
	if _, ok := store.tenants["tenant-1"]; !ok {
		t.Fatalf("store saw tenants %v; want tenant-1", store.tenants)
	}
}

// A per-row not-found (non-existent / cross-tenant / out-of-A5-scope) fails ONLY its row —
// siblings before AND after still run (partial semantics, results stay index-aligned).
func TestUpdateBulk_PerRowNotFoundDoesNotAbort(t *testing.T) {
	store := &massOpStore{failUpdate: map[string]error{"b": dynrepo.ErrRecordNotFound}}
	svc := newMassOpService(store)

	res, err := svc.UpdateBulk(context.Background(), massScope(), "tenant-1", "courses",
		[]string{"a", "b", "c"}, map[string]any{"title": "X"})
	if err != nil {
		t.Fatalf("UpdateBulk: %v", err)
	}
	if res.Updated != 2 || res.Failed != 1 {
		t.Fatalf("updated=%d failed=%d; want 2/1", res.Updated, res.Failed)
	}
	row := res.Results[1]
	if row.ID != "b" || row.OK || row.Code != "RECORD_NOT_FOUND" {
		t.Fatalf("row[1] = %+v; want b/failed/RECORD_NOT_FOUND", row)
	}
	if len(store.updateCalls) != 3 {
		t.Fatalf("update calls = %v; want all 3 ids attempted", store.updateCalls)
	}
}

// A values-level defect (unknown field / server-computed field) is NOT a request-level reject:
// per the ENG-MASSOP contract it fails EVERY row with the SAME code a single PUT would return
// (per-row like bulk-create partial mode) and never touches the store.
func TestUpdateBulk_ValuesDefectFailsPerRow(t *testing.T) {
	tests := []struct {
		name     string
		values   map[string]any
		wantCode string
	}{
		{name: "values field lạ", values: map[string]any{"nope": float64(1)}, wantCode: "RECORD_UNKNOWN_FIELD"},
		{name: "values readonly formula", values: map[string]any{"total": float64(5)}, wantCode: "RECORD_FORMULA_READONLY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &massOpStore{}
			svc := newMassOpService(store)
			res, err := svc.UpdateBulk(context.Background(), massScope(), "tenant-1", "courses",
				[]string{"a", "b"}, tt.values)
			if err != nil {
				t.Fatalf("UpdateBulk: %v (values defects must be per-row, not top-level)", err)
			}
			if res.Updated != 0 || res.Failed != 2 || len(res.Results) != 2 {
				t.Fatalf("updated=%d failed=%d len=%d; want 0/2/2", res.Updated, res.Failed, len(res.Results))
			}
			for _, row := range res.Results {
				if row.OK || row.Code != tt.wantCode {
					t.Fatalf("row %+v; want failed %s", row, tt.wantCode)
				}
			}
			if len(store.updateCalls) != 0 {
				t.Fatalf("store touched (%v) despite validation-failing values", store.updateCalls)
			}
		})
	}
}

// Request-level guards return a coded ValidationError (→ 422) and never touch the store.
func TestMassOp_RequestLevelGuards(t *testing.T) {
	big := make([]string, defaultMassOpMaxIDs+1)
	for i := range big {
		big[i] = string(rune('a' + i%26))
	}
	tests := []struct {
		name     string
		ids      []string
		values   map[string]any
		wantCode string
	}{
		{name: "ids rỗng", ids: nil, values: map[string]any{"title": "X"}, wantCode: "BULK_EMPTY"},
		{name: "ids toàn chuỗi trắng", ids: []string{"", "  "}, values: map[string]any{"title": "X"}, wantCode: "BULK_EMPTY"},
		{name: "quá cap id", ids: big, values: map[string]any{"title": "X"}, wantCode: "BULK_TOO_MANY"},
		{name: "values rỗng", ids: []string{"a"}, values: map[string]any{}, wantCode: "BULK_VALUES_EMPTY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &massOpStore{}
			svc := newMassOpService(store)
			_, err := svc.UpdateBulk(context.Background(), massScope(), "tenant-1", "courses", tt.ids, tt.values)
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != tt.wantCode {
				t.Fatalf("err = %v; want ValidationError %s", err, tt.wantCode)
			}
			if len(store.updateCalls) != 0 {
				t.Fatalf("store touched (%v) despite request-level reject", store.updateCalls)
			}
		})
	}
}

// MAX_BULK_OPS overrides the cap; a garbage/non-positive value degrades to the default instead
// of disabling the cap (same env contract as BULK_MAX_ROWS).
func TestMassOpMaxIDs_EnvOverrideAndDegrade(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want int
	}{
		{name: "unset → default", env: "", want: defaultMassOpMaxIDs},
		{name: "override", env: "3", want: 3},
		{name: "âm → default", env: "-5", want: defaultMassOpMaxIDs},
		{name: "rác → default", env: "many", want: defaultMassOpMaxIDs},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env == "" {
				t.Setenv("MAX_BULK_OPS", "")
				// t.Setenv registers the restore; unset for the real "absent" case.
				if err := os.Unsetenv("MAX_BULK_OPS"); err != nil {
					t.Fatalf("unsetenv: %v", err)
				}
			} else {
				t.Setenv("MAX_BULK_OPS", tt.env)
			}
			if got := MassOpMaxIDs(); got != tt.want {
				t.Fatalf("MassOpMaxIDs() = %d; want %d", got, tt.want)
			}
		})
	}
}

// The lowered env cap actually rejects an oversized batch with BULK_TOO_MANY + details.max.
func TestUpdateBulk_EnvCapEnforced(t *testing.T) {
	t.Setenv("MAX_BULK_OPS", "2")
	svc := newMassOpService(&massOpStore{})
	_, err := svc.UpdateBulk(context.Background(), massScope(), "tenant-1", "courses",
		[]string{"a", "b", "c"}, map[string]any{"title": "X"})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "BULK_TOO_MANY" {
		t.Fatalf("err = %v; want BULK_TOO_MANY", err)
	}
	if ve.Details["max"] != 2 {
		t.Fatalf("details.max = %v; want 2", ve.Details["max"])
	}
}

// An infrastructure failure (not a data problem) aborts with a TOP-LEVEL error so the handler
// classifies it (503/500) instead of masking it as a per-row 422-shaped code.
func TestUpdateBulk_InfraErrorBubbles(t *testing.T) {
	store := &massOpStore{failUpdate: map[string]error{"b": errors.New("conn reset")}}
	svc := newMassOpService(store)

	_, err := svc.UpdateBulk(context.Background(), massScope(), "tenant-1", "courses",
		[]string{"a", "b"}, map[string]any{"title": "X"})
	if err == nil {
		t.Fatal("want top-level error, got nil")
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		t.Fatalf("infra error surfaced as ValidationError %s; must stay top-level", ve.Code)
	}
}

// DeleteBulk mirrors the same partial semantics through the single-record Delete path.
func TestDeleteBulk_PartialAndDedupe(t *testing.T) {
	store := &massOpStore{failDelete: map[string]error{"b": dynrepo.ErrRecordNotFound}}
	svc := newMassOpService(store)

	res, err := svc.DeleteBulk(context.Background(), massScope(), "tenant-1", "courses",
		[]string{"a", "b", "c", "a"})
	if err != nil {
		t.Fatalf("DeleteBulk: %v", err)
	}
	if res.Deleted != 2 || res.Failed != 1 || len(res.Results) != 3 {
		t.Fatalf("deleted=%d failed=%d len=%d; want 2/1/3", res.Deleted, res.Failed, len(res.Results))
	}
	if res.Results[1].Code != "RECORD_NOT_FOUND" || res.Results[1].OK {
		t.Fatalf("row[1] = %+v; want failed RECORD_NOT_FOUND", res.Results[1])
	}
	if len(store.deleteCalls) != 3 || store.deleteCalls[2] != "c" {
		t.Fatalf("delete calls = %v; want [a b c]", store.deleteCalls)
	}
}

// DeleteBulk with empty ids is the same request-level 422 as update.
func TestDeleteBulk_EmptyIDs(t *testing.T) {
	svc := newMassOpService(&massOpStore{})
	_, err := svc.DeleteBulk(context.Background(), massScope(), "tenant-1", "courses", []string{})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "BULK_EMPTY" {
		t.Fatalf("err = %v; want BULK_EMPTY", err)
	}
}

// Per-row ENGINE reuse proof: because each row runs the real Update/Delete, the auditor receives
// exactly one event per SUCCEEDED row (failed rows never commit, so they never audit) with the
// right action + record id — the same emission a manual PUT/DELETE produces.
func TestMassOp_PerRowAuditEmission(t *testing.T) {
	store := &massOpStore{failDelete: map[string]error{"b": dynrepo.ErrRecordNotFound}}
	aud := &captureAuditor{}
	svc := newMassOpService(store).WithAuditor(aud)

	if _, err := svc.UpdateBulk(context.Background(), massScope(), "tenant-1", "courses",
		[]string{"a", "b"}, map[string]any{"title": "X"}); err != nil {
		t.Fatalf("UpdateBulk: %v", err)
	}
	if _, err := svc.DeleteBulk(context.Background(), massScope(), "tenant-1", "courses",
		[]string{"a", "b"}); err != nil {
		t.Fatalf("DeleteBulk: %v", err)
	}

	var updates, deletes int
	for _, evt := range aud.events {
		if evt.TenantID != "tenant-1" || evt.EntityName != "courses" {
			t.Fatalf("audit event off-scope: %+v", evt)
		}
		switch evt.Action {
		case audit.ActionUpdate:
			updates++
		case audit.ActionDelete:
			deletes++
		}
	}
	// 2 update rows both succeed (store default nil); delete: a succeeds, b fails not-found.
	if updates != 2 || deletes != 1 {
		t.Fatalf("audit events update=%d delete=%d; want 2/1 (one per committed row)", updates, deletes)
	}
}
