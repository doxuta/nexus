//go:build integration

// Issue #2, verifier round 1 (gap 1): a table born after the last publish was hidden from the metadata
// reads only. With a grant on it (the assistant's access card applies at once, before the publish) the
// rep could GET /<table> (200) and POST it (201) — the customer used a table nobody had published.
// Driven through the production router over a migrated throwaway DB, as the NON-admin rep holding the
// grant: the record path answers 404 ENTITY_NOT_FOUND, as for a table that is not there, until the next
// version moves the cutoff past it. Round 2: the CSV import too (it resolved the table on its own).
package handlers

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/services"
)

func TestRecordPath_TableBornAfterThePublishIsAbsentForTheRep(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	ctx := context.Background()
	tenantID := "c2c2c2c2-0000-0000-0000-00000000c2c2"
	a4SeedTenant(t, gdb, tenantID, "cutoff-rec", "Cutoff records")
	grants := func(entity string) []auth.RoleAction {
		return []auth.RoleAction{{EntityName: entity, Action: "view"}, {EntityName: entity, Action: "create"}}
	}
	repRole := a4SeedRole(t, gdb, tenantID, "rep", append(grants("cut_old"), grants("cut_new")...))

	metaRepo := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sx)
	entSvc := services.NewEntityService(dyn, metaRepo)
	create := func(name string) {
		t.Helper()
		if _, _, err := entSvc.CreateEntity(ctx, tenantID, services.CreateEntityInput{Name: name, Label: name,
			Fields: []services.FieldInput{{Name: "title", Label: "Title", FieldType: "text"}}}); err != nil {
			t.Fatalf("CreateEntity %s: %v", name, err)
		}
	}
	create("cut_old")

	// Deliver: a version whose cutoff is "now"; a table created in a later second is new.
	versions := metaversion.NewVersionRepo(sx)
	insertVersion := func(no uint, at time.Time) {
		t.Helper()
		v := &metaversion.Version{TenantID: tenantID, Seq: no, VersionNo: &no, Kind: metaversion.KindBaseline, CoveredTypes: []byte("[]"),
			SchemaCutoffAt: &at, CreatedBy: "builder"}
		if err := versions.InsertVersion(ctx, sx, v); err != nil {
			t.Fatalf("InsertVersion: %v", err)
		}
	}
	time.Sleep(1100 * time.Millisecond) // created_at has second precision: the seed falls in an earlier second
	insertVersion(1, time.Now().UTC().Truncate(time.Second))
	time.Sleep(1100 * time.Millisecond)
	create("cut_new")

	gate := services.NewDraftGate(versions)
	aclSvc := services.NewACLService(auth.NewRoleRepository(gdb))
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), accessVerifier(t), aclSvc, APIHandlers{
		Record: NewDynamicRecordHandler(services.NewRecordService(metaRepo, dyn).WithACLAuthorizer(aclSvc).WithSchemaCutoff(gate), zap.NewNop()),
		Import: NewImportHandler(services.NewImportService(metaRepo, dyn, zap.NewNop()).WithSchemaCutoff(gate), zap.NewNop()),
	}, zap.NewNop())
	rep := flsMetaToken(t, tenantID, repRole, false)
	do := func(method, path string, body any) (int, string) {
		w := doAuthed(t, r, method, "/api/v1"+path, rep, body, nil)
		return w.Code, w.Body.String()
	}
	codeOf := func(method, path string, body any) (int, string) {
		w := doAuthed(t, r, method, "/api/v1"+path, rep, body, nil)
		return w.Code, errorCode(parseBody(t, w))
	}
	// importCSV posts a one-row CSV import as the rep (verifier round 2: the import resolved the table on its own).
	importCSV := func(table string) *httptest.ResponseRecorder {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, err := mw.CreateFormFile("file", "rows.csv")
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		_, _ = fw.Write([]byte("title\nimported by the rep\n"))
		_ = mw.WriteField("mapping", `{"title":"title"}`)
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/"+table+"/import", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+rep)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// Control: the same grants on a table the customer has.
	if code, body := do("POST", "/cut_old", map[string]any{"title": "old"}); code != http.StatusCreated {
		t.Fatalf("POST /cut_old as the rep = %d %s; want 201", code, body)
	}
	if code, body := do("GET", "/cut_old", nil); code != http.StatusOK {
		t.Fatalf("GET /cut_old as the rep = %d %s; want 200", code, body)
	}
	if w := importCSV("cut_old"); w.Code != http.StatusOK {
		t.Fatalf("POST /cut_old/import as the rep = %d %s; want 200", w.Code, w.Body.String())
	}
	// The table born after the publish does not exist for the rep yet, grant or not.
	for _, m := range []string{"GET", "POST"} {
		if code, ec := codeOf(m, "/cut_new", map[string]any{"title": "new"}); code != http.StatusNotFound || ec != "ENTITY_NOT_FOUND" {
			t.Fatalf("%s /cut_new as the rep before the publish = %d %s; want 404 ENTITY_NOT_FOUND", m, code, ec)
		}
	}
	if w := importCSV("cut_new"); w.Code != http.StatusNotFound || errorCode(parseBody(t, w)) != "ENTITY_NOT_FOUND" {
		t.Fatalf("POST /cut_new/import as the rep before the publish = %d %s; want 404 ENTITY_NOT_FOUND", w.Code, w.Body.String())
	}
	var rows int
	if err := sx.Get(&rows, "SELECT COUNT(*) FROM obj_cut_new WHERE tenant_id = ?", tenantID); err != nil || rows != 0 {
		t.Fatalf("rows written into the unpublished table = %d (%v); want 0", rows, err)
	}

	// The next version publishes it: the same requests now go through.
	insertVersion(2, time.Now().UTC().Truncate(time.Second).Add(time.Second))
	gate.RefreshCutoff(tenantID, time.Now().UTC().Add(time.Second))
	if code, body := do("POST", "/cut_new", map[string]any{"title": "new"}); code != http.StatusCreated {
		t.Fatalf("POST /cut_new after the publish = %d %s; want 201", code, body)
	}
	if code, body := do("GET", "/cut_new", nil); code != http.StatusOK {
		t.Fatalf("GET /cut_new after the publish = %d %s; want 200", code, body)
	}
}
