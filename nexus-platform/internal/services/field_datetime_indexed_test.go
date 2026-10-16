package services

// Tests for the D1 blocker fix: datetime fields with indexed=true must be rejected at
// validation time (422 FIELD_INDEX_UNSUPPORTED_TYPE) in both the CreateEntity and
// EditField paths. The date type must continue to work without restriction.

import (
	"context"
	"testing"
)

// --- 1. ValidateCreateEntity path ----------------------------------------------------------

// TestValidateCreateEntity_IndexedDatetimeRejected asserts that declaring an indexed
// datetime field inside a CreateEntityInput is rejected with FIELD_INDEX_UNSUPPORTED_TYPE
// before any DDL or metadata write runs. This is the primary chokepoint via
// validateFieldConfig.
func TestValidateCreateEntity_IndexedDatetimeRejected(t *testing.T) {
	in := validEntity()
	in.Fields = []FieldInput{
		{
			Name:      "birthday",
			Label:     "Ngày sinh",
			FieldType: "datetime",
			Options:   map[string]any{"indexed": true},
		},
	}
	err := ValidateCreateEntity(in)
	if codeOf(err) != "FIELD_INDEX_UNSUPPORTED_TYPE" {
		t.Errorf("ValidateCreateEntity indexed datetime: code = %q (err=%v); want FIELD_INDEX_UNSUPPORTED_TYPE", codeOf(err), err)
	}
}

// TestValidateCreateEntity_IndexedDateNotRejected confirms that date (YYYY-MM-DD) remains
// indexable after the fix — only datetime is rejected (control case).
func TestValidateCreateEntity_IndexedDateNotRejected(t *testing.T) {
	in := validEntity()
	in.Fields = []FieldInput{
		{
			Name:      "birthday",
			Label:     "Ngày sinh",
			FieldType: "date",
			Options:   map[string]any{"indexed": true},
		},
	}
	if err := ValidateCreateEntity(in); err != nil {
		t.Errorf("ValidateCreateEntity indexed date: unexpected error %v", err)
	}
}

// TestValidateCreateEntity_DatetimeNotIndexedPasses confirms a datetime field without
// indexed=true still passes validation (regression guard: the guard must be narrow).
func TestValidateCreateEntity_DatetimeNotIndexedPasses(t *testing.T) {
	in := validEntity()
	in.Fields = []FieldInput{
		{
			Name:      "scheduled_at",
			Label:     "Thời gian",
			FieldType: "datetime",
			Options:   nil,
		},
	}
	if err := ValidateCreateEntity(in); err != nil {
		t.Errorf("ValidateCreateEntity non-indexed datetime: unexpected error %v", err)
	}
}

// --- 2. AddField / EntityService CreateEntity orchestration path ---------------------------

// TestAddField_IndexedDatetimeRejectedBeforeDDL confirms that the orchestration layer also
// rejects indexed datetime early via validateFieldConfig (called from validateField →
// ValidateAddField), so no schema call or metadata write runs.
func TestAddField_IndexedDatetimeRejectedBeforeDDL(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := &fakeFieldMeta{entity: entityFixture()}
	svc := NewFieldService(schema, meta)

	in := AddFieldInput{
		EntityName: "courses",
		Field: FieldInput{
			Name:      "created_at_dt",
			Label:     "Thời gian tạo",
			FieldType: "datetime",
			Options:   map[string]any{"indexed": true},
		},
	}
	_, err := svc.AddField(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_INDEX_UNSUPPORTED_TYPE" {
		t.Errorf("AddField indexed datetime: code = %q (err=%v); want FIELD_INDEX_UNSUPPORTED_TYPE", codeOf(err), err)
	}
	// No DDL must have been attempted.
	if len(schema.idxAdded) != 0 {
		t.Errorf("AddField indexed datetime must not run DDL; idxAdded=%v", schema.idxAdded)
	}
	if len(meta.created) != 0 {
		t.Errorf("AddField indexed datetime must not persist metadata; created=%v", meta.created)
	}
}

