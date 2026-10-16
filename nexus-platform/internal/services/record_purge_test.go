package services

// specs/056-record-hard-delete — RecordService.Purge at the service seam: who may call it, what a
// dry run is allowed to touch (nothing), what a real purge leaves behind (one hard_delete audit row
// per removed record, sensitive values masked; the blobs), and how the repository's refusals map.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// purgeStore is a RecordStore that also implements the purge seam (auto-wired like userExister).
type purgeStore struct {
	fakeRecordStore
	got []dynrepo.PurgeParams
	res *dynrepo.PurgeResult
	err error
}

func (s *purgeStore) PurgeRecord(_ context.Context, p dynrepo.PurgeParams) (*dynrepo.PurgeResult, error) {
	s.got = append(s.got, p)
	if s.err != nil {
		return nil, s.err
	}
	return s.res, nil
}

// blobCall records one call of the blob-remover port.
type blobCall struct {
	tenant string
	ids    []string
}

func purgeFixtureResult() *dynrepo.PurgeResult {
	return &dynrepo.PurgeResult{
		Records: []dynrepo.PurgedRecord{{Entity: "customers", ID: "rec-1", Data: map[string]any{"name": "Trần Văn A", "so_cccd": "001099001122"}}},
		FileIDs: []string{"f-1", "f-2"},
		Tables:  []dynrepo.PurgeTableCount{{Table: "sys_files", Rows: 2}, {Table: "obj_customers", Rows: 1}},
	}
}

func operatorScope() RecordScope { return RecordScope{UserID: "op-1", IsAdmin: true} }

// noRelationships: an entity nothing points at (the #46 cases).
type noRelationships struct{}

func (noRelationships) ListRelationshipsForEntity(context.Context, string, string) ([]metadata.RelationshipDefinition, error) {
	return nil, nil
}

func (noRelationships) ListEntities(string) ([]metadata.EntityDefinition, error) { return nil, nil }

func purgeSvc(t *testing.T, store RecordStore, removed int, blobErr error) (*RecordService, *captureAuditor, *[]blobCall) {
	t.Helper()
	aud := &captureAuditor{}
	calls := &[]blobCall{}
	svc := NewRecordService(sensitiveFixture(t), store).WithAuditor(aud).WithRelationshipLister(noRelationships{}).
		WithBlobRemover(func(_ context.Context, tenantID string, ids []string) (int, error) {
			*calls = append(*calls, blobCall{tenantID, ids})
			return removed, blobErr
		})
	return svc, aud, calls
}

func TestRecordPurge_NonAdminRefusedBeforeAnyQuery(t *testing.T) {
	store := &purgeStore{res: purgeFixtureResult()}
	svc, aud, calls := purgeSvc(t, store, 2, nil)

	_, err := svc.Purge(context.Background(), nonAdminScope(), "tenant-1", "customers", "rec-1", false)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "FORBIDDEN" {
		t.Fatalf("non-admin purge: err = %v; want ValidationError FORBIDDEN", err)
	}
	if len(store.got) != 0 || len(aud.events) != 0 || len(*calls) != 0 {
		t.Fatalf("a refused purge must not reach the store/auditor/blobs: store=%d audit=%d blobs=%d", len(store.got), len(aud.events), len(*calls))
	}
}

