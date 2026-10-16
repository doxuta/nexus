package services

// preview_draft_test.go — specs/064-preview-as-role-sample-data S9-9 (T130): the "viewing the draft"
// flag of a preview session. Start decides it (default on when delivered + drafts pending), the row
// stores it, the token carries it (pdr), Verify rebuilds it from the ROW.

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/metaversion"
)

type fakeDraftDelivered struct {
	delivered bool
	err       error
	asked     []string
}

func (f *fakeDraftDelivered) Delivered(_ context.Context, tenantID string) (bool, error) {
	f.asked = append(f.asked, tenantID)
	return f.delivered, f.err
}

type fakeDraftCount struct {
	n     int
	err   error
	asked []string
}

func (f *fakeDraftCount) Count(_ context.Context, tenantID string) (int, error) {
	f.asked = append(f.asked, tenantID)
	return f.n, f.err
}

// draftFixture wires the draft probe: delivered flag + pending drafts count.
func draftFixture(t *testing.T, delivered bool, pending int) (*previewFixture, *fakeDraftDelivered, *fakeDraftCount) {
	t.Helper()
	f := newPreviewFixture(t)
	d, c := &fakeDraftDelivered{delivered: delivered}, &fakeDraftCount{n: pending}
	f.svc.WithDrafts(d, c)
	return f, d, c
}

func TestPreviewStart_DraftFlag(t *testing.T) {
	cases := []struct {
		name      string
		delivered bool
		pending   int
		ask       *bool
		want      bool
		wantCode  string
	}{
		{"default: delivered + has drafts => on", true, 2, nil, true, ""},
		{"default: delivered, no drafts => off", true, 0, nil, false, ""},
		{"default: not delivered => off", false, 0, nil, false, ""},
		{"default: not delivered, stray drafts => off", false, 3, nil, false, ""},
		{"explicit false wins over available draft", true, 2, boolPtr(false), false, ""},
		{"explicit true + delivered + drafts => on", true, 1, boolPtr(true), true, ""},
		{"explicit true, not delivered => 422", false, 0, boolPtr(true), false, "PREVIEW_DRAFT_UNAVAILABLE"},
		{"explicit true, delivered but no drafts => 422", true, 0, boolPtr(true), false, "PREVIEW_DRAFT_UNAVAILABLE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _, _ := draftFixture(t, tc.delivered, tc.pending)
			res, err := f.svc.Start(context.Background(), builderSubject(), PreviewStartInput{RoleID: pvRole, Draft: tc.ask})
			if tc.wantCode != "" {
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Code != tc.wantCode {
					t.Fatalf("err = %v, want ValidationError %s", err, tc.wantCode)
				}
				if f.store.created != 0 || len(f.auditor.events) != 0 {
					t.Errorf("refused Start wrote %d rows / %d audit events", f.store.created, len(f.auditor.events))
				}
				return
			}
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if res.Draft != tc.want {
				t.Errorf("result draft = %v, want %v", res.Draft, tc.want)
			}
			if row := f.store.rows[pvTenant+"|"+res.SessionID]; row == nil || row.Draft != tc.want {
				t.Errorf("row draft = %+v, want %v", row, tc.want)
			}
			if got := f.auditor.events[0].Diff.After["draft"]; got != tc.want {
				t.Errorf("audit draft = %v, want %v", got, tc.want)
			}
			// The token carries the flag (pdr); Verify hands it to the Subject from the ROW.
			tok, err := f.tokens.VerifyPreviewToken(res.Token)
			if err != nil || tok.PreviewDraft != tc.want {
				t.Errorf("token pdr = %v (err %v), want %v", tok != nil && tok.PreviewDraft, err, tc.want)
			}
			claims, err := f.svc.Verify(context.Background(), res.Token)
			if err != nil || claims.PreviewDraft != tc.want {
				t.Fatalf("Verify PreviewDraft = %v (err %v), want %v", claims != nil && claims.PreviewDraft, err, tc.want)
			}
			if claims.IsAdmin || claims.IsPlatformOperator {
				t.Error("a draft preview must stay non-admin, non-operator")
			}
		})
	}
}

