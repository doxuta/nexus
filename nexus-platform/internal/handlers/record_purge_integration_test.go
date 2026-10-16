//go:build integration

// specs/056-record-hard-delete — POST /:entity/:id/purge end to end over MySQL: the real route
// (operator tier wired), RecordService, dynrepo transaction, audit emit and blob port.
//
// Run: go test -tags=integration ./internal/handlers/ -run TestRecordPurgeHTTP
package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// purgeAuditSink captures what the service hands the (async) audit writer.
type purgeAuditSink struct {
	mu     sync.Mutex
	events []audit.Event
}

func (s *purgeAuditSink) Record(_ context.Context, evt audit.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, evt)
}

// everyoneOperator: tokenForTenant mints a random admin per request, so the operator gate's live
// read answers "operator" for any id — the wall itself is proven in record_purge_route_test.go.
type everyoneOperator struct{}

func (everyoneOperator) GetUserByID(_ context.Context, tenantID, id string) (*auth.User, error) {
	return &auth.User{ID: id, TenantID: tenantID, IsAdmin: true, IsPlatformOperator: true, Status: "active"}, nil
}

func newPurgeRouter(t *testing.T) (*gin.Engine, *sqlx.DB, *purgeAuditSink, *[][]string) {
	t.Helper()
	sx, err := sqlx.Connect("mysql", dsn())
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping", err)
	}
	gdb, err := gorm.Open(gormmysql.Open(dsn()), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	meta := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sx)
	sink := &purgeAuditSink{}
	blobCalls := &[][]string{}
	recordSvc := services.NewRecordService(meta, dyn).WithAuditor(sink).WithRelationshipLister(meta).
		WithBlobRemover(func(_ context.Context, _ string, ids []string) (int, error) {
			*blobCalls = append(*blobCalls, ids)
			return len(ids), nil
		})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), accessVerifier(t), nil, APIHandlers{
		Admin:  NewAdminEntityHandler(services.NewEntityService(dyn, meta), services.NewFieldService(dyn, meta), zap.NewNop()),
		Record: NewDynamicRecordHandler(recordSvc, zap.NewNop()),
	}, zap.NewNop(), WithOperatorTier(middleware.RequirePlatformOperator(everyoneOperator{}, zap.NewNop())))
	return r, sx, sink, blobCalls
}

// hardDeletes returns the hard_delete events captured so far (create/delete rows of the fixture
// records also land in the sink).
func hardDeletes(s *purgeAuditSink) []audit.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []audit.Event
	for _, ev := range s.events {
		if ev.Action == audit.ActionHardDelete {
			out = append(out, ev)
		}
	}
	return out
}

type purgeReport struct {
	DryRun  bool `json:"dryRun"`
	Records []struct{ Entity, ID string }
	Tables  []struct {
		Table string
		Rows  int64
	}
	TotalRows int64  `json:"totalRows"`
	Removed   int    `json:"blobsRemoved"`
	Note      string `json:"blobsNote"`
}

