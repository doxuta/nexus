package services

// W6 — GET /approvals/:id visibility + shape unit tests (fakes, no DB). The detail endpoint must
// mirror the /approvals/pending ACL surface: admin OR the current step's approver sees it;
// everyone else gets the SAME not-found as an unknown id (no existence oracle).

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/dynrepo"
)

type fakeApprovalInstRepo struct {
	inst *bpm.Instance
}

func (f *fakeApprovalInstRepo) ListPendingForUser(context.Context, string, string, []string, bpm.EntityView) ([]bpm.Instance, error) {
	return nil, nil
}

func (f *fakeApprovalInstRepo) GetInstance(_ context.Context, tenantID, id string) (*bpm.Instance, error) {
	if f.inst == nil || f.inst.ID != id || f.inst.TenantID != tenantID {
		return nil, bpm.ErrInstanceNotFound
	}
	cp := *f.inst
	return &cp, nil
}

func (f *fakeApprovalInstRepo) ListByRecordLane(context.Context, string, string, string, string) ([]bpm.Instance, error) {
	return nil, nil
}

func (f *fakeApprovalInstRepo) ListByRecord(context.Context, string, string, string) ([]bpm.Instance, error) {
	return nil, nil
}

// ListOpenSeats: this fake holds no seats — the visibility gate answers through the projection.
func (f *fakeApprovalInstRepo) ListOpenSeats(context.Context, string, string) ([]bpm.Seat, error) {
	return nil, nil
}

type fakeApprovalProcRepo struct {
	fakeApprovalProcessRepo // the S10 draft/publish/history half (specs/043) — unused here
	proc                    *bpm.ProcessRecord
}

func (f *fakeApprovalProcRepo) CreateProcess(context.Context, bpm.CreateProcessInput) (string, error) {
	return "", errors.New("not implemented")
}

func (f *fakeApprovalProcRepo) GetProcess(_ context.Context, tenantID, id string) (*bpm.ProcessRecord, error) {
	if f.proc == nil || f.proc.ID != id || f.proc.TenantID != tenantID {
		return nil, bpm.ErrProcessNotFound
	}
	cp := *f.proc
	return &cp, nil
}

func (f *fakeApprovalProcRepo) ListProcesses(context.Context, string) ([]bpm.ProcessRecord, error) {
	return nil, nil
}

func (f *fakeApprovalProcRepo) UpdateProcess(context.Context, string, string, bpm.UpdateProcessInput) error {
	return errors.New("not implemented")
}

func (f *fakeApprovalProcRepo) DeleteProcess(context.Context, string, string) error {
	return errors.New("not implemented")
}

func w6ApprovalSvc(inst *bpm.Instance, proc *bpm.ProcessRecord) *ApprovalService {
	return NewApprovalService(&fakeApprovalProcRepo{proc: proc}, &fakeApprovalInstRepo{inst: inst}, nil)
}

func w6Instance() *bpm.Instance {
	due := time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC)
	return &bpm.Instance{
		ID: "inst-1", TenantID: "t1", ProcessID: "proc-1", EntityName: "deals", RecordID: "rec-1",
		CurrentStep: 1, Status: bpm.StatusPending,
		CurrentApproverType: bpm.ApproverUser, CurrentApproverID: "approver-9",
		SLADueAt: &due,
		History: []bpm.HistoryEntry{
			{Step: 0, Action: bpm.HistoryApproved, Actor: "u-first", At: time.Date(2026, 7, 3, 8, 0, 0, 0, time.UTC)},
		},
		CreatedBy: "creator-1",
	}
}

func w6Process() *bpm.ProcessRecord {
	return &bpm.ProcessRecord{
		ID: "proc-1", TenantID: "t1", Name: "Duyệt chi", EntityName: "deals", Trigger: "after_create",
		Steps:    json.RawMessage(`[{"approverType":"user","approverId":"u-first"},{"approverType":"user","approverId":"approver-9","slaSeconds":3600}]`),
		IsActive: true,
	}
}

