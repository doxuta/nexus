package services

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// queue #358 — a unique field the maker did not pick as "chống trùng" is still probed, in the DB and
// in the file, so the preview names the colliding rows and the run does not refuse the whole batch.

func uniqueCodeCatalog() *fakeImportCatalog {
	return &fakeImportCatalog{
		entity: &metadata.EntityDefinition{ID: "units", Name: "units", Label: "Căn"},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text", IsRequired: true},
			{Name: "code", Label: "Mã căn", FieldType: "text", IsUnique: true},
		},
	}
}

func importUnits(t *testing.T, store *fakeImportStore, in ImportInput) *ImportResult {
	t.Helper()
	in.TenantID, in.UserID, in.EntityName = "t1", "u1", "units"
	in.Reader = strings.NewReader("Tên,Mã\nCăn 1,A1\nCăn 2,B2\nCăn 3,B2\nCăn 4,C3\n")
	in.Mapping = map[string]string{"Tên": "name", "Mã": "code"}
	res, err := NewImportService(uniqueCodeCatalog(), store, nil).Import(context.Background(), in)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	return res
}

func TestImport_UniqueFieldCollisionsNamedPerRow(t *testing.T) {
	for _, dry := range []bool{true, false} {
		store := &fakeImportStore{existingDedupVals: map[string]struct{}{"A1": {}}}
		res := importUnits(t, store, ImportInput{IsAdmin: true, DryRun: dry})
		if res.Valid != 2 || res.Failed != 2 || len(res.Errors) != 2 {
			t.Fatalf("dryRun=%v result = %+v; want 2 valid (B2 first, C3) and the 2 collisions named", dry, res)
		}
		byRow := map[int]ImportRowError{}
		for _, e := range res.Errors {
			byRow[e.Row] = e
		}
		if e := byRow[1]; e.Code != "RECORD_DUPLICATE" || e.Field != "code" || !strings.Contains(e.Message, "đã tồn tại") {
			t.Fatalf("row 1 = %+v; want RECORD_DUPLICATE on code (already stored)", e)
		}
		if e := byRow[3]; e.Code != "RECORD_DUPLICATE" || !strings.Contains(e.Message, "trùng với dòng trước") {
			t.Fatalf("row 3 = %+v; want RECORD_DUPLICATE (repeats row 2 in the file)", e)
		}
		if dry == store.insertCalled || (!dry && len(store.insertedRows) != 2) {
			t.Fatalf("dryRun=%v insertCalled=%v rows=%d; the run writes the 2 clean rows, the preview none", dry, store.insertCalled, len(store.insertedRows))
		}
	}
}

// The chosen dedup field is probed once (by its own pass), not a second time as "another unique field".
func TestImport_UniqueProbeSkipsTheDedupField(t *testing.T) {
	store := &fakeImportStore{}
	importUnits(t, store, ImportInput{IsAdmin: true, DryRun: true, DedupField: "code"})
	if store.batchDedupCalls != 1 {
		t.Fatalf("BatchExistingValuesForField calls = %d; want 1", store.batchDedupCalls)
	}
}

// A race between the probe and the insert still ends in the old whole-batch 409.
func TestImport_UniqueRaceStillRefusesTheBatch(t *testing.T) {
	store := &fakeImportStore{insertErr: dynrepo.ErrDuplicateUnique}
	svc := NewImportService(uniqueCodeCatalog(), store, nil)
	_, err := svc.Import(context.Background(), ImportInput{
		TenantID: "t1", UserID: "u1", EntityName: "units", IsAdmin: true,
		Reader: strings.NewReader("Tên,Mã\nCăn 1,A1\n"), Mapping: map[string]string{"Tên": "name", "Mã": "code"},
	})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "IMPORT_BATCH_DUPLICATE" {
		t.Fatalf("err = %v; want IMPORT_BATCH_DUPLICATE", err)
	}
}
