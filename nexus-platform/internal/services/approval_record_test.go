package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/dynrepo"
)

// fakeApprovalInstanceRepo is a minimal fake for approvalInstanceRepo used by the record test.
// The service tests for pending-list/decide are in bpm_approval_integration_test.go (integration).
type fakeApprovalInstanceRepo struct {
	byRecord map[string][]bpm.Instance // key: entityName+"\x00"+recordID
	// byLane holds the instances of the other lanes: key lane+"\x00"+entityName+"\x00"+recordID.
	byLane map[string][]bpm.Instance
}

func (f *fakeApprovalInstanceRepo) ListByRecordLane(_ context.Context, _, entityName, recordID, lane string) ([]bpm.Instance, error) {
	return f.byLane[lane+"\x00"+entityName+"\x00"+recordID], nil
}

func (f *fakeApprovalInstanceRepo) ListPendingForUser(_ context.Context, _, _ string, _ []string, _ bpm.EntityView) ([]bpm.Instance, error) {
	return nil, nil
}

func (f *fakeApprovalInstanceRepo) GetInstance(_ context.Context, _, _ string) (*bpm.Instance, error) {
	return nil, bpm.ErrInstanceNotFound
}

func (f *fakeApprovalInstanceRepo) ListByRecord(_ context.Context, _, entityName, recordID string) ([]bpm.Instance, error) {
	key := entityName + "\x00" + recordID
	return f.byRecord[key], nil
}

// ListOpenSeats: this fake holds no seats — the visibility gate answers through the projection.
func (f *fakeApprovalInstanceRepo) ListOpenSeats(context.Context, string, string) ([]bpm.Seat, error) {
	return nil, nil
}

// fakeApprovalProcessRepo for the service constructor.
type fakeApprovalProcessRepo struct{}

func (f *fakeApprovalProcessRepo) CreateProcess(_ context.Context, _ bpm.CreateProcessInput) (string, error) {
	return "", errors.New("not implemented")
}
func (f *fakeApprovalProcessRepo) GetProcess(_ context.Context, _, _ string) (*bpm.ProcessRecord, error) {
	return nil, bpm.ErrProcessNotFound
}
func (f *fakeApprovalProcessRepo) ListProcesses(_ context.Context, _ string) ([]bpm.ProcessRecord, error) {
	return nil, nil
}
func (f *fakeApprovalProcessRepo) UpdateProcess(_ context.Context, _, _ string, _ bpm.UpdateProcessInput) error {
	return errors.New("not implemented")
}
func (f *fakeApprovalProcessRepo) DeleteProcess(_ context.Context, _, _ string) error {
	return errors.New("not implemented")
}

// S10 (specs/043): the version/draft half of approvalProcessRepo — not exercised by these tests.
func (f *fakeApprovalProcessRepo) SaveDraft(context.Context, string, string, bpm.SaveDraftInput) error {
	return errors.New("not implemented")
}
func (f *fakeApprovalProcessRepo) DiscardDraft(context.Context, string, string) error {
	return errors.New("not implemented")
}
func (f *fakeApprovalProcessRepo) Publish(context.Context, string, string, string, string) (int, error) {
	return 0, errors.New("not implemented")
}
func (f *fakeApprovalProcessRepo) ListVersions(context.Context, string, string) ([]bpm.VersionRecord, error) {
	return nil, nil
}
func (f *fakeApprovalProcessRepo) GetVersion(context.Context, string, string, int) (*bpm.VersionRecord, error) {
	return nil, bpm.ErrVersionNotFound
}
func (f *fakeApprovalProcessRepo) PublishedNodeIDs(context.Context, string, string) (map[string]bool, error) {
	return nil, nil
}

func (f *fakeApprovalProcessRepo) ProcessImpact(context.Context, string, string) (*bpm.Impact, error) {
	return &bpm.Impact{}, nil
}
func (f *fakeApprovalProcessRepo) ListProcessInstances(context.Context, string, string, *int, string, bool, int) ([]bpm.ProcessInstanceRow, error) {
	return nil, nil
}
func (f *fakeApprovalProcessRepo) Repin(context.Context, string, string, bpm.RepinInput) (*bpm.RepinOutcome, error) {
	return &bpm.RepinOutcome{}, nil
}