func TestApprovalDetail_Visibility(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(i *bpm.Instance)
		actor   bpm.Actor
		wantOK  bool
		wantErr string
	}{
		{name: "current user-approver sees it", actor: bpm.Actor{UserID: "approver-9"}, wantOK: true},
		{name: "admin sees it", actor: bpm.Actor{UserID: "someone-else", IsAdmin: true}, wantOK: true},
		{
			name: "current role-approver sees it",
			mutate: func(i *bpm.Instance) {
				i.CurrentApproverType = bpm.ApproverRole
				i.CurrentApproverID = "role-5"
			},
			actor: bpm.Actor{UserID: "u-any", RoleIDs: []string{"role-5"}}, wantOK: true,
		},
		{
			name:  "unrelated user gets not-found (no existence leak)",
			actor: bpm.Actor{UserID: "stranger"}, wantOK: false, wantErr: "APPROVAL_INSTANCE_NOT_FOUND",
		},
		{
			name: "past approver of a terminal instance gets not-found (mirrors pending scope)",
			mutate: func(i *bpm.Instance) {
				i.Status = bpm.StatusApproved
			},
			actor: bpm.Actor{UserID: "approver-9"}, wantOK: false, wantErr: "APPROVAL_INSTANCE_NOT_FOUND",
		},
		{
			name: "admin still sees a terminal instance",
			mutate: func(i *bpm.Instance) {
				i.Status = bpm.StatusRejected
			},
			actor: bpm.Actor{UserID: "x", IsAdmin: true}, wantOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := w6Instance()
			if tt.mutate != nil {
				tt.mutate(inst)
			}
			svc := w6ApprovalSvc(inst, w6Process())
			got, err := svc.GetApprovalDetail(context.Background(), RecordScope{}, "t1", "inst-1", tt.actor)
			if tt.wantOK {
				if err != nil {
					t.Fatalf("GetApprovalDetail err = %v; want detail", err)
				}
				if got.ID != "inst-1" || got.RecordID != "rec-1" || got.EntityName != "deals" {
					t.Errorf("detail = %+v; want inst-1/rec-1/deals", got)
				}
				if got.Process == nil || got.Process.ID != "proc-1" || got.Process.Name != "Duyệt chi" {
					t.Errorf("process context missing/wrong: %+v", got.Process)
				}
				if len(got.History) != 1 || got.History[0].Actor != "u-first" {
					t.Errorf("history timeline = %+v; want the step-0 approval", got.History)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != tt.wantErr {
				t.Fatalf("err = %v; want code %s", err, tt.wantErr)
			}
		})
	}
}

func TestApprovalDetail_CrossTenantAndUnknown(t *testing.T) {
	svc := w6ApprovalSvc(w6Instance(), w6Process())
	admin := bpm.Actor{UserID: "root", IsAdmin: true}

	// Cross-tenant id → not-found even for an admin of ANOTHER tenant.
	if _, err := svc.GetApprovalDetail(context.Background(), RecordScope{}, "t2", "inst-1", admin); err == nil {
		t.Fatalf("cross-tenant read must be not-found")
	}
	// Unknown id → not-found.
	var ve *ValidationError
	_, err := svc.GetApprovalDetail(context.Background(), RecordScope{}, "t1", "ghost", admin)
	if !errors.As(err, &ve) || ve.Code != "APPROVAL_INSTANCE_NOT_FOUND" {
		t.Fatalf("unknown id err = %v; want APPROVAL_INSTANCE_NOT_FOUND", err)
	}
	// Blank id → 422 required.
	_, err = svc.GetApprovalDetail(context.Background(), RecordScope{}, "t1", "  ", admin)
	if !errors.As(err, &ve) || ve.Code != "APPROVAL_INSTANCE_ID_REQUIRED" {
		t.Fatalf("blank id err = %v; want APPROVAL_INSTANCE_ID_REQUIRED", err)
	}
}

func TestApprovalDetail_ProcessGoneIsNull(t *testing.T) {
	// A pre-000065 instance (no pin) whose process was soft-deleted AFTER arming leaves
	// process: null — there is nothing left to draw; the instance detail still loads.
	svc := w6ApprovalSvc(w6Instance(), nil)
	got, err := svc.GetApprovalDetail(context.Background(), RecordScope{}, "t1", "inst-1", bpm.Actor{UserID: "approver-9"})
	if err != nil {
		t.Fatalf("GetApprovalDetail err = %v", err)
	}
	if got.Process != nil {
		t.Errorf("process = %+v; want nil when the definition is gone", got.Process)
	}
	if got.History == nil {
		t.Errorf("history must never be nil (JSON [] not null)")
	}
}

