package services

// Issue #2, verifier round 1 (gap 1): a table born after the last publish was hidden from the metadata
// reads only. Once a grant on it landed (the assistant's access card applies at once), the rep could
// GET /zz_hidden (200) and POST it (201) before any publish. The record path now answers as if the
// table did not exist, with the decision the metadata reads take.

import (
	"context"
	"errors"
	"testing"
	"time"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

type fakeSchemaCutoff struct {
	at    time.Time
	apply bool
	err   error
}

func (f fakeSchemaCutoff) SchemaCutoff(context.Context, string) (time.Time, bool, error) {
	return f.at, f.apply, f.err
}

func TestRecordResolve_TableBornAfterThePublishIsAbsentForTheCustomer(t *testing.T) {
	cut := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	svc := func(born time.Time, c fakeSchemaCutoff) *RecordService {
		return NewRecordService(&fakeRecordCatalog{entity: &metadata.EntityDefinition{ID: "e1", TenantID: "t1", Name: "zz_new", CreatedAt: born}}, &fakeRecordStore{}).
			WithSchemaCutoff(c)
	}
	delivered := fakeSchemaCutoff{at: cut, apply: true}
	rep := metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: "u-rep", TenantID: "t1"})
	builder := metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: "u-b", TenantID: "t1", IsBuilder: true})
	for _, tc := range []struct {
		name   string
		svc    *RecordService
		ctx    context.Context
		hidden bool
	}{
		{"the rep, a table born after the publish", svc(cut.Add(time.Second), delivered), rep, true},
		{"the rep, a table born on the publish second", svc(cut, delivered), rep, false},
		{"the rep, a draft-overlay read or a workspace never delivered", svc(cut.Add(time.Second), fakeSchemaCutoff{}), rep, false},
		{"the builder fills the new table before publishing it", svc(cut.Add(time.Second), delivered), builder, false},
		{"no request caller (an automation, an import job)", svc(cut.Add(time.Second), delivered), context.Background(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := tc.svc.resolve(tc.ctx, "t1", "zz_new")
			if got := errors.Is(err, ErrEntityNotFound); got != tc.hidden {
				t.Fatalf("resolve err = %v; want hidden (ErrEntityNotFound) = %v", err, tc.hidden)
			}
			if !tc.hidden && err != nil {
				t.Fatalf("resolve err = %v; want the table", err)
			}
		})
	}
	// A cutoff that cannot be read refuses the request (fail closed), never serves the table.
	_, _, err := svc(cut.Add(time.Second), fakeSchemaCutoff{err: errors.New("versions down")}).resolve(rep, "t1", "zz_new")
	if err == nil || errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("resolve with an unreadable cutoff = %v; want a server error", err)
	}
}