func TestRecordPurge_DryRunTouchesNothing(t *testing.T) {
	store := &purgeStore{res: purgeFixtureResult()}
	svc, aud, calls := purgeSvc(t, store, 2, nil)

	rep, err := svc.Purge(context.Background(), operatorScope(), "tenant-1", "customers", "rec-1", true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(store.got) != 1 || !store.got[0].DryRun || store.got[0].Entity != "customers" || store.got[0].TenantID != "tenant-1" || store.got[0].ID != "rec-1" {
		t.Fatalf("store params = %+v; want one dry-run call for customers/rec-1 in tenant-1", store.got)
	}
	if !rep.DryRun || rep.TotalRows != 3 || len(rep.Records) != 1 || rep.Records[0].ID != "rec-1" {
		t.Fatalf("report = %+v; want dryRun, 3 rows, record rec-1", rep)
	}
	if rep.BlobsRemoved != 0 || !strings.Contains(rep.BlobsNote, "2 tệp") {
		t.Fatalf("dry run blobs: removed=%d note=%q; want 0 and a note announcing 2 files", rep.BlobsRemoved, rep.BlobsNote)
	}
	if len(aud.events) != 0 || len(*calls) != 0 {
		t.Fatalf("dry run wrote audit (%d) or deleted blobs (%d)", len(aud.events), len(*calls))
	}
}

func TestRecordPurge_CommitAuditsHardDeleteAndRemovesBlobs(t *testing.T) {
	store := &purgeStore{res: purgeFixtureResult()}
	svc, aud, calls := purgeSvc(t, store, 2, nil)

	rep, err := svc.Purge(context.Background(), operatorScope(), "tenant-1", "customers", "rec-1", false)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if rep.DryRun || rep.BlobsRemoved != 2 || rep.BlobsNote != "" {
		t.Fatalf("report = %+v; want a real purge with 2 blobs removed and no note", rep)
	}
	if len(*calls) != 1 || (*calls)[0].tenant != "tenant-1" || strings.Join((*calls)[0].ids, ",") != "f-1,f-2" {
		t.Fatalf("blob remover calls = %+v; want one call for tenant-1 with f-1,f-2", *calls)
	}
	evt := soleEvent(t, aud)
	if audit.ActionHardDelete != "hard_delete" || evt.Action != audit.ActionHardDelete {
		t.Fatalf("action = %q; want hard_delete", evt.Action)
	}
	if evt.TenantID != "tenant-1" || evt.ActorUserID != "op-1" || evt.EntityName != "customers" || evt.RecordID != "rec-1" {
		t.Fatalf("event = %+v; want tenant-1 / op-1 / customers / rec-1", evt)
	}
	if evt.Diff == nil || evt.Diff.After != nil || evt.Diff.Before["name"] != "Trần Văn A" {
		t.Fatalf("diff = %+v; want before-only with the record's values", evt.Diff)
	}
	wantMasked(t, evt.Diff.Before, "so_cccd", "001099001122")
}

func TestRecordPurge_BlobsTheReportMustNotHide(t *testing.T) {
	tests := []struct {
		name     string
		wire     bool
		removed  int
		blobErr  error
		wantNote string
	}{
		{"remover not wired", false, 0, nil, "CHƯA xoá 2 tệp"},
		{"storage fails half way", true, 1, errors.New("disk"), "xoá được 1/2 tệp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &purgeStore{res: purgeFixtureResult()}
			var svc *RecordService
			if tt.wire {
				svc, _, _ = purgeSvc(t, store, tt.removed, tt.blobErr)
			} else {
				svc = NewRecordService(sensitiveFixture(t), store).WithAuditor(&captureAuditor{}).WithRelationshipLister(noRelationships{})
			}
			rep, err := svc.Purge(context.Background(), operatorScope(), "tenant-1", "customers", "rec-1", false)
			if err != nil {
				t.Fatalf("a committed purge must not fail on storage: %v", err)
			}
			if !strings.Contains(rep.BlobsNote, tt.wantNote) {
				t.Fatalf("blobsNote = %q; want it to say %q", rep.BlobsNote, tt.wantNote)
			}
		})
	}
}

func TestRecordPurge_RepositoryRefusalsMap(t *testing.T) {
	tests := []struct {
		name     string
		storeErr error
		wantCode string // ValidationError code, "" = sentinel passthrough
		wantIs   error
	}{
		{"live record", dynrepo.ErrRecordNotInBin, "RECORD_NOT_IN_RECYCLE_BIN", nil},
		{"unknown / other tenant", dynrepo.ErrRecordNotFound, "", ErrRecordNotFound},
		{"deadlock", dynrepo.ErrWriteConflict, "", ErrWriteConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &purgeStore{err: tt.storeErr}
			svc, aud, calls := purgeSvc(t, store, 0, nil)
			_, err := svc.Purge(context.Background(), operatorScope(), "tenant-1", "customers", "rec-1", false)
			if tt.wantCode != "" {
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Code != tt.wantCode {
					t.Fatalf("err = %v; want ValidationError %s", err, tt.wantCode)
				}
			} else if !errors.Is(err, tt.wantIs) {
				t.Fatalf("err = %v; want errors.Is %v", err, tt.wantIs)
			}
			if len(aud.events) != 0 || len(*calls) != 0 {
				t.Fatalf("a refused purge audited (%d) or touched blobs (%d)", len(aud.events), len(*calls))
			}
		})
	}
}

// A store without the purge seam (a test fake, a future store) must refuse, never panic and never
// report a purge that did not happen. Same for a service that cannot read the relationship graph:
// purging blind would leave required children pointing at nothing (specs/056 US2).
func TestRecordPurge_MissingSeamFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		svc  *RecordService
		st   *purgeStore
	}{
		{"no purger", NewRecordService(sensitiveFixture(t), &fakeRecordStore{}).WithRelationshipLister(noRelationships{}), nil},
		{"no relationship lister", nil, &purgeStore{res: purgeFixtureResult()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := tt.svc
			if svc == nil {
				svc = NewRecordService(sensitiveFixture(t), tt.st)
			}
			rep, err := svc.Purge(context.Background(), operatorScope(), "tenant-1", "customers", "rec-1", true)
			if err == nil || rep != nil {
				t.Fatalf("rep=%+v err=%v; want an error and no report", rep, err)
			}
			if tt.st != nil && len(tt.st.got) != 0 {
				t.Fatalf("store reached without a graph: %+v", tt.st.got)
			}
		})
	}
}
