package services

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
)

type fakeLive map[string]bool

func (f fakeLive) LiveUser(_ context.Context, _, id string) (bool, error) { return f[id], nil }

type fakeRecGet struct {
	approvalRecordReader // nil: only Get is live
	rec                  *dynrepo.Record
	err                  error
}

func (f fakeRecGet) Get(context.Context, RecordScope, string, string, string, []string, []string) (*dynrepo.Record, error) {
	return f.rec, f.err
}

func TestApprovalSubmitterResolver_ResolveSubmitter(t *testing.T) {
	for _, tc := range []struct {
		name string
		live fakeLive
		rec  fakeRecGet
		want string
	}{
		{"creator live", fakeLive{"c": true}, fakeRecGet{}, "c"},
		{"owner fallback", fakeLive{"o": true}, fakeRecGet{rec: &dynrepo.Record{OwnerID: "o"}}, "o"},
		{"record deleted, creator gone: nobody, not an error", fakeLive{}, fakeRecGet{err: ErrRecordNotFound}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewApprovalSubmitterResolver(tc.live, tc.rec).ResolveSubmitter(context.Background(), "t1", "e", "r", "c")
			if err != nil || got != tc.want {
				t.Fatalf("got %q err %v; want %q", got, err, tc.want)
			}
		})
	}
}