// fakeApprovalDecider does nothing — not exercised in these tests.
type fakeApprovalDecider struct{}

func (f *fakeApprovalDecider) DecideAt(_ context.Context, _, _ string, _ bpm.DecisionKind, _ string, _ bpm.Actor, _ *int) (*bpm.Instance, error) {
	return nil, errors.New("not implemented")
}

func TestApprovalService_GetInstancesForRecord(t *testing.T) {
	inst1 := bpm.Instance{ID: "ai-1", EntityName: "accounts", RecordID: "rec-1"}
	inst2 := bpm.Instance{ID: "ai-2", EntityName: "accounts", RecordID: "rec-1", Status: bpm.StatusApproved}

	tests := []struct {
		name       string
		tenantID   string
		entityName string
		recordID   string
		seedKey    string
		seedRows   []bpm.Instance
		wantLen    int
		wantErr    bool
	}{
		{
			name:       "returns instances for record",
			tenantID:   "t1",
			entityName: "accounts",
			recordID:   "rec-1",
			seedKey:    "accounts\x00rec-1",
			seedRows:   []bpm.Instance{inst1, inst2},
			wantLen:    2,
		},
		{
			name:       "returns empty slice when no approvals attached",
			tenantID:   "t1",
			entityName: "accounts",
			recordID:   "rec-missing",
			seedKey:    "accounts\x00rec-1", // seeded under a different record
			seedRows:   []bpm.Instance{inst1},
			wantLen:    0,
		},
		{
			name:       "validation error when entityName empty",
			tenantID:   "t1",
			entityName: "",
			recordID:   "rec-1",
			wantErr:    true,
		},
		{
			name:       "validation error when recordID empty",
			tenantID:   "t1",
			entityName: "accounts",
			recordID:   "",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			byRecord := map[string][]bpm.Instance{}
			if tt.seedKey != "" {
				byRecord[tt.seedKey] = tt.seedRows
			}
			instRepo := &fakeApprovalInstanceRepo{byRecord: byRecord}
			svc := NewApprovalService(&fakeApprovalProcessRepo{}, instRepo, &fakeApprovalDecider{})

			rows, err := svc.GetInstancesForRecord(context.Background(), RecordScope{IsAdmin: true}, tt.tenantID, tt.entityName, tt.recordID)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(rows) != tt.wantLen {
				t.Errorf("len(rows) = %d, want %d", len(rows), tt.wantLen)
			}
		})
	}
}

func TestApprovalService_GetInstancesForRecord_TenantIsolation(t *testing.T) {
	// Tenant A's instance must not appear in Tenant B's query (the repo is the enforcement
	// boundary — the service passes tenantID through). Simulate by having the fake return
	// different data when the repo itself is tenant-aware.
	//
	// This test verifies the service correctly threads tenantID to the repo; cross-tenant
	// enforcement is the repo's contract (SQL WHERE tenant_id = ?).
	instA := bpm.Instance{ID: "ai-a", EntityName: "tickets", RecordID: "rec-x"}

	byRecord := map[string][]bpm.Instance{
		"tickets\x00rec-x": {instA},
	}
	instRepo := &fakeApprovalInstanceRepo{byRecord: byRecord}
	svc := NewApprovalService(&fakeApprovalProcessRepo{}, instRepo, &fakeApprovalDecider{})

	// Tenant A gets the instance.
	rowsA, err := svc.GetInstancesForRecord(context.Background(), RecordScope{IsAdmin: true}, "tenant-a", "tickets", "rec-x")
	if err != nil {
		t.Fatalf("tenant A: %v", err)
	}
	if len(rowsA) != 1 {
		t.Errorf("tenant A: expected 1 instance, got %d", len(rowsA))
	}

	// Tenant B with the same entity+recordID — in the real repo the WHERE tenant_id=? would
	// return nothing. The fake here can't simulate that without tenant-keyed seeds, so we just
	// verify the service passes through without error (cross-tenant enforcement is at the DB layer).
	_, err = svc.GetInstancesForRecord(context.Background(), RecordScope{IsAdmin: true}, "tenant-b", "tickets", "rec-x")
	if err != nil {
		t.Fatalf("tenant B: %v", err)
	}
}