// TestApprovalDetail_ProcessGonePinnedStepsServed: the engine keeps deciding a PINNED instance
// after its process is deleted (definitionOf never touches the live row), so the detail must keep
// serving the pinned steps too — otherwise the approver decides blind (no "/3", no next step)
// on exactly the path where the timeline and the engine are contractually the same thing.
func TestApprovalDetail_ProcessGonePinnedStepsServed(t *testing.T) {
	inst := w6Instance()
	inst.Definition = &bpm.Definition{Steps: []bpm.Step{
		{Name: "Trưởng phòng", ApproverType: bpm.ApproverUser, ApproverID: "u-first"},
		{Name: "Giám đốc", ApproverType: bpm.ApproverUser, ApproverID: "approver-9"},
		{Name: "CFO", ApproverType: bpm.ApproverRole, ApproverID: "role-cfo"},
	}}
	svc := w6ApprovalSvc(inst, nil)
	got, err := svc.GetApprovalDetail(context.Background(), RecordScope{}, "t1", "inst-1", bpm.Actor{UserID: "approver-9"})
	if err != nil {
		t.Fatalf("GetApprovalDetail err = %v", err)
	}
	if got.Process == nil {
		t.Fatalf("process = nil; want the pinned definition served even though the live row is gone")
	}
	if !got.Process.Pinned {
		t.Errorf("process.pinned = false; want true — a deleted process serves the pinned copy too")
	}
	if !got.Process.Deleted || got.Process.ID != "proc-1" || got.Process.EntityName != "deals" || got.Process.IsActive {
		t.Errorf("process context = %+v; want deleted=true id=proc-1 entityName=deals isActive=false", got.Process)
	}
	var steps []bpm.Step
	if err := json.Unmarshal(got.Process.Steps, &steps); err != nil || len(steps) != 3 || steps[2].ApproverID != "role-cfo" {
		t.Errorf("process.steps = %s (err=%v); want the 3 pinned steps", got.Process.Steps, err)
	}
	// DAILY-QUEUE #69: with the process gone, the pinned step NAMES are the only human labels left
	// on the approval screen (a role approver reads "vai trò #<uuid8>" to a non-admin) — they must
	// reach the payload. The FE mapper dropping them was the bug; this pins the server half.
	for i, want := range []string{"Trưởng phòng", "Giám đốc", "CFO"} {
		if i < len(steps) && steps[i].Name != want {
			t.Errorf("process.steps[%d].name = %q; want %q (the pinned step name)", i, steps[i].Name, want)
		}
	}
}

// TestApprovalDetail_LiveProcessEditedPinnedStepsWin is the display half of DAILY-QUEUE #8. The
// engine walks the PINNED steps, so the timeline must too — that is the whole contract of
// api-conventions.md §9.5 ("màn hình và engine không bao giờ lệch nhau"). The deleted-process case
// above only proves the pin is used when the live row is GONE; narrowing the override in
// GetApprovalDetail to `proc == nil` left every other test green, so the case that actually bites
// — live row PRESENT but edited after arming — was unguarded. An approver reading a 2-step
// timeline while the engine runs 3 pinned steps is the same blind decision, one layer up.
func TestApprovalDetail_LiveProcessEditedPinnedStepsWin(t *testing.T) {
	inst := w6Instance()
	inst.Definition = &bpm.Definition{Steps: []bpm.Step{
		{Name: "Trưởng phòng", ApproverType: bpm.ApproverUser, ApproverID: "u-first"},
		{Name: "Giám đốc", ApproverType: bpm.ApproverUser, ApproverID: "approver-9"},
		{Name: "CFO", ApproverType: bpm.ApproverRole, ApproverID: "role-cfo"},
	}}
	// The live row still exists and was shortened to 2 steps after this instance was armed.
	svc := w6ApprovalSvc(inst, w6Process())
	got, err := svc.GetApprovalDetail(context.Background(), RecordScope{}, "t1", "inst-1", bpm.Actor{UserID: "approver-9"})
	if err != nil {
		t.Fatalf("GetApprovalDetail err = %v", err)
	}
	if got.Process == nil {
		t.Fatalf("process = nil; want the live display context plus the pinned steps")
	}
	if got.Process.Deleted {
		t.Errorf("process.deleted = true; want false — the live row is still there")
	}
	if !got.Process.Pinned {
		t.Errorf("process.pinned = false; want true — steps are the frozen copy, and the screen has no other way to say so")
	}
	// Display context stays LIVE: name/trigger/isActive are not pinned.
	if got.Process.Name != "Duyệt chi" || got.Process.Trigger != "after_create" || !got.Process.IsActive {
		t.Errorf("process display context = %+v; want the live name/trigger/isActive", got.Process)
	}
	var steps []bpm.Step
	if err := json.Unmarshal(got.Process.Steps, &steps); err != nil {
		t.Fatalf("decode process.steps = %v", err)
	}
	if len(steps) != 3 {
		t.Fatalf("process.steps = %s; want the 3 PINNED steps, not the 2 the live row was edited down to", got.Process.Steps)
	}
	if steps[2].ApproverType != bpm.ApproverRole || steps[2].ApproverID != "role-cfo" {
		t.Errorf("process.steps[2] = %+v; want the pinned CFO role step", steps[2])
	}
}

