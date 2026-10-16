//go:build integration

// Text-result formulas + AND/OR (specs/053, queue #109) against a real throwaway MySQL 8: the
// grading formula saves with resultType "text", a record write stores the chosen text in the
// JSON data column and it reads back as a string, a partial update recomputes it, and a
// mismatched resultType is refused on add-field AND edit with the stored options untouched.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestFormulaTextInt -v
package services

import (
	"context"
	"strings"
	"testing"
)

func TestFormulaTextInt_GradeAndAndOr(t *testing.T) {
	st := frWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "cccc1111-0000-0000-0000-000000000051"
	const grade = `IF(score >= 8, "Giỏi", IF(score >= 6.5, "Khá", "TB"))`

	st.createEntity(t, ctx, tenant, "fx_results", []FieldInput{
		{Name: "student", Label: "Học viên", FieldType: "text"},
		{Name: "score", Label: "Điểm", FieldType: "number"},
		{Name: "absences", Label: "Buổi vắng", FieldType: "number"},
		{Name: "grade", Label: "Xếp loại", FieldType: "formula",
			Options: map[string]any{"formula": grade, "resultType": "text"}},
		{Name: "scholarship", Label: "Học bổng", FieldType: "formula",
			Options: map[string]any{"formula": "IF(score >= 8 AND absences <= 2, 1000000, 0)"}},
	})

	id, err := st.recordSvc.Create(ctx, wfAdmin(user), tenant, user, "fx_results",
		map[string]any{"student": "An", "score": 9.0, "absences": 1.0})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec, err := st.recordSvc.Get(ctx, wfAdmin(user), tenant, "fx_results", id, nil, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got, ok := rec.Data["grade"].(string); !ok || got != "Giỏi" {
		t.Fatalf("grade = %T %v; want string Giỏi", rec.Data["grade"], rec.Data["grade"])
	}
	if got, _ := rec.Data["scholarship"].(float64); got != 1000000 {
		t.Fatalf("scholarship = %v; want 1000000 (score 9 AND 1 absence)", rec.Data["scholarship"])
	}

	// A partial update recomputes both from the merged image.
	if err := st.recordSvc.Update(ctx, wfAdmin(user), tenant, "fx_results", id,
		map[string]any{"score": 7.0}, nil); err != nil {
		t.Fatalf("Update score: %v", err)
	}
	rec, err = st.recordSvc.Get(ctx, wfAdmin(user), tenant, "fx_results", id, nil, nil)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got, _ := rec.Data["grade"].(string); got != "Khá" {
		t.Fatalf("grade after score 7 = %v; want Khá", rec.Data["grade"])
	}
	if got, _ := rec.Data["scholarship"].(float64); got != 0 {
		t.Fatalf("scholarship after score 7 = %v; want 0", rec.Data["scholarship"])
	}

	// Add-field: a text formula stored as a number is refused.
	_, err = st.fieldSvc.AddField(ctx, tenant, AddFieldInput{EntityName: "fx_results", Field: FieldInput{
		Name: "tier", Label: "Hạng", FieldType: "formula",
		Options: map[string]any{"formula": `IF(score >= 5, "Đạt", "Chưa đạt")`, "resultType": "number"},
	}})
	if got := w5IntCode(t, err); got != "FIELD_FORMULA_TYPE_MISMATCH" {
		t.Fatalf("add text formula as number: code = %q; want FIELD_FORMULA_TYPE_MISMATCH", got)
	}

	// Edit: turning the grade into a number formula while keeping resultType text is refused,
	// and the stored expression stays.
	const gradeWhere = " FROM sys_fields f JOIN sys_entities e ON e.id = f.entity_id " +
		"WHERE e.tenant_id = ? AND e.name = ? AND f.name = ? AND f.deleted_at IS NULL"
	var gradeID string
	if err := st.sdb.GetContext(ctx, &gradeID, "SELECT f.id"+gradeWhere, tenant, "fx_results", "grade"); err != nil {
		t.Fatalf("read grade field id: %v", err)
	}
	_, err = st.fieldSvc.EditField(ctx, tenant, EditFieldInput{
		FieldID: gradeID,
		Options: map[string]any{"formula": "score * 10", "resultType": "text"},
	})
	if got := w5IntCode(t, err); got != "FIELD_FORMULA_TYPE_MISMATCH" {
		t.Fatalf("edit grade to a number formula: code = %q; want FIELD_FORMULA_TYPE_MISMATCH", got)
	}
	var stored string
	if err := st.sdb.GetContext(ctx, &stored, "SELECT CAST(f.options AS CHAR)"+gradeWhere, tenant, "fx_results", "grade"); err != nil {
		t.Fatalf("read grade options: %v", err)
	}
	if !strings.Contains(stored, "Giỏi") {
		t.Errorf("refused edit changed the stored grade formula: %s", stored)
	}
}