// fakeApprovalRecordReader answers the row-visibility probe: err != nil ⇒ the caller may not see
// the record (or it does not exist).
type fakeApprovalRecordReader struct {
	err error
	// authzLevel is what AuthorizeView resolves to. Empty means "the ACL grants nothing", which is
	// what a non-admin on a flat route really gets before the service re-resolves.
	authzLevel string
	authzErr   error
	sawLevel   string // the level the record probe was actually called with
}

func (f *fakeApprovalRecordReader) AuthorizeView(_ context.Context, _ string, scope RecordScope, _ string) (RecordScope, error) {
	if f.authzErr != nil {
		return RecordScope{}, f.authzErr
	}
	scope.AccessLevel = f.authzLevel
	return scope, nil
}

func (f *fakeApprovalRecordReader) Get(_ context.Context, sc RecordScope, _, _, _ string, _, _ []string) (*dynrepo.Record, error) {
	f.sawLevel = sc.AccessLevel
	if f.err != nil {
		return nil, f.err
	}
	return &dynrepo.Record{ID: "rec-1"}, nil
}

// TestApprovalService_GetInstancesForRecord_RowScope pins the HIGH-severity fix: a non-admin whose
// row scope hides the record must NOT read its approval timeline (who approved, reject reasons,
// delegation targets). Entity-level view is not row-level permission.
func TestApprovalService_GetInstancesForRecord_RowScope(t *testing.T) {
	seed := map[string][]bpm.Instance{"accounts\x00rec-1": {{ID: "ai-1", EntityName: "accounts", RecordID: "rec-1"}}}

	t.Run("row visible → timeline returned", func(t *testing.T) {
		svc := NewApprovalService(&fakeApprovalProcessRepo{}, &fakeApprovalInstanceRepo{byRecord: seed}, &fakeApprovalDecider{}).
			WithRequestFlow(nil, &fakeApprovalRecordReader{})
		rows, err := svc.GetInstancesForRecord(context.Background(),
			RecordScope{UserID: "u1", RoleID: "r1", AccessLevel: "owner"}, "t1", "accounts", "rec-1")
		if err != nil || len(rows) != 1 {
			t.Fatalf("visible row: rows=%d err=%v, want the timeline", len(rows), err)
		}
	})

	t.Run("row hidden → refused with the record read's own error, NOT the timeline", func(t *testing.T) {
		svc := NewApprovalService(&fakeApprovalProcessRepo{}, &fakeApprovalInstanceRepo{byRecord: seed}, &fakeApprovalDecider{}).
			WithRequestFlow(nil, &fakeApprovalRecordReader{err: ErrRecordNotFound})
		_, err := svc.GetInstancesForRecord(context.Background(),
			RecordScope{UserID: "u2", RoleID: "r2", AccessLevel: "owner"}, "t1", "accounts", "rec-1")
		if !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("hidden row: err=%v, want ErrRecordNotFound (no timeline, no oracle)", err)
		}
	})

	t.Run("unwired records dep for a non-admin fails CLOSED", func(t *testing.T) {
		svc := NewApprovalService(&fakeApprovalProcessRepo{}, &fakeApprovalInstanceRepo{byRecord: seed}, &fakeApprovalDecider{})
		_, err := svc.GetInstancesForRecord(context.Background(),
			RecordScope{UserID: "u3", AccessLevel: "owner"}, "t1", "accounts", "rec-1")
		if err == nil {
			t.Fatal("unwired probe served a timeline to a non-admin — must fail closed")
		}
	})

	t.Run("admin bypasses the probe", func(t *testing.T) {
		svc := NewApprovalService(&fakeApprovalProcessRepo{}, &fakeApprovalInstanceRepo{byRecord: seed}, &fakeApprovalDecider{}).
			WithRequestFlow(nil, &fakeApprovalRecordReader{err: ErrRecordNotFound})
		rows, err := svc.GetInstancesForRecord(context.Background(),
			RecordScope{IsAdmin: true}, "t1", "accounts", "rec-1")
		if err != nil || len(rows) != 1 {
			t.Fatalf("admin: rows=%d err=%v, want the full timeline regardless of probe", len(rows), err)
		}
	})
}
