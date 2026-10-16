package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// fakeSampleStore serves canned counts per (tenant, entity) and records the tenant it was asked for.
type fakeSampleStore struct {
	counts  map[string]dynrepo.SampleCounts // key tenant|entity
	names   map[string][]string             // tenant -> entities with marks
	gotTens []string
}

func (f *fakeSampleStore) CountSampleRecords(_ context.Context, tenantID, entity string) (dynrepo.SampleCounts, error) {
	f.gotTens = append(f.gotTens, tenantID)
	return f.counts[tenantID+"|"+entity], nil
}
func (f *fakeSampleStore) SampleEntityNames(_ context.Context, tenantID string) ([]string, error) {
	f.gotTens = append(f.gotTens, tenantID)
	return f.names[tenantID], nil
}

func (f *fakeSampleStore) SampleRoots(context.Context, string, string) ([]dynrepo.SampleRootRow, error) {
	return nil, nil
}
func (f *fakeSampleStore) ResetSequenceIfEmpty(context.Context, string, string, string) (bool, error) {
	return false, nil
}

func (f *fakeSampleStore) LiveRecordIDs(_ context.Context, _, _ string, _ int) ([]string, error) {
	return nil, nil
}
func (f *fakeSampleStore) ActiveUserIDs(_ context.Context, _ string, _ int) ([]string, error) {
	return nil, nil
}

// fakeSampleCatalog knows the live entities of a tenant; anything else is not found.
type fakeSampleCatalog map[string]string // tenant|name -> label

func (c fakeSampleCatalog) GetEntityByName(_ context.Context, tenantID, name string) (*metadata.EntityDefinition, error) {
	if label, ok := c[tenantID+"|"+name]; ok {
		return &metadata.EntityDefinition{ID: "id-" + name, TenantID: tenantID, Name: name, Label: label}, nil
	}
	return nil, metadata.ErrEntityNotFound
}

func (c fakeSampleCatalog) ListFields(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	return nil, nil
}

// fakeSampleWriter stands in for RecordService.CreateBulk; it records the ctx mark it received.
type fakeSampleWriter struct {
	err      error
	gotMark  dynrepo.SampleMark
	gotOK    bool
	gotMode  BulkMode
	gotRows  int
	onCreate func()
}

func (w *fakeSampleWriter) CreateBulk(ctx context.Context, _ RecordScope, _, _, _ string, rows []map[string]any, mode BulkMode) (BulkCreateResult, error) {
	w.gotMark, w.gotOK = dynrepo.SampleMarkFrom(ctx)
	w.gotMode, w.gotRows = mode, len(rows)
	if w.onCreate != nil {
		w.onCreate()
	}
	if w.err != nil {
		return BulkCreateResult{}, w.err
	}
	return BulkCreateResult{Mode: string(mode), Created: len(rows)}, nil
}

func TestSampleDataService_Summary(t *testing.T) {
	store := &fakeSampleStore{
		counts: map[string]dynrepo.SampleCounts{
			"t1|crm_accounts": {Live: 3, InBin: 1, BySource: map[string]int{"ai": 3, "generator": 1}},
			"t2|crm_accounts": {Live: 9, BySource: map[string]int{"ai": 9}},
		},
		names: map[string][]string{"t1": {"crm_accounts", "gone_entity"}, "t2": {"crm_accounts"}},
	}
	cat := fakeSampleCatalog{"t1|crm_accounts": "Khách hàng", "t2|crm_accounts": "Khách hàng"}
	svc := NewSampleDataService(store, cat, &fakeSampleWriter{})

	t.Run("default scope = tenant marks; entity gone from metadata is skipped, no error", func(t *testing.T) {
		got, err := svc.Summary(context.Background(), "t1", nil)
		if err != nil {
			t.Fatalf("Summary: %v", err)
		}
		if got.Total != 4 || len(got.Entities) != 1 {
			t.Fatalf("summary = %+v, want total 4 over 1 live entity", got)
		}
		e := got.Entities[0]
		if e.Entity != "crm_accounts" || e.Label != "Khách hàng" || e.Count != 4 || e.Live != 3 || e.InBin != 1 ||
			e.BySource.AI != 3 || e.BySource.Generator != 1 || e.BySource.Pack != 0 {
			t.Fatalf("entity row = %+v", e)
		}
	})
	t.Run("another tenant's counts never leak", func(t *testing.T) {
		got, err := svc.Summary(context.Background(), "t2", []string{"crm_accounts"})
		if err != nil || got.Total != 9 {
			t.Fatalf("t2 summary = %+v err=%v, want total 9", got, err)
		}
		for _, tn := range store.gotTens[len(store.gotTens)-1:] {
			if tn != "t2" {
				t.Fatalf("store asked for tenant %q, want t2", tn)
			}
		}
	})
	t.Run("an explicitly requested unknown entity is SAMPLE_ENTITY_NOT_FOUND", func(t *testing.T) {
		_, err := svc.Summary(context.Background(), "t1", []string{"nope"})
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "SAMPLE_ENTITY_NOT_FOUND" {
			t.Fatalf("err = %v, want SAMPLE_ENTITY_NOT_FOUND", err)
		}
	})
}