// TestGetApprovalDetail_RecordViewerCanSee pins the widened visibility: a non-admin who is NOT the
// current approver but CAN see the record must read the detail — the record's Approvals tab links
// to /approvals/:id, so without this the submitter 404s on their own submission and a past approver
// 404s on a decision they made.
func TestGetApprovalDetail_RecordViewerCanSee(t *testing.T) {
	// A TERMINAL instance whose current-approver check would reject a non-admin.
	inst := &bpm.Instance{
		ID: "inst-1", TenantID: "t1", ProcessID: "p1", EntityName: "crm_quotes", RecordID: "q1",
		Status: bpm.StatusApproved, CurrentApproverType: bpm.ApproverUser, CurrentApproverID: "someone-else",
	}
	instRepo := &fakeApprovalInstRepo{inst: inst}
	procRepo := &fakeApprovalProcRepo{}

	t.Run("record visible → detail returned", func(t *testing.T) {
		svc := NewApprovalService(procRepo, instRepo, &fakeApprovalDecider{}).
			WithRequestFlow(nil, &fakeApprovalRecordReader{})
		got, err := svc.GetApprovalDetail(context.Background(),
			RecordScope{UserID: "submitter", AccessLevel: "owner"}, "t1", "inst-1", bpm.Actor{UserID: "submitter"})
		if err != nil {
			t.Fatalf("record viewer denied their own submission's detail: %v", err)
		}
		if got.ID != "inst-1" {
			t.Errorf("detail = %+v, want inst-1", got)
		}
	})

	t.Run("record NOT visible → 404, no oracle", func(t *testing.T) {
		svc := NewApprovalService(procRepo, instRepo, &fakeApprovalDecider{}).
			WithRequestFlow(nil, &fakeApprovalRecordReader{err: ErrRecordNotFound})
		_, err := svc.GetApprovalDetail(context.Background(),
			RecordScope{UserID: "stranger", AccessLevel: "owner"}, "t1", "inst-1", bpm.Actor{UserID: "stranger"})
		if err == nil {
			t.Fatal("a stranger who cannot see the record read the approval detail")
		}
	})
}

// TestGetApprovalDetail_ResolvesScopeBeforeProbing is the regression guard for a fix that did not
// work in production — worth pinning precisely because it LOOKED right.
//
// WAVE 27 added a "you can see the record, so you can see its approval detail" fallback so the
// submitter would stop 404ing on their own submission. But it probed with the scope that ARRIVED,
// and /approvals/:id is a flat route with no :entity segment, so the ACL stage never runs and
// scopeOf(c) yields AccessNone for every non-admin. The probe therefore asked "can you see this row"
// while holding no access at all, always answered no, and the branch was dead code: the reported
// bug was never actually fixed. Same shape as the files bug — when the route cannot carry the ACL
// stage, the SERVICE must re-run it.
func TestGetApprovalDetail_ResolvesScopeBeforeProbing(t *testing.T) {
	inst := &bpm.Instance{
		ID: "ai-1", TenantID: "t1", EntityName: "crm_quotes", RecordID: "rec-1",
		Status: bpm.StatusApproved, // terminal → awaitsActor says no, so the fallback decides
	}
	// The submitter: not the current approver, not an admin.
	actor := bpm.Actor{UserID: "submitter-1"}
	// What the flat route really hands over.
	routeScope := RecordScope{UserID: "submitter-1", RoleID: "r1", AccessLevel: string(dynrepo.AccessNone)}

	t.Run("ACL grants owner → probe runs at OWNER, detail returned", func(t *testing.T) {
		rec := &fakeApprovalRecordReader{authzLevel: "owner"}
		svc := NewApprovalService(&fakeApprovalProcessRepo{},
			&fakeApprovalInstRepo{inst: inst}, &fakeApprovalDecider{}).
			WithRequestFlow(nil, rec)

		got, err := svc.GetApprovalDetail(context.Background(), routeScope, "t1", "ai-1", actor)
		if err != nil {
			t.Fatalf("submitter still cannot open their own approval: %v", err)
		}
		if got == nil || got.ID != "ai-1" {
			t.Fatalf("detail = %+v", got)
		}
		if rec.sawLevel != "owner" {
			t.Errorf("record probe ran at %q; want \"owner\" — the RESOLVED level, not the route's AccessNone", rec.sawLevel)
		}
	})

	t.Run("ACL refuses → still NOT_FOUND, no oracle", func(t *testing.T) {
		rec := &fakeApprovalRecordReader{authzErr: errors.New("forbidden")}
		svc := NewApprovalService(&fakeApprovalProcessRepo{},
			&fakeApprovalInstRepo{inst: inst}, &fakeApprovalDecider{}).
			WithRequestFlow(nil, rec)

		_, err := svc.GetApprovalDetail(context.Background(), routeScope, "t1", "ai-1", actor)
		if codeOf(err) != "APPROVAL_INSTANCE_NOT_FOUND" {
			t.Fatalf("err = %v; want APPROVAL_INSTANCE_NOT_FOUND (same answer as unknown id)", err)
		}
	})
}
