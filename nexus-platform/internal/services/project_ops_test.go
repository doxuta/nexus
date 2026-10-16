package services

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"nexus/platform/internal/project"
)

type fakeOps struct {
	status string
	err    error
	asked  []string
}

func (f *fakeOps) Summary(_ context.Context, tenantID string) (HealthSummary, error) {
	f.asked = append(f.asked, tenantID)
	return HealthSummary{Status: f.status}, f.err
}

// specs/066 T138(b)+(c): HealthCheck stores health_json.ops, List returns it as health.ops, and the
// ops label changes neither health_status nor the stage.
func TestProjectHealthCheck_StoresOpsWithoutTouchingStatus(t *testing.T) {
	f := newPjFixture(t)
	ops := &fakeOps{status: "critical"}
	f.svc.WithOps(ops)
	h, err := f.svc.HealthCheck(context.Background(), f.actor(), pjA)
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" || h.Ops == nil || h.Ops.Status != "critical" {
		t.Fatalf("health = %+v ops %+v, want status ok and ops critical", h, h.Ops)
	}
	if len(ops.asked) != 1 || ops.asked[0] != pjA {
		t.Fatalf("Summary asked %v, want the target tenant only", ops.asked)
	}
	stored := f.repo.upserts[len(f.repo.upserts)-1]
	var doc struct {
		Ops *OpsHealth `json:"ops"`
	}
	if err := json.Unmarshal(stored.HealthJSON, &doc); err != nil || doc.Ops == nil || doc.Ops.Status != "critical" {
		t.Fatalf("stored health_json = %s (%v)", stored.HealthJSON, err)
	}

	f.repo.profiles[pjA] = stored
	got, err := f.svc.List(context.Background(), pjHome)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Health.Ops == nil || got[0].Health.Ops.Status != "critical" || got[0].Health.Status != "ok" {
		t.Fatalf("list health = %+v, want ops critical and status ok", got[0].Health)
	}
}

func TestProjectHealthCheck_OpsFailureKeepsDoctorSnapshot(t *testing.T) {
	f := newPjFixture(t)
	f.svc.WithOps(&fakeOps{err: errors.New("db down")})
	h, err := f.svc.HealthCheck(context.Background(), f.actor(), pjA)
	if err != nil || h.Ops != nil || h.Status != "ok" {
		t.Fatalf("health = %+v err %v, want snapshot stored with ops null", h, err)
	}
}

type fakeProfileGetter struct {
	p   *project.Profile
	err error
}

func (f fakeProfileGetter) Get(context.Context, string) (*project.Profile, error) { return f.p, f.err }

// T138(a): the Doctor level is derived from the counts; health_status and $.ops are never read.
func TestProjectDoctorSnapshot(t *testing.T) {
	at := time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC)
	warnStatus := "warn" // readiness folded in: must NOT show up as a Doctor level
	cases := []struct {
		name         string
		g            fakeProfileGetter
		wantNil      bool
		wantC, wantW int
		wantErr      bool
	}{
		{"no profile", fakeProfileGetter{err: project.ErrNotFound}, true, 0, 0, false},
		{"never checked", fakeProfileGetter{p: &project.Profile{}}, true, 0, 0, false},
		{"counts only", fakeProfileGetter{p: &project.Profile{HealthStatus: &warnStatus, HealthCheckedAt: &at,
			HealthJSON: json.RawMessage(`{"critical":0,"warn":0,"appsNotReady":[{"appId":"x"}],"ops":{"status":"critical"}}`)}}, false, 0, 0, false},
		{"critical", fakeProfileGetter{p: &project.Profile{HealthCheckedAt: &at, HealthJSON: json.RawMessage(`{"critical":2,"warn":1}`)}}, false, 2, 1, false},
		{"store error", fakeProfileGetter{err: errors.New("db down")}, true, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap, err := NewProjectDoctorSnapshot(tc.g).Latest(context.Background(), "t1")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if tc.wantNil {
				if snap != nil {
					t.Fatalf("snap = %+v, want nil", snap)
				}
				return
			}
			if snap == nil || snap.Critical != tc.wantC || snap.Warn != tc.wantW || snap.CheckedAt == nil {
				t.Fatalf("snap = %+v", snap)
			}
			if d := doctorOf(snap); tc.wantC == 0 && tc.wantW == 0 && d.Status != "ok" {
				t.Fatalf("doctor level = %q, want ok", d.Status)
			}
		})
	}
}