func decodePurge(t *testing.T, body []byte) purgeReport {
	t.Helper()
	var env struct {
		Data purgeReport `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode purge report: %v (%s)", err, body)
	}
	return env.Data
}

func binTotal(t *testing.T, r *gin.Engine, entity string) int {
	t.Helper()
	w := do(t, r, http.MethodGet, "/api/v1/"+entity+"/deleted", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list bin: %d %s", w.Code, w.Body.String())
	}
	var env struct {
		Pagination struct{ Total int } `json:"pagination"`
	}
	json.Unmarshal(w.Body.Bytes(), &env)
	return env.Pagination.Total
}

func TestRecordPurgeHTTP_DryRunThenPurge(t *testing.T) {
	r, sx, sink, blobCalls := newPurgeRouter(t)
	const entity = "it_purge_http"
	clean := func() {
		cleanupNamed(sx, entity)
		sx.Exec(`DELETE FROM sys_files WHERE tenant_id = ? AND entity_name = ?`, testTenant, entity)
	}
	clean()
	t.Cleanup(clean)

	if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", map[string]any{
		"name": entity, "label": "Purge",
		"fields": []map[string]any{{"name": "title", "label": "Title", "fieldType": "text"}},
	}); w.Code != http.StatusCreated {
		t.Fatalf("create entity: %d %s", w.Code, w.Body.String())
	}
	binned := w9bCreateRecord(t, r, entity, map[string]any{"title": "Binned"})
	live := w9bCreateRecord(t, r, entity, map[string]any{"title": "Live"})
	fileID := uuid.NewString()
	if _, err := sx.Exec(`INSERT INTO sys_files (id, tenant_id, entity_name, record_id, field_name, name, path, created_by)
		VALUES (?, ?, ?, ?, 'doc', 'a.pdf', ?, 'u')`, fileID, testTenant, entity, binned, testTenant+"/"+fileID); err != nil {
		t.Fatalf("seed sys_files: %v", err)
	}
	if w := do(t, r, http.MethodDelete, "/api/v1/"+entity+"/"+binned, nil); w.Code != http.StatusNoContent {
		t.Fatalf("soft-delete: %d %s", w.Code, w.Body.String())
	}

	// A live record: 409, nothing touched.
	w := do(t, r, http.MethodPost, "/api/v1/"+entity+"/"+live+"/purge", map[string]any{"dryRun": false})
	if w.Code != http.StatusConflict || !json.Valid(w.Body.Bytes()) {
		t.Fatalf("purge live = %d %s; want 409", w.Code, w.Body.String())
	}
	var e struct{ Code string }
	json.Unmarshal(w.Body.Bytes(), &e)
	if e.Code != "RECORD_NOT_IN_RECYCLE_BIN" {
		t.Fatalf("purge live code = %q; want RECORD_NOT_IN_RECYCLE_BIN", e.Code)
	}

	// A malformed body is refused, not read as a real purge.
	if w := do(t, r, http.MethodPost, "/api/v1/"+entity+"/"+binned+"/purge", "not-json"); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed body = %d %s; want 400", w.Code, w.Body.String())
	}

	// No body at all = dry run: the report, and the record still in the bin.
	w = do(t, r, http.MethodPost, "/api/v1/"+entity+"/"+binned+"/purge", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("dry run = %d %s", w.Code, w.Body.String())
	}
	rep := decodePurge(t, w.Body.Bytes())
	if !rep.DryRun || rep.TotalRows != 2 || len(rep.Records) != 1 || rep.Records[0].ID != binned || rep.Note == "" {
		t.Fatalf("dry-run report = %+v; want dryRun, 2 rows (record + file), a blobs note", rep)
	}
	if n := binTotal(t, r, entity); n != 1 {
		t.Fatalf("dry run emptied the bin (total=%d)", n)
	}
	if n := len(hardDeletes(sink)); n != 0 || len(*blobCalls) != 0 {
		t.Fatalf("dry run audited hard_delete (%d) or removed blobs (%d)", n, len(*blobCalls))
	}

	// The real purge.
	w = do(t, r, http.MethodPost, "/api/v1/"+entity+"/"+binned+"/purge", map[string]any{"dryRun": false})
	if w.Code != http.StatusOK {
		t.Fatalf("purge = %d %s", w.Code, w.Body.String())
	}
	rep = decodePurge(t, w.Body.Bytes())
	if rep.DryRun || rep.TotalRows != 2 || rep.Removed != 1 {
		t.Fatalf("purge report = %+v; want 2 rows and 1 blob removed", rep)
	}
	if n := binTotal(t, r, entity); n != 0 {
		t.Fatalf("record still in the bin after purge (total=%d)", n)
	}
	if w := do(t, r, http.MethodPost, "/api/v1/"+entity+"/"+binned+"/restore", nil); w.Code != http.StatusNotFound {
		t.Fatalf("restore after purge = %d; want 404", w.Code)
	}
	if len(*blobCalls) != 1 || len((*blobCalls)[0]) != 1 || (*blobCalls)[0][0] != fileID {
		t.Fatalf("blob remover calls = %v; want [[%s]]", *blobCalls, fileID)
	}
	hard := hardDeletes(sink)
	if len(hard) != 1 || hard[0].RecordID != binned || hard[0].Diff == nil || hard[0].Diff.Before["title"] != "Binned" {
		t.Fatalf("hard_delete audit = %+v; want one before-only row for the purged record", hard)
	}

	// Twice: nothing left.
	if w := do(t, r, http.MethodPost, "/api/v1/"+entity+"/"+binned+"/purge", map[string]any{"dryRun": false}); w.Code != http.StatusNotFound {
		t.Fatalf("second purge = %d %s; want 404", w.Code, w.Body.String())
	}
	// The live neighbour is untouched.
	if w := do(t, r, http.MethodGet, "/api/v1/"+entity+"/"+live, nil); w.Code != http.StatusOK {
		t.Fatalf("live neighbour after purge = %d; want 200", w.Code)
	}
}

// TestRecordPurgeHTTP_CascadeFromRealMetadata (#47): relation fields added through the admin API
// register their sys_relationships rows; the purge reads THAT registry. A LIVE required child blocks
// (dry run lists it, the real call is 409 and deletes nothing); once binned it goes with the parent,
// and a live optional reference is cleared (the record that held it stays).
func TestRecordPurgeHTTP_CascadeFromRealMetadata(t *testing.T) {
	r, sx, sink, _ := newPurgeRouter(t)
	const parent, child, opt = "it_purge_hp", "it_purge_hc", "it_purge_ho"
	clean := func() {
		sx.Exec(`DELETE FROM sys_relationships WHERE tenant_id = ? AND (parent_entity_name = ? OR child_entity_name IN (?, ?))`, testTenant, parent, child, opt)
		for _, e := range []string{child, opt, parent} {
			cleanupNamed(sx, e)
		}
	}
	clean()
	t.Cleanup(clean)
	for _, spec := range []map[string]any{
		{"name": parent, "label": "Parent", "fields": []map[string]any{{"name": "title", "label": "Title", "fieldType": "text"}}},
		{"name": child, "label": "Child", "fields": []map[string]any{{"name": "title", "label": "Title", "fieldType": "text"}}},
		{"name": opt, "label": "Opt", "fields": []map[string]any{{"name": "title", "label": "Title", "fieldType": "text"}}},
	} {
		if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", spec); w.Code != http.StatusCreated {
			t.Fatalf("create entity %v: %d %s", spec["name"], w.Code, w.Body.String())
		}
	}
	for _, f := range []map[string]any{
		{"entityName": child, "name": "owner_rec", "label": "Parent", "fieldType": "relation", "isRequired": true, "options": map[string]any{"relatedEntity": parent}},
		{"entityName": opt, "name": "about", "label": "About", "fieldType": "relation", "options": map[string]any{"relatedEntity": parent}},
	} {
		if w := do(t, r, http.MethodPost, "/api/v1/admin/fields", f); w.Code != http.StatusCreated {
			t.Fatalf("add relation field %v: %d %s", f["name"], w.Code, w.Body.String())
		}
	}
	p := w9bCreateRecord(t, r, parent, map[string]any{"title": "P"})
	c := w9bCreateRecord(t, r, child, map[string]any{"title": "C", "owner_rec": p})
	o := w9bCreateRecord(t, r, opt, map[string]any{"title": "O", "about": p})
	if w := do(t, r, http.MethodDelete, "/api/v1/"+parent+"/"+p, nil); w.Code != http.StatusNoContent {
		t.Fatalf("bin parent: %d %s", w.Code, w.Body.String())
	}

	// C is live and requires P: the dry run names it, the real purge refuses.
	w := do(t, r, http.MethodPost, "/api/v1/"+parent+"/"+p+"/purge", nil)
	var dry struct {
		Data struct {
			Blocked []struct {
				Entity, Field string
				Count         int
			} `json:"blocked"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &dry)
	if w.Code != http.StatusOK || len(dry.Data.Blocked) != 1 || dry.Data.Blocked[0].Entity != child || dry.Data.Blocked[0].Field != "owner_rec" || dry.Data.Blocked[0].Count != 1 {
		t.Fatalf("dry run with a live required child = %d %s; want blocked %s.owner_rec x1", w.Code, w.Body.String(), child)
	}
	w = do(t, r, http.MethodPost, "/api/v1/"+parent+"/"+p+"/purge", map[string]any{"dryRun": false})
	var e struct{ Code string }
	json.Unmarshal(w.Body.Bytes(), &e)
	if w.Code != http.StatusConflict || e.Code != "RECORD_PURGE_LIVE_CHILDREN" {
		t.Fatalf("purge with a live required child = %d %s; want 409 RECORD_PURGE_LIVE_CHILDREN", w.Code, w.Body.String())
	}
	if n := binTotal(t, r, parent); n != 1 {
		t.Fatalf("refused purge emptied the bin (total=%d)", n)
	}

	// Bin the child: now both go, and O's reference is cleared.
	if w := do(t, r, http.MethodDelete, "/api/v1/"+child+"/"+c, nil); w.Code != http.StatusNoContent {
		t.Fatalf("bin child: %d %s", w.Code, w.Body.String())
	}
	w = do(t, r, http.MethodPost, "/api/v1/"+parent+"/"+p+"/purge", map[string]any{"dryRun": false})
	var rep struct {
		Data struct {
			Records []struct{ Entity, ID string }        `json:"records"`
			Cleared []struct{ Entity, ID, Field string } `json:"cleared"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &rep)
	if w.Code != http.StatusOK || len(rep.Data.Records) != 2 || len(rep.Data.Cleared) != 1 || rep.Data.Cleared[0].ID != o || rep.Data.Cleared[0].Field != "about" {
		t.Fatalf("cascade purge = %d %s; want 2 records + O.about cleared", w.Code, w.Body.String())
	}
	if n := binTotal(t, r, child); n != 0 {
		t.Fatalf("required child still in its bin (total=%d)", n)
	}
	w = do(t, r, http.MethodGet, "/api/v1/"+opt+"/"+o, nil)
	var got struct {
		Data struct {
			Data    map[string]any `json:"data"`
			Version int            `json:"version"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	_, hasAbout := got.Data.Data["about"]
	if w.Code != http.StatusOK || hasAbout || got.Data.Data["title"] != "O" || got.Data.Version != 1 {
		t.Fatalf("O after purge = %d %s; want alive with about cleared", w.Code, w.Body.String())
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var childRows, clearedRows int
	for _, ev := range sink.events {
		if ev.Action == audit.ActionHardDelete && ev.EntityName == child && ev.RecordID == c {
			childRows++
		}
		if ev.Action == audit.ActionUpdate && ev.EntityName == opt && ev.RecordID == o && ev.Diff != nil && ev.Diff.Before["about"] == p {
			clearedRows++
		}
	}
	if childRows != 1 || clearedRows != 1 {
		t.Fatalf("trail: child hard_delete=%d cleared update=%d; want 1 and 1", childRows, clearedRows)
	}
}
