//go:build integration

// #272 — the table's label field is served as isLabel on GET /metadata/fields/:entity so the FE
// title/card/chip logic names records exactly like the server's ?expand. Real DB, production router.
package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

func labelFlags(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	var resp struct {
		Data []struct {
			Name    string `json:"name"`
			IsLabel bool   `json:"isLabel"`
			// #479: true only when the builder's own choice named the field.
			IsLabelExplicit bool `json:"isLabelExplicit"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	out := map[string]bool{}
	for _, f := range resp.Data {
		out[f.Name] = f.IsLabel
		if f.IsLabelExplicit {
			out["explicit:"+f.Name] = true
		}
	}
	return out
}

func TestEntityLabelField_IsLabelServed(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	tenantID := uuid.NewString()
	a4SeedTenant(t, gdb, tenantID, "lbl-field", "Label Field")
	role := a4SeedRole(t, gdb, tenantID, "lbl-staff", nil)
	metaRepo := metadata.NewRepository(gdb)
	svc := services.NewEntityService(dynrepo.New(sx), metaRepo)
	if _, _, err := svc.CreateEntity(context.Background(), tenantID, services.CreateEntityInput{
		Name: "lbl_patients", Label: "Bệnh nhân",
		Fields: []services.FieldInput{
			{Name: "ghi_chu", Label: "Ghi chú", FieldType: "text"},
			{Name: "ho_ten", Label: "Họ tên", FieldType: "text", IsRequired: true},
			{Name: "email", Label: "Email", FieldType: "email"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	r := newFLSMetaRouter(t, gdb, nil)
	path := "/api/v1/metadata/fields/lbl_patients"
	tok := flsMetaToken(t, tenantID, role, false) // non-admin reads it too

	// Default: no choice, no field called "name" -> first required text.
	w := doAuthed(t, r, "GET", path, tok, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET fields: %d %s", w.Code, w.Body.String())
	}
	if got := labelFlags(t, w.Body.Bytes()); !got["ho_ten"] || got["ghi_chu"] || got["email"] || got["explicit:ho_ten"] {
		t.Fatalf("default (inferred) label field must be ho_ten only and NOT explicit, got %v", got)
	}

	// The builder picks email -> only email is flagged.
	if _, err := svc.UpdateEntity(context.Background(), tenantID, "lbl_patients", services.UpdateEntityInput{
		Options: map[string]any{"labelField": "email"},
	}); err != nil {
		t.Fatalf("UpdateEntity: %v", err)
	}
	w = doAuthed(t, r, "GET", path, tok, nil, nil)
	if got := labelFlags(t, w.Body.Bytes()); !got["email"] || got["ho_ten"] || !got["explicit:email"] {
		t.Fatalf("chosen (explicit) label field must be email only, got %v (%s)", got, w.Body.String())
	}
}

// FR-004 (#479): the label flag is never attached to a field the caller's role cannot see — the
// chosen field hidden by sys_field_acl yields NO isLabel at all for that role (the FE then falls to
// its own guesses over the served fields), while a role without the restriction and the admin keep it.
func TestEntityLabelField_NeverOnHiddenField(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	tenantID := uuid.NewString()
	a4SeedTenant(t, gdb, tenantID, "lbl-hidden", "Label Hidden")
	hiddenRole := a4SeedRole(t, gdb, tenantID, "lbl-hid", nil)
	freeRole := a4SeedRole(t, gdb, tenantID, "lbl-free", nil)
	metaRepo := metadata.NewRepository(gdb)
	svc := services.NewEntityService(dynrepo.New(sx), metaRepo)
	if _, _, err := svc.CreateEntity(context.Background(), tenantID, services.CreateEntityInput{
		Name: "lbl_hidden", Label: "Khách",
		Fields: []services.FieldInput{
			{Name: "ho_ten", Label: "Họ tên", FieldType: "text", IsRequired: true},
			{Name: "ghi_chu", Label: "Ghi chú", FieldType: "text"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if _, err := svc.UpdateEntity(context.Background(), tenantID, "lbl_hidden", services.UpdateEntityInput{
		Options: map[string]any{"labelField": "ho_ten"},
	}); err != nil {
		t.Fatalf("UpdateEntity: %v", err)
	}
	if _, err := sx.Exec(
		`INSERT INTO sys_field_acl (id, tenant_id, role_id, entity_name, field_name, can_view, can_edit, created_at, updated_at)
		 VALUES (?, ?, ?, 'lbl_hidden', 'ho_ten', 0, 0, NOW(3), NOW(3))`,
		uuid.NewString(), tenantID, hiddenRole); err != nil {
		t.Fatalf("seed sys_field_acl: %v", err)
	}
	r := newFLSMetaRouter(t, gdb, services.NewFieldACLService(auth.NewFieldACLRepository(gdb)))
	path := "/api/v1/metadata/fields/lbl_hidden"

	w := doAuthed(t, r, "GET", path, flsMetaToken(t, tenantID, hiddenRole, false), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("hidden-role GET fields: %d %s", w.Code, w.Body.String())
	}
	got := labelFlags(t, w.Body.Bytes())
	if _, served := got["ho_ten"]; served {
		t.Fatalf("hidden field must not be served at all: %v", got)
	}
	for name, v := range got {
		if v {
			t.Fatalf("%s carries isLabel/explicit for a role that cannot see the chosen field: %v", name, got)
		}
	}
	for _, tok := range []string{flsMetaToken(t, tenantID, freeRole, false), flsMetaToken(t, tenantID, "", true)} {
		w = doAuthed(t, r, "GET", path, tok, nil, nil)
		if g := labelFlags(t, w.Body.Bytes()); !g["ho_ten"] || !g["explicit:ho_ten"] {
			t.Fatalf("unrestricted caller must keep the label flag, got %v", g)
		}
	}
}
