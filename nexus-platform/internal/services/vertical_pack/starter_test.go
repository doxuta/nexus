package vertical_pack

// specs/062-customer-workspace T087 - Starter: the built-in lookup and the install call it makes on
// a new customer workspace (fake installer, so no database).

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/services"
)

type fakePackInstaller struct {
	gotTenant, gotUser string
	gotPack            *Pack
	res                *InstallResult
	err                error
}

func (f *fakePackInstaller) Install(_ context.Context, tenantID, userID string, pack *Pack) (*InstallResult, error) {
	f.gotTenant, f.gotUser, f.gotPack = tenantID, userID, pack
	return f.res, f.err
}

func TestStarter_BuiltinName(t *testing.T) {
	s := NewStarter(&fakePackInstaller{})
	for _, slug := range BuiltinSlugs() {
		if name, ok := s.BuiltinName(slug); !ok || name == "" {
			t.Errorf("BuiltinName(%q) = (%q, %v), want a non-empty name", slug, name, ok)
		}
	}
	if name, ok := s.BuiltinName(" CRM "); !ok || name == "" {
		t.Errorf("BuiltinName(' CRM ') = (%q, %v), want the slug matched case- and space-insensitively", name, ok)
	}
	if _, ok := s.BuiltinName("no-such-pack"); ok {
		t.Error("BuiltinName(no-such-pack) found a pack")
	}
	if _, ok := s.BuiltinName(""); ok {
		t.Error("BuiltinName('') found a pack")
	}
}

func TestStarter_InstallBuiltin_InstallsTheBuiltinPackAndMapsTheResult(t *testing.T) {
	inst := &fakePackInstaller{res: &InstallResult{
		EntitiesCreated: []string{"a", "b"}, RolesCreated: []string{"r"}, WorkflowsCreated: []string{"w1", "w2", "w3"},
		ReportsCreated: []string{"x"}, AppSlug: "crm", Warnings: []string{"note"},
	}}
	got, err := NewStarter(inst).InstallBuiltin(context.Background(), "tenant-9", "builder-1", "CRM")
	if err != nil {
		t.Fatalf("InstallBuiltin: %v", err)
	}
	if inst.gotTenant != "tenant-9" || inst.gotUser != "builder-1" || inst.gotPack == nil {
		t.Fatalf("Install called with (%q, %q, %v), want (tenant-9, builder-1, the crm pack)", inst.gotTenant, inst.gotUser, inst.gotPack)
	}
	want := services.PackStartResult{Slug: "crm", EntitiesCreated: 2, RolesCreated: 1, WorkflowsCreated: 3, ReportsCreated: 1, AppSlug: "crm"}
	if got.Slug != want.Slug || got.EntitiesCreated != 2 || got.RolesCreated != 1 || got.WorkflowsCreated != 3 ||
		got.ReportsCreated != 1 || got.AppSlug != "crm" || len(got.Warnings) != 1 || got.Name == "" || got.Version == "" {
		t.Errorf("result = %+v, want counts/app/warnings mapped from the install and a name + version", got)
	}
}

func TestStarter_InstallBuiltin_Errors(t *testing.T) {
	_, err := NewStarter(&fakePackInstaller{}).InstallBuiltin(context.Background(), "t", "u", "no-such-pack")
	var ve *services.ValidationError
	if !errors.As(err, &ve) || ve.Code != "VERTICAL_BUILTIN_NOT_FOUND" {
		t.Errorf("unknown pack error = %v, want VERTICAL_BUILTIN_NOT_FOUND", err)
	}

	boom := &services.ValidationError{Code: "VERTICAL_PACK_EMPTY", Msg: "empty"}
	_, err = NewStarter(&fakePackInstaller{err: boom}).InstallBuiltin(context.Background(), "t", "u", "crm")
	if !errors.As(err, &ve) || ve.Code != "VERTICAL_PACK_EMPTY" {
		t.Errorf("install error = %v, want the installer's ValidationError preserved through %%w", err)
	}
}
