//go:build integration

// DAILY-QUEUE #334 edit-action-follows-record-scope over the FULL production chain (Auth → Tenant →
// ACL → DynamicRecordHandler → RecordService → dynrepo) on a freshly migrated MySQL, with REAL logins
// of non-admin users: a role whose VIEW is "all" but whose EDIT is "owner" (crm_sales_rep on a lead)
// opens a colleague's record. GET must say canEdit:false — and the PUT the page would otherwise offer
// must indeed be refused — while the owner, a manager (edit=all) and an admin carry no such flag.
package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"nexus/platform/internal/auth"
)

const (
	canEditTenantID   = "ce0e0000-0000-0000-0000-0000000000a1"
	canEditTenantSlug = "canedit-acme"
	canEditEntity     = "it_canedit_leads"
)

func TestRecordGet_CanEditFlag_OverHTTP(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)
	a4SeedTenant(t, gdb, canEditTenantID, canEditTenantSlug, "Acme canEdit")

	// "rep" mirrors crm_sales_rep on crm_leads: sees everyone's leads, edits only their own.
	rep := a4SeedRole(t, gdb, canEditTenantID, "rep", []auth.RoleAction{
		grantLevel(canEditEntity, "create", "all"), grantLevel(canEditEntity, "view", "all"),
		grantLevel(canEditEntity, "edit", "owner"),
	})
	manager := a4SeedRole(t, gdb, canEditTenantID, "manager", []auth.RoleAction{
		grantLevel(canEditEntity, "view", "all"), grantLevel(canEditEntity, "edit", "all"),
	})
	viewer := a4SeedRole(t, gdb, canEditTenantID, "viewer", []auth.RoleAction{grantLevel(canEditEntity, "view", "all")})
	a4SeedUser(t, gdb, canEditTenantID, "a@"+canEditTenantSlug, false, &rep)
	a4SeedUser(t, gdb, canEditTenantID, "b@"+canEditTenantSlug, false, &rep)
	a4SeedUser(t, gdb, canEditTenantID, "m@"+canEditTenantSlug, false, &manager)
	a4SeedUser(t, gdb, canEditTenantID, "v@"+canEditTenantSlug, false, &viewer)
	a4SeedUser(t, gdb, canEditTenantID, "admin@"+canEditTenantSlug, true, nil)

	a4DropEntity(sx, []string{canEditTenantID}, canEditEntity)
	t.Cleanup(func() { a4DropEntity(sx, []string{canEditTenantID}, canEditEntity) })
	rt.createEntity(t, rt.login(t, canEditTenantSlug, "admin@"+canEditTenantSlug).Data.AccessToken, canEditEntity)

	tok := func(email string) string { return rt.login(t, canEditTenantSlug, email).Data.AccessToken }
	aTok, bTok, mTok, vTok, adminTok := tok("a@"+canEditTenantSlug), tok("b@"+canEditTenantSlug), tok("m@"+canEditTenantSlug), tok("v@"+canEditTenantSlug), tok("admin@"+canEditTenantSlug)

	base := "/api/v1/" + canEditEntity
	w := doAuthed(t, rt.engine, http.MethodPost, base, aTok, map[string]any{"title": "Lead của A"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("A create: %d %s", w.Code, w.Body.String())
	}
	var created recordCreateEnvelope
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	url := base + "/" + created.Data.ID

	// canEdit reads the flag off GET: (value, present).
	canEdit := func(who, token string) (bool, bool) {
		t.Helper()
		w := doAuthed(t, rt.engine, http.MethodGet, url, token, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s GET: %d %s", who, w.Code, w.Body.String())
		}
		var env struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		v, present := env.Data["canEdit"]
		b, _ := v.(bool)
		return b, present
	}
	put := func(token string) int {
		t.Helper()
		return doAuthed(t, rt.engine, http.MethodPut, url, token, map[string]any{"title": "sửa"}, nil).Code
	}

	// B: sees A's lead (view=all) but cannot edit it — the flag says so, and the PUT agrees.
	if got, present := canEdit("B", bTok); !present || got {
		t.Fatalf("B on A's lead: canEdit = (%v, present=%v); want (false, true)", got, present)
	}
	if code := put(bTok); code != http.StatusForbidden {
		t.Fatalf("B PUT on A's lead = %d; want 403 (the flag must match what the write does; B can read it, so not 404 — #18)", code)
	}
	// A (owner), the manager (edit=all) and the admin: no restriction known → flag absent; and they can edit.
	for who, token := range map[string]string{"A": aTok, "manager": mTok, "admin": adminTok} {
		if _, present := canEdit(who, token); present {
			t.Fatalf("%s on A's lead: canEdit present, want absent", who)
		}
		if code := put(token); code != http.StatusOK {
			t.Fatalf("%s PUT on A's lead = %d; want 200", who, code)
		}
	}
	// The viewer holds no edit grant at all: the page must not offer Sửa either.
	if got, present := canEdit("viewer", vTok); !present || got {
		t.Fatalf("viewer: canEdit = (%v, present=%v); want (false, true)", got, present)
	}
}
