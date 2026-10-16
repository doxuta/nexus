package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/metaversion"
)

type fakeDelivered struct {
	delivered bool
	calls     int
	err       error
}

func (f *fakeDelivered) HasNumbered(context.Context, string) (bool, error) {
	f.calls++
	return f.delivered, f.err
}

func TestDraftGate_Delivered_CachesOnlyTrue(t *testing.T) {
	f := &fakeDelivered{delivered: true}
	g := NewDraftGate(f)
	for i := 0; i < 3; i++ {
		if ok, err := g.Delivered(context.Background(), "t1"); err != nil || !ok {
			t.Fatalf("Delivered = %v, %v", ok, err)
		}
	}
	if f.calls != 1 {
		t.Errorf("true answer asked %d times, want 1 (cached for the process)", f.calls)
	}

	f2 := &fakeDelivered{delivered: false}
	g2 := NewDraftGate(f2)
	for i := 0; i < 3; i++ {
		if ok, _ := g2.Delivered(context.Background(), "t1"); ok {
			t.Fatal("not delivered reported delivered")
		}
	}
	if f2.calls != 3 {
		t.Errorf("false answer asked %d times, want 3 (another replica may deliver the tenant)", f2.calls)
	}

	g3 := NewDraftGate(&fakeDelivered{err: errors.New("db down")})
	if _, err := g3.Delivered(context.Background(), "t1"); err == nil {
		t.Fatal("a DB error must surface, not read as 'not delivered'")
	}
}

func TestDraftGate_WriteMode(t *testing.T) {
	builder := metaversion.Caller{UserID: "b", TenantID: "t1", HomeTenantID: "t1", IsBuilder: true}
	legacyAdmin := metaversion.Caller{UserID: "a", TenantID: "t1", HomeTenantID: "t1"}
	with := func(c metaversion.Caller) context.Context { return metaversion.WithCaller(context.Background(), c) }
	tests := []struct {
		name      string
		delivered bool
		ctx       context.Context
		tenant    string
		want      metaversion.Mode
		wantErr   error
	}{
		{"not delivered, builder: straight live (today's behaviour)", false, with(builder), "t1", metaversion.ModeLive, nil},
		{"not delivered, no caller: live, behaviour unchanged", false, context.Background(), "t1", metaversion.ModeLive, nil},
		{"delivered builder on own tenant: draft", true, with(builder), "t1", metaversion.ModeDraft, nil},
		{"delivered legacy admin: live direct", true, with(legacyAdmin), "t1", metaversion.ModeLiveDirect, nil},
		{"delivered, publish apply: live", true, metaversion.WithPublishApply(with(builder)), "t1", metaversion.ModeLive, nil},
		{"delivered, publish apply with no caller: live", true, metaversion.WithPublishApply(context.Background()), "t1", metaversion.ModeLive, nil},
		{"delivered, NO caller: fails closed", true, context.Background(), "t1", metaversion.ModeLive, metaversion.ErrNoCaller},
		{"delivered, other tenant than the caller's: fails closed", true, with(builder), "t2", metaversion.ModeLive, metaversion.ErrCrossTenantDraftWrite},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewDraftGate(&fakeDelivered{delivered: tc.delivered})
			got, err := g.WriteMode(tc.ctx, tc.tenant)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && got != tc.want {
				t.Fatalf("mode = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDraftGate_ReadOverlay(t *testing.T) {
	reader := metaversion.Caller{UserID: "b", TenantID: "t1", IsBuilder: true, ReadDraft: true}
	tests := []struct {
		name      string
		ctx       context.Context
		tenant    string
		delivered bool
		want      bool
		wantCalls int
	}{
		{"asked + delivered + own tenant", metaversion.WithCaller(context.Background(), reader), "t1", true, true, 1},
		{"no caller: live, zero queries", context.Background(), "t1", true, false, 0},
		{"ReadDraft=false (customer hot path): zero queries", metaversion.WithCaller(context.Background(), metaversion.Caller{TenantID: "t1"}), "t1", true, false, 0},
		{"WithLiveRead overrides", metaversion.WithLiveRead(metaversion.WithCaller(context.Background(), reader)), "t1", true, false, 0},
		{"other tenant (?targetTenant=) reads live", metaversion.WithCaller(context.Background(), reader), "t2", true, false, 0},
		{"not delivered: nothing to overlay", metaversion.WithCaller(context.Background(), reader), "t1", false, false, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeDelivered{delivered: tc.delivered}
			got, err := NewDraftGate(f).ReadOverlay(tc.ctx, tc.tenant)
			if err != nil || got != tc.want || f.calls != tc.wantCalls {
				t.Fatalf("ReadOverlay = %v, %v with %d queries; want %v with %d", got, err, f.calls, tc.want, tc.wantCalls)
			}
		})
	}
}

func TestDraftGate_CacheDelivered(t *testing.T) {
	f := &fakeDelivered{}
	g := NewDraftGate(f)
	g.CacheDelivered("t1")
	if ok, _ := g.Delivered(context.Background(), "t1"); !ok || f.calls != 0 {
		t.Fatalf("CacheDelivered not honoured: ok=%v calls=%d", ok, f.calls)
	}
}