func TestSampleDataService_CreateRecords(t *testing.T) {
	scope := RecordScope{UserID: "u1", AccessLevel: "all", IsAdmin: true}
	rows := []map[string]any{{"name": "A"}, {"name": "B"}}
	cat := fakeSampleCatalog{"t1|crm_accounts": "Khách hàng"}

	t.Run("marks ctx source=ai with a batch id, audits one __sample_data__ create", func(t *testing.T) {
		w, aud := &fakeSampleWriter{}, &spyAuditor{}
		svc := NewSampleDataService(&fakeSampleStore{}, cat, w).WithAuditor(aud)
		res, err := svc.CreateRecords(context.Background(), scope, "t1", "u1", "crm_accounts", rows, BulkModeAtomic)
		if err != nil || res.Created != 2 {
			t.Fatalf("CreateRecords = %+v, %v", res, err)
		}
		if !w.gotOK || w.gotMark.Source != dynrepo.SampleSourceAI || w.gotMark.BatchID == "" || w.gotMode != BulkModeAtomic {
			t.Fatalf("writer saw mark=%+v ok=%v mode=%s; want source ai, a batch id, atomic", w.gotMark, w.gotOK, w.gotMode)
		}
		if len(aud.events) != 1 || aud.events[0].EntityName != audit.SampleDataMeta || aud.events[0].Action != audit.ActionCreate ||
			aud.events[0].RecordID != w.gotMark.BatchID || aud.events[0].TenantID != "t1" || aud.events[0].ActorUserID != "u1" {
			t.Fatalf("audit = %+v", aud.events)
		}
	})
	t.Run("unknown entity is SAMPLE_ENTITY_NOT_FOUND and the writer is never called", func(t *testing.T) {
		w := &fakeSampleWriter{}
		svc := NewSampleDataService(&fakeSampleStore{}, cat, w)
		_, err := svc.CreateRecords(context.Background(), scope, "t1", "u1", "nope", rows, BulkModeAtomic)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "SAMPLE_ENTITY_NOT_FOUND" || w.gotRows != 0 {
			t.Fatalf("err = %v rows=%d, want SAMPLE_ENTITY_NOT_FOUND and no write", err, w.gotRows)
		}
	})
	t.Run("a second call for the same tenant while one runs is SAMPLE_BUSY; other tenant is free; lock released after", func(t *testing.T) {
		w := &fakeSampleWriter{}
		catBoth := fakeSampleCatalog{"t1|crm_accounts": "KH", "t2|crm_accounts": "KH"}
		svc := NewSampleDataService(&fakeSampleStore{}, catBoth, w)
		w.onCreate = func() {
			_, err := svc.CreateRecords(context.Background(), scope, "t1", "u1", "crm_accounts", rows, BulkModeAtomic)
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != "SAMPLE_BUSY" {
				t.Errorf("re-entrant same tenant err = %v, want SAMPLE_BUSY", err)
			}
			w.onCreate = nil
			if _, err := svc.CreateRecords(context.Background(), scope, "t2", "u1", "crm_accounts", rows, BulkModeAtomic); err != nil {
				t.Errorf("other tenant err = %v, want nil", err)
			}
		}
		if _, err := svc.CreateRecords(context.Background(), scope, "t1", "u1", "crm_accounts", rows, BulkModeAtomic); err != nil {
			t.Fatalf("first call: %v", err)
		}
		if _, err := svc.CreateRecords(context.Background(), scope, "t1", "u1", "crm_accounts", rows, BulkModeAtomic); err != nil {
			t.Fatalf("lock not released after the first call: %v", err)
		}
	})
	t.Run("writer error is returned and the lock is released", func(t *testing.T) {
		w := &fakeSampleWriter{err: errors.New("boom")}
		svc := NewSampleDataService(&fakeSampleStore{}, cat, w)
		if _, err := svc.CreateRecords(context.Background(), scope, "t1", "u1", "crm_accounts", rows, BulkModeAtomic); err == nil {
			t.Fatal("want the writer error")
		}
		w.err = nil
		if _, err := svc.CreateRecords(context.Background(), scope, "t1", "u1", "crm_accounts", rows, BulkModeAtomic); err != nil {
			t.Fatalf("lock leaked after an error: %v", err)
		}
	})
}
