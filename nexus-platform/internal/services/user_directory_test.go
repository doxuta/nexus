package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/auth"
)

// fakeDirectoryRepo records the (tenantID, q, limit, offset) it was called with and returns a
// canned page — enough to prove UserDirectoryService forwards/clamps params correctly without a
// real DB (the SQL-level tenant/status/q filtering is proven separately by the DB-backed
// integration test in internal/handlers, per docs/NEXUS-MOT-XUONG-2026-07-29.md §0b: "mỗi cổng
// chỉ kiểm một đầu" — this unit test covers the service's clamping logic, the handler-level test
// covers response shape, the integration test covers the actual WHERE clause).
type fakeDirectoryRepo struct {
	gotTenantID         string
	gotQ                string
	gotLimit, gotOffset int
	rows                []auth.DirectoryUser
	total               int64
	err                 error
}

func (f *fakeDirectoryRepo) ListDirectory(_ context.Context, tenantID, q string, limit, offset int) ([]auth.DirectoryUser, int64, error) {
	f.gotTenantID, f.gotQ, f.gotLimit, f.gotOffset = tenantID, q, limit, offset
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.rows, f.total, nil
}

func TestUserDirectoryService_ListDirectory_Clamping(t *testing.T) {
	tests := []struct {
		name        string
		page, limit int
		wantLimit   int
		wantOffset  int
		wantPage    int
	}{
		{"defaults on zero", 0, 0, 20, 0, 1},
		{"negative page clamps to 1", -5, 20, 20, 0, 1},
		{"negative limit clamps to 20", 1, -1, 20, 0, 1},
		{"limit over 100 clamps to 100", 1, 500, 100, 0, 1},
		{"page 2 computes offset from clamped limit", 2, 10, 10, 10, 2},
		{"within bounds passes through", 3, 50, 50, 100, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeDirectoryRepo{}
			svc := NewUserDirectoryService(repo)
			res, err := svc.ListDirectory(context.Background(), "t1", "", tt.page, tt.limit)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if repo.gotLimit != tt.wantLimit {
				t.Errorf("repo limit = %d, want %d", repo.gotLimit, tt.wantLimit)
			}
			if repo.gotOffset != tt.wantOffset {
				t.Errorf("repo offset = %d, want %d", repo.gotOffset, tt.wantOffset)
			}
			if res.Page != tt.wantPage {
				t.Errorf("result page = %d, want %d", res.Page, tt.wantPage)
			}
			if res.Limit != tt.wantLimit {
				t.Errorf("result limit = %d, want %d", res.Limit, tt.wantLimit)
			}
		})
	}
}

// TestUserDirectoryService_ListDirectory_ForwardsTenantAndQ proves tenantID and q reach the repo
// UNCHANGED — the actual WHERE-clause enforcement (tenant isolation, status='active', q substring
// match) lives in auth.UserRepository.ListDirectory and is proven by the DB-backed integration
// test; this only proves the service does not drop or mutate them en route.
func TestUserDirectoryService_ListDirectory_ForwardsTenantAndQ(t *testing.T) {
	repo := &fakeDirectoryRepo{rows: []auth.DirectoryUser{{ID: "u1", FullName: "Nguyễn Văn A", Email: "a@co.vn"}}, total: 1}
	svc := NewUserDirectoryService(repo)

	res, err := svc.ListDirectory(context.Background(), "tenant-42", "  nguyen  ", 1, 20)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if repo.gotTenantID != "tenant-42" {
		t.Errorf("tenantID = %q, want tenant-42", repo.gotTenantID)
	}
	// The service does not trim q itself (the handler already does); it must still pass through
	// whatever it receives verbatim so the repo's own trim/escape stays the single source of truth.
	if repo.gotQ != "  nguyen  " {
		t.Errorf("q = %q, want unchanged", repo.gotQ)
	}
	if len(res.Items) != 1 || res.Items[0].ID != "u1" {
		t.Errorf("items = %+v, want the single fake row", res.Items)
	}
	if res.Total != 1 || res.TotalPages != 1 {
		t.Errorf("total=%d totalPages=%d, want 1/1", res.Total, res.TotalPages)
	}
}

func TestUserDirectoryService_ListDirectory_TotalPagesFloorsAtOne(t *testing.T) {
	repo := &fakeDirectoryRepo{rows: nil, total: 0}
	svc := NewUserDirectoryService(repo)

	res, err := svc.ListDirectory(context.Background(), "t1", "", 1, 20)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.TotalPages != 1 {
		t.Errorf("TotalPages = %d, want 1 (never zero, even with no rows)", res.TotalPages)
	}
	if len(res.Items) != 0 {
		t.Errorf("Items = %+v, want empty", res.Items)
	}
}

func TestUserDirectoryService_ListDirectory_WrapsRepoError(t *testing.T) {
	sentinel := errors.New("boom")
	repo := &fakeDirectoryRepo{err: sentinel}
	svc := NewUserDirectoryService(repo)

	_, err := svc.ListDirectory(context.Background(), "t1", "", 1, 20)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error does not wrap sentinel: %v", err)
	}
	wantPrefix := "services.UserDirectoryService.ListDirectory: "
	if got := err.Error(); len(got) < len(wantPrefix) || got[:len(wantPrefix)] != wantPrefix {
		t.Errorf("error = %q, want prefix %q (fmt.Errorf %%w convention)", got, wantPrefix)
	}
}
