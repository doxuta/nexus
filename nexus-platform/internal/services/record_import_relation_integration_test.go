//go:build integration

package services

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// specs/061 SC-001/SC-002 (#268) on real MySQL, wired as cmd/server wires it: a file that names
// its projects and customers the way the BĐS journey's file did goes in, a missing name and a
// duplicated name are refused on their own rows — in the preview first, then in the run — and a
// same-named record of ANOTHER tenant is never matched.
func TestImportRelationByName_Integration(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	ctx := context.Background()
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	entSvc := NewEntityService(dynRepo, metaRepo)
	tenant, other, user := uuid.NewString(), uuid.NewString(), uuid.NewString()

	for _, tn := range []string{tenant, other} {
		if _, _, err := entSvc.CreateEntity(ctx, tn, CreateEntityInput{Name: "bds_du_an", Label: "Dự án", Fields: []FieldInput{
			{Name: "ten_du_an", Label: "Tên dự án", FieldType: "text", IsRequired: true},
			{Name: "ma_du_an", Label: "Mã dự án", FieldType: "text", IsUnique: true},
		}}); err != nil {
			t.Fatalf("CreateEntity bds_du_an: %v", err)
		}
		if _, _, err := entSvc.CreateEntity(ctx, tn, CreateEntityInput{Name: "bds_san_pham", Label: "Sản phẩm", Fields: []FieldInput{
			{Name: "ma_can", Label: "Mã căn", FieldType: "text", IsRequired: true},
			{Name: "gia", Label: "Giá", FieldType: "currency", Options: map[string]any{"currencyCode": "VND"}},
			{Name: "du_an", Label: "Dự án", FieldType: "relation", Options: map[string]any{"relatedEntity": "bds_du_an"}},
		}}); err != nil {
			t.Fatalf("CreateEntity bds_san_pham: %v", err)
		}
	}
	seed := func(tn, name, code string) string {
		t.Helper()
		id, err := dynRepo.InsertRecord(ctx, "bds_du_an", tn, user, map[string]any{"ten_du_an": name, "ma_du_an": code}, nil)
		if err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		return id
	}
	riverside := seed(tenant, "Phú Gia Riverside", "PGR")
	seed(tenant, "Sky Garden", "SG1")
	seed(tenant, "Sky Garden", "SG2")
	seed(other, "Lotus Tower", "LT") // another tenant's project: never matched

	svc := NewImportService(metaRepo, dynRepo, nil).WithSequenceClaimer(dynRepo).WithRelationChecker(dynRepo)
	csv := "\ufeffMã căn;Giá;Dự án\n" +
		"A1-01;3.200.000.000;Phú Gia Riverside\n" +
		"A1-02;2.750.000.000 đ;phú gia riverside\n" +
		"B2-01;1.900.000.000;Sky Garden\n" +
		"C3-01;2.000.000.000;Lotus Tower\n"
	in := ImportInput{TenantID: tenant, UserID: user, IsAdmin: true, EntityName: "bds_san_pham",
		Mapping: map[string]string{"Mã căn": "ma_can", "Giá": "gia", "Dự án": "du_an"}}

	// Preview first: same verdicts, nothing written.
	in.DryRun, in.Reader = true, strings.NewReader(csv)
	preview, err := svc.Import(ctx, in)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if preview.Valid != 2 || preview.Failed != 2 || preview.Inserted != 0 {
		t.Fatalf("preview = %+v; want 2 valid, 2 failed, 0 inserted", preview)
	}
	codes := map[int]string{}
	for _, e := range preview.Errors {
		if e.Field != "du_an" {
			t.Fatalf("preview error on field %q: %+v", e.Field, e)
		}
		codes[e.Row] = e.Code
	}
	if codes[3] != "IMPORT_RELATION_AMBIGUOUS" || codes[4] != "RECORD_RELATION_INVALID" {
		t.Fatalf("preview codes = %v; want row 3 ambiguous, row 4 not found", codes)
	}
	var n int
	if err := sdb.Get(&n, "SELECT COUNT(*) FROM obj_bds_san_pham WHERE tenant_id = ?", tenant); err != nil || n != 0 {
		t.Fatalf("preview wrote %d rows (err %v)", n, err)
	}

	// The run: exactly the previewed rows go in, linked by id.
	in.DryRun, in.Reader = false, strings.NewReader(csv)
	res, err := svc.Import(ctx, in)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Inserted != 2 || res.Failed != 2 {
		t.Fatalf("result = %+v; want 2 inserted, 2 failed", res)
	}
	var linked []string
	if err := sdb.Select(&linked, "SELECT JSON_UNQUOTE(JSON_EXTRACT(data, '$.du_an')) FROM obj_bds_san_pham WHERE tenant_id = ? ORDER BY JSON_EXTRACT(data, '$.ma_can')", tenant); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(linked) != 2 || linked[0] != riverside || linked[1] != riverside {
		t.Fatalf("stored du_an = %v; want both linked to %s", linked, riverside)
	}

	// "Khớp theo" a unique code instead of the name.
	in.MatchBy, in.Reader = map[string]string{"du_an": "ma_du_an"}, strings.NewReader("Mã căn,Dự án\nD4-01,sg2\n")
	byCode, err := svc.Import(ctx, in)
	if err != nil || byCode.Inserted != 1 {
		t.Fatalf("match by code = %+v, %v; want 1 inserted", byCode, err)
	}

	// A rep whose VIEW grant on projects is "owner" finds only their own project by name: a
	// colleague's project, named by a guessable key, reads as missing — in the preview too, where
	// nothing is written (review MEDIUM, FR-015).
	rep := uuid.NewString()
	own, err := dynRepo.InsertRecord(ctx, "bds_du_an", tenant, rep, map[string]any{"ten_du_an": "Dự án Của Rep", "ma_du_an": "REP"}, nil)
	if err != nil {
		t.Fatalf("seed rep project: %v", err)
	}
	repSvc := NewImportService(metaRepo, dynRepo, nil).WithRelationChecker(dynRepo).WithACLAuthorizer(&levelAuthz{level: "owner"})
	repCSV := "Mã căn,Dự án\nE5-01,Dự án của rep\nE5-02,Phú Gia Riverside\n"
	repIn := ImportInput{TenantID: tenant, UserID: rep, RoleID: "sales_rep", EntityName: "bds_san_pham",
		Mapping: map[string]string{"Mã căn": "ma_can", "Dự án": "du_an"}, DryRun: true, Reader: strings.NewReader(repCSV)}
	repRes, err := repSvc.Import(ctx, repIn)
	if err != nil {
		t.Fatalf("rep dry run: %v", err)
	}
	if repRes.Valid != 1 || len(repRes.Errors) != 1 || repRes.Errors[0].Row != 2 || repRes.Errors[0].Code != "RECORD_RELATION_INVALID" {
		t.Fatalf("rep preview = %+v; want row 1 valid (own project), row 2 not found (a colleague's)", repRes)
	}
	repIn.DryRun, repIn.Reader = false, strings.NewReader(repCSV)
	if repRes, err = repSvc.Import(ctx, repIn); err != nil || repRes.Inserted != 1 {
		t.Fatalf("rep import = %+v, %v; want 1 inserted", repRes, err)
	}
	var repLinked string
	if err := sdb.Get(&repLinked, "SELECT JSON_UNQUOTE(JSON_EXTRACT(data, '$.du_an')) FROM obj_bds_san_pham WHERE tenant_id = ? AND JSON_UNQUOTE(JSON_EXTRACT(data, '$.ma_can')) = 'E5-01'", tenant); err != nil || repLinked != own {
		t.Fatalf("rep row du_an = %q (err %v); want %s", repLinked, err, own)
	}
}