// The draft question is asked of the BUILDER'S tenant only (the workspace being previewed).
func TestPreviewStart_DraftProbePinnedToTenant(t *testing.T) {
	f, d, c := draftFixture(t, true, 1)
	if _, err := f.svc.Start(context.Background(), builderSubject(), PreviewStartInput{RoleID: pvRole}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(d.asked) != 1 || d.asked[0] != pvTenant || len(c.asked) != 1 || c.asked[0] != pvTenant {
		t.Errorf("probe asked delivered=%v count=%v, want only [%s]", d.asked, c.asked, pvTenant)
	}
}

// A probe failure is a wrapped infrastructure error: never a silent "no draft" (explicit true) nor a
// silent live preview (default). Explicit false never needs the probe.
func TestPreviewStart_DraftProbeErrorSurfaces(t *testing.T) {
	for _, ask := range []*bool{nil, boolPtr(true)} {
		f, d, _ := draftFixture(t, true, 1)
		d.err = errors.New("db down")
		_, err := f.svc.Start(context.Background(), builderSubject(), PreviewStartInput{RoleID: pvRole, Draft: ask})
		var ve *ValidationError
		if err == nil || errors.As(err, &ve) {
			t.Fatalf("ask=%v: err = %v, want a wrapped infrastructure error", ask, err)
		}
		if f.store.created != 0 {
			t.Errorf("ask=%v: wrote %d rows despite the probe failing", ask, f.store.created)
		}
	}
	f, d, _ := draftFixture(t, true, 1)
	d.err = errors.New("db down")
	if _, err := f.svc.Start(context.Background(), builderSubject(), PreviewStartInput{RoleID: pvRole, Draft: boolPtr(false)}); err != nil {
		t.Errorf("explicit false must not depend on the probe: %v", err)
	}
}

// pdr in the token must agree with the row (the row is the authority).
func TestPreviewVerify_DraftClaimMustMatchRow(t *testing.T) {
	f, _, _ := draftFixture(t, true, 1)
	res, err := f.svc.Start(context.Background(), builderSubject(), PreviewStartInput{RoleID: pvRole, Draft: boolPtr(true)})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.store.rows[pvTenant+"|"+res.SessionID].Draft = false
	if _, err := f.svc.Verify(context.Background(), res.Token); !errors.Is(err, ErrPreviewUnauthorized) {
		t.Fatalf("Verify with pdr != row.draft: err = %v, want ErrPreviewUnauthorized", err)
	}
}

// A preview-draft Caller (non-builder, ReadDraft) reads the overlay of ITS tenant only, and a write
// is never turned into a draft write (PreviewReadOnly refuses it first; the gate must not rescue it).
func TestDraftGate_PreviewDraftCaller(t *testing.T) {
	prev := metaversion.Caller{UserID: "principal", TenantID: "t1", HomeTenantID: "home", ReadDraft: true}
	ctx := metaversion.WithCaller(context.Background(), prev)
	g := NewDraftGate(&fakeDelivered{delivered: true})
	if on, err := g.ReadOverlay(ctx, "t1"); err != nil || !on {
		t.Errorf("ReadOverlay(own tenant) = %v, %v; want true", on, err)
	}
	if on, _ := g.ReadOverlay(ctx, "t2"); on {
		t.Error("ReadOverlay(another tenant) must read live")
	}
	if m, err := g.WriteMode(ctx, "t1"); err != nil || m == metaversion.ModeDraft {
		t.Errorf("WriteMode = %v, %v; a preview caller must never write a draft", m, err)
	}
	off := metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: "principal", TenantID: "t1"})
	if on, _ := g.ReadOverlay(off, "t1"); on {
		t.Error("preview without pdr read the overlay")
	}
}
