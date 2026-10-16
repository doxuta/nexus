package services

// specs/064 S9-7 — RecordService.PurgeSample: the 056 purge in "root is a sample row" mode. It must
// reuse Purge's whole tail (blobs after commit, hard_delete trail, refusal mapping) and differ only
// in the repository parameters.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
)

func TestRecordPurgeSample_NonAdminRefusedBeforeAnyQuery(t *testing.T) {
	store := &purgeStore{res: purgeFixtureResult()}
	svc, aud, calls := purgeSvc(t, store, 2, nil)

	_, err := svc.PurgeSample(context.Background(), nonAdminScope(), "tenant-1", "customers", "rec-1", nil)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "FORBIDDEN" {
		t.Fatalf("err = %v; want FORBIDDEN", err)
	}
	if len(store.got) != 0 || len(aud.events) != 0 || len(*calls) != 0 {
		t.Fatalf("a refused purge reached store=%d audit=%d blobs=%d", len(store.got), len(aud.events), len(*calls))
	}
}

func TestRecordPurgeSample_RunsSampleRootAndReusesTheTail(t *testing.T) {
	store := &purgeStore{res: purgeFixtureResult()}
	svc, aud, calls := purgeSvc(t, store, 2, nil)
	pinned := map[string]dynrepo.PurgePin{dynrepo.PinKey("customers", "rec-9"): {Entity: "orders", Field: "customer"}}

	rep, err := svc.PurgeSample(context.Background(), operatorScope(), "tenant-1", "customers", "rec-1", pinned)
	if err != nil {
		t.Fatalf("PurgeSample: %v", err)
	}
	if len(store.got) != 1 {
		t.Fatalf("store calls = %d; want 1", len(store.got))
	}
	p := store.got[0]
	if !p.SampleRoot || p.DryRun || p.TenantID != "tenant-1" || p.ID != "rec-1" || len(p.Pinned) != 1 {
		t.Fatalf("params = %+v; want a real SampleRoot purge of rec-1 with the pinned set", p)
	}
	if rep.DryRun || rep.BlobsRemoved != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if len(*calls) != 1 || strings.Join((*calls)[0].ids, ",") != "f-1,f-2" {
		t.Fatalf("blob calls = %+v; the bytes go after the commit, through the shared remover", *calls)
	}
	if len(aud.events) != 1 || aud.events[0].Action != audit.ActionHardDelete {
		t.Fatalf("audit = %+v; want the hard_delete row of the purge trail", aud.events)
	}
}

func TestRecordPurgeSample_BlockedKeepsTheKindOfTheBlocker(t *testing.T) {
	store := &purgeStore{err: &dynrepo.PurgeBlockedError{Blocked: []dynrepo.PurgeBlocker{
		{Entity: "orders", Field: "customer", Count: 1, Kind: dynrepo.BlockerFieldDefault}}}}
	svc, aud, calls := purgeSvc(t, store, 0, nil)

	_, err := svc.PurgeSample(context.Background(), operatorScope(), "tenant-1", "customers", "rec-1", nil)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "RECORD_PURGE_LIVE_CHILDREN" {
		t.Fatalf("err = %v; want RECORD_PURGE_LIVE_CHILDREN", err)
	}
	blockers, _ := ve.Details["blockers"].([]dynrepo.PurgeBlocker)
	if len(blockers) != 1 || blockers[0].Kind != dynrepo.BlockerFieldDefault {
		t.Fatalf("details = %+v; the wipe reads Kind from here", ve.Details)
	}
	if len(aud.events) != 0 || len(*calls) != 0 {
		t.Fatal("a refused purge wrote a trail or deleted blobs")
	}
}