// TestAddField_IndexedDateStillWorks confirms that adding an indexed date field is still
// accepted and runs the expected DDL — verifying the guard is narrow (date only, not all
// temporal types).
func TestAddField_IndexedDateStillWorks(t *testing.T) {
	schema := &fakeFieldSchema{}
	meta := &fakeFieldMeta{entity: entityFixture()}
	svc := NewFieldService(schema, meta)

	in := AddFieldInput{
		EntityName: "courses",
		Field: FieldInput{
			Name:      "start_date",
			Label:     "Ngày bắt đầu",
			FieldType: "date",
			Options:   map[string]any{"indexed": true},
		},
	}
	fd, err := svc.AddField(context.Background(), "tenant-1", in)
	if err != nil {
		t.Fatalf("AddField indexed date: unexpected error %v", err)
	}
	if len(schema.idxAdded) != 1 || schema.idxAdded[0] != "start_date" {
		t.Errorf("AddField indexed date must ADD indexed column; idxAdded=%v", schema.idxAdded)
	}
	if len(meta.created) != 1 {
		t.Errorf("AddField indexed date must persist metadata; created=%v", meta.created)
	}
	if fd.Name != "start_date" {
		t.Errorf("returned field name = %q; want start_date", fd.Name)
	}
}

// --- 3. EditField indexed-toggle path -------------------------------------------------------

// TestEditField_ToggleIndexedOnDatetimeRejected confirms that toggling indexed=true on an
// existing datetime field via EditField is rejected with FIELD_INDEX_UNSUPPORTED_TYPE (422),
// and that no AddIndexedColumn DDL is attempted.
func TestEditField_ToggleIndexedOnDatetimeRejected(t *testing.T) {
	stored := storedField()
	stored.FieldType = "datetime"
	// Stored options: indexed is currently false (not set).
	meta := &fakeEditMeta{field: stored}
	schema := &fakeFieldSchema{}
	svc := NewFieldService(schema, meta)

	in := EditFieldInput{
		FieldID:   "fld-1",
		FieldType: "datetime",
		// Toggle indexed ON.
		Options: map[string]any{"indexed": true},
	}
	_, err := svc.EditField(context.Background(), "tenant-1", in)
	if codeOf(err) != "FIELD_INDEX_UNSUPPORTED_TYPE" {
		t.Errorf("EditField toggle indexed datetime: code = %q (err=%v); want FIELD_INDEX_UNSUPPORTED_TYPE", codeOf(err), err)
	}
	// No DDL must have run.
	if len(schema.idxAdded) != 0 {
		t.Errorf("EditField indexed datetime toggle must not ADD indexed column; idxAdded=%v", schema.idxAdded)
	}
	// No metadata update must have been persisted.
	if len(meta.updated) != 0 {
		t.Errorf("EditField indexed datetime toggle must not persist; updated=%v", meta.updated)
	}
}

// TestEditField_ToggleIndexedOnDateAccepted confirms that toggling indexed=true on a date
// field is still accepted after the fix — regression guard for the date-safe path.
func TestEditField_ToggleIndexedOnDateAccepted(t *testing.T) {
	stored := storedField()
	stored.FieldType = "date"
	// Stored options: no indexed flag (false by default).
	meta := &fakeEditMeta{field: stored}
	schema := &fakeFieldSchema{}
	svc := NewFieldService(schema, meta)

	in := EditFieldInput{
		FieldID:   "fld-1",
		FieldType: "date",
		Options:   map[string]any{"indexed": true},
	}
	_, err := svc.EditField(context.Background(), "tenant-1", in)
	if err != nil {
		t.Errorf("EditField toggle indexed date: unexpected error %v", err)
	}
	if len(schema.idxAdded) != 1 {
		t.Errorf("EditField indexed date toggle must ADD indexed column; idxAdded=%v", schema.idxAdded)
	}
}
