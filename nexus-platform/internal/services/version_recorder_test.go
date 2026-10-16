package services

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"nexus/platform/internal/metaversion"
)

type fakeDirectStore struct {
	delivered bool
	appendErr error
	versions  []metaversion.Version
	docs      [][]metaversion.VersionDoc
}

func (f *fakeDirectStore) Delivered(context.Context, string) (bool, error) { return f.delivered, nil }
func (f *fakeDirectStore) ActorLabel(context.Context, string, string) string {
	return "Quản trị Khách"
}
func (f *fakeDirectStore) Append(_ context.Context, v metaversion.Version, d []metaversion.VersionDoc) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	f.versions = append(f.versions, v)
	f.docs = append(f.docs, d)
	return nil
}

func callerCtx() context.Context {
	ctx := metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: "u1", TenantID: "t1", HomeTenantID: "t1"})
	return metaversion.WithWarnings(ctx)
}

func TestVersionRecorder_RecordDirect(t *testing.T) {
	doc := DirectDoc{DocType: "role", DocKey: "r1", Label: "Nhân viên", Op: "update", Before: map[string]any{"a": 1}, After: map[string]any{"a": 2}}
	tests := []struct {
		name      string
		store     *fakeDirectStore
		ctx       context.Context
		wantRows  int
		wantWarn  bool
		wantLogEr bool
	}{
		{"not delivered: nothing written", &fakeDirectStore{delivered: false}, callerCtx(), 0, false, false},
		{"delivered: one direct entry", &fakeDirectStore{delivered: true}, callerCtx(), 1, false, false},
		{"no caller (system path): nothing written", &fakeDirectStore{delivered: true}, metaversion.WithWarnings(context.Background()), 0, false, false},
		{"write error: change stays, warning + zap.Error", &fakeDirectStore{delivered: true, appendErr: errors.New("boom")}, callerCtx(), 0, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.ErrorLevel)
			r := &VersionRecorder{store: tc.store, logger: zap.New(core)}
			r.RecordDirect(tc.ctx, "t1", "", doc)
			if len(tc.store.versions) != tc.wantRows {
				t.Fatalf("rows = %d, want %d", len(tc.store.versions), tc.wantRows)
			}
			if got := len(metaversion.Warnings(tc.ctx)) > 0; got != tc.wantWarn {
				t.Errorf("warning = %v, want %v", got, tc.wantWarn)
			}
			if got := logs.Len() > 0; got != tc.wantLogEr {
				t.Errorf("error log = %v, want %v", got, tc.wantLogEr)
			}
			if tc.wantRows == 1 {
				v := tc.store.versions[0]
				if v.Kind != metaversion.KindDirect || v.VersionNo != nil || v.CreatedBy != "u1" || v.CreatedByLabel != "Quản trị Khách" || v.DocCount != 1 {
					t.Errorf("version = %+v", v)
				}
				d := tc.store.docs[0][0]
				if d.DocType != "role" || string(d.BeforeJSON) != `{"a":1}` || string(d.AfterJSON) != `{"a":2}` {
					t.Errorf("doc = %+v", d)
				}
			}
		})
	}
}

func TestVersionRecorder_NilIsNoop(t *testing.T) {
	var r *VersionRecorder
	r.RecordDirect(callerCtx(), "t1", "", DirectDoc{DocType: "role"})
	if r.Active(callerCtx(), "t1") {
		t.Fatal("nil recorder must be inactive")
	}
}
