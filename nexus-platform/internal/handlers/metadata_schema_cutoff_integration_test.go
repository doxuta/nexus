//go:build integration

// specs/063-draft-publish S7-8 T118: a table, a field and a relation created AFTER the last publish do
// not reach the customer's staff through GET /metadata/entities | fields/:entity | relationships (nor
// the "Liên quan" card they would open) until the next version moves the cutoff. Driven through the
// production router over a freshly migrated throwaway DB, as a NON-admin member.
package handlers

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/services"
)

func TestMetadataReads_HideSchemaBornAfterTheLastPublish(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	tenantID, _, freeRole := flsMetaSeed(t, gdb, sx) // fls_accounts (name/revenue/notes)
	ctx := context.Background()
	metaRepo := metadata.NewRepository(gdb)
	dyn := dynrepo.New(sx)
	entSvc := services.NewEntityService(dyn, metaRepo)
	if _, _, err := entSvc.CreateEntity(ctx, tenantID, services.CreateEntityInput{Name: "fls_child", Label: "Child",
		Fields: []services.FieldInput{{Name: "title", Label: "Title", FieldType: "text"}}}); err != nil {
		t.Fatalf("CreateEntity fls_child: %v", err)
	}

	// Deliver: one numbered version whose cutoff is "now"; whatever is created in a later second is new.
	versions := metaversion.NewVersionRepo(sx)
	insertVersion := func(no uint, at time.Time) {
		t.Helper()
		v := &metaversion.Version{TenantID: tenantID, Seq: no, VersionNo: &no, Kind: metaversion.KindBaseline, CoveredTypes: []byte("[]"),
			SchemaCutoffAt: &at, CreatedBy: "builder"}
		if err := versions.InsertVersion(ctx, sx, v); err != nil {
			t.Fatalf("InsertVersion: %v", err)
		}
	}
	time.Sleep(1100 * time.Millisecond) // created_at has second precision and rounds: let the seed fall in an earlier second
	insertVersion(1, time.Now().UTC().Truncate(time.Second))
	time.Sleep(1100 * time.Millisecond)

	fieldSvc := services.NewFieldService(dyn, metaRepo)
	if _, err := fieldSvc.AddField(ctx, tenantID, services.AddFieldInput{EntityName: "fls_accounts", Field: services.FieldInput{Name: "fresh_note", Label: "Fresh", FieldType: "text"}}); err != nil {
		t.Fatalf("AddField fresh_note: %v", err)
	}
	if _, err := fieldSvc.AddField(ctx, tenantID, services.AddFieldInput{EntityName: "fls_child", Field: services.FieldInput{Name: "account", Label: "Account", FieldType: "relation",
		Options: map[string]any{"relatedEntity": "fls_accounts"}}}); err != nil {
		t.Fatalf("AddField relation: %v", err)
	}
	if _, _, err := entSvc.CreateEntity(ctx, tenantID, services.CreateEntityInput{Name: "fls_fresh_table", Label: "Fresh table",
		Fields: []services.FieldInput{{Name: "x", Label: "X", FieldType: "text"}}}); err != nil {
		t.Fatalf("CreateEntity fls_fresh_table: %v", err)
	}

	gate := services.NewDraftGate(versions)
	reg := metaversion.NewRegistry(services.NewFieldDocAdapter())
	mh := NewMetadataHandler(gdb, zap.NewNop()).WithSchemaCutoff(gate).
		WithFieldLister(services.NewDraftFieldRepo(metaRepo, metaversion.NewDraftRepo(sx, reg), gate))
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), accessVerifier(t), nil, APIHandlers{Metadata: mh}, zap.NewNop())
	member := flsMetaToken(t, tenantID, freeRole, false)

	get := func(path string) (int, string) {
		w := doAuthed(t, r, "GET", "/api/v1"+path, member, nil, nil)
		return w.Code, w.Body.String()
	}
	assertHidden := func(label string) {
		t.Helper()
		if code, body := get("/metadata/fields/fls_accounts"); code != http.StatusOK || strings.Contains(body, "fresh_note") || !strings.Contains(body, `"name"`) {
			t.Errorf("%s: fields/fls_accounts = %d, want 200 without fresh_note and with name: %s", label, code, body)
		}
		if code, body := get("/metadata/fields/fls_child"); code != http.StatusOK || strings.Contains(body, `"account"`) {
			t.Errorf("%s: fields/fls_child = %d, want the new relation field hidden: %s", label, code, body)
		}
		if code, body := get("/metadata/entities"); code != http.StatusOK || strings.Contains(body, "fls_fresh_table") || !strings.Contains(body, "fls_accounts") {
			t.Errorf("%s: entities = %d, want the new table hidden and the old ones listed: %s", label, code, body)
		}
		if code, body := get("/metadata/fields/fls_fresh_table"); code != http.StatusNotFound {
			t.Errorf("%s: fields of a table born after the publish = %d %s, want 404 (like an absent table)", label, code, body)
		}
		// the "Liên quan" card of fls_accounts is fed by this list: the new relation must not open one
		if code, body := get("/metadata/relationships?entity=fls_accounts"); code != http.StatusOK || strings.Contains(body, "fls_child") {
			t.Errorf("%s: relationships = %d, want no row for the relation born after the publish: %s", label, code, body)
		}
	}
	assertHidden("before the next publish")

	// The next version moves the cutoff past everything: it is all the customer's now. (A new
	// process has no cached cutoff; the running one picks it up within the 5-second TTL, or at once
	// through DraftGate.RefreshCutoff, which PublishService calls.)
	insertVersion(2, time.Now().UTC().Truncate(time.Second).Add(time.Second))
	gate.RefreshCutoff(tenantID, time.Now().UTC().Add(time.Second))
	if code, body := get("/metadata/fields/fls_accounts"); code != http.StatusOK || !strings.Contains(body, "fresh_note") {
		t.Errorf("after the next version: fields = %d, want fresh_note visible: %s", code, body)
	}
	if code, body := get("/metadata/entities"); code != http.StatusOK || !strings.Contains(body, "fls_fresh_table") {
		t.Errorf("after the next version: entities = %d, want the new table: %s", code, body)
	}
	if code, body := get("/metadata/relationships?entity=fls_accounts"); code != http.StatusOK || !strings.Contains(body, "fls_child") {
		t.Errorf("after the next version: relationships = %d, want the relation: %s", code, body)
	}
}
