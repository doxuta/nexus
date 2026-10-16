package vertical_pack

// DAILY-QUEUE #720 (4/4 of #295): SHARED saved views travel with the pack; personal ones never do.

import (
	"context"
	"encoding/json"
	"testing"

	"nexus/platform/internal/services"
	"nexus/platform/internal/view"
)

type fakeViewStore struct {
	rows    []view.Record
	created []services.CreateViewInput
	admin   []bool
}

func (f *fakeViewStore) List(_ context.Context, _, userID, entity string) ([]view.Record, error) {
	var out []view.Record
	for _, r := range f.rows {
		if r.EntityName == entity && (r.Shared || (r.UserID != nil && *r.UserID == userID)) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeViewStore) Create(_ context.Context, _, _ string, isAdmin bool, in services.CreateViewInput) (string, error) {
	f.created = append(f.created, in)
	f.admin = append(f.admin, isAdmin)
	f.rows = append(f.rows, view.Record{EntityName: in.EntityName, Name: in.Name, Shared: in.Shared, Config: in.Config})
	return "v", nil
}

func TestPackViews_SharedTravelPersonalStays(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMintMeta{}
	if _, _, err := meta.CreateEntity(ctx, "src", services.CreateEntityInput{Name: "vw_jobs", Label: "Jobs"}); err != nil {
		t.Fatal(err)
	}
	owner := "u1"
	src := &fakeViewStore{rows: []view.Record{
		{EntityName: "vw_jobs", Name: "Đang mở", Shared: true, Config: json.RawMessage(`{"filters":[{"field":"status","op":"eq","value":"open"}]}`)},
		{EntityName: "vw_jobs", Name: "Của tôi", Shared: false, UserID: &owner, Config: json.RawMessage(`{}`)},
		{EntityName: "other", Name: "Ngoài phạm vi", Shared: true, Config: json.RawMessage(`{}`)},
	}}
	pack, _, err := NewService(Deps{EntityReader: meta, LayoutReader: fakeApLayouts{}, RoleReader: fakeMintRoles{}, ViewStore: src}).
		Export(ctx, "src", []string{"vw_jobs"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Views) != 1 || pack.Views[0].Name != "Đang mở" {
		t.Fatalf("views = %+v, want only the shared one of the selected entity", pack.Views)
	}

	var wire Pack
	if err := json.Unmarshal(mustJSON(t, pack), &wire); err != nil {
		t.Fatal(err)
	}
	dst := &fakeViewStore{}
	res := &InstallResult{}
	svc := NewService(Deps{ViewStore: dst})
	if err := svc.installViews(ctx, "dst", "admin", &wire, res); err != nil {
		t.Fatal(err)
	}
	if len(dst.created) != 1 || !dst.created[0].Shared || !dst.admin[0] || string(dst.created[0].Config) != string(pack.Views[0].Config) {
		t.Fatalf("created = %+v admin=%v", dst.created, dst.admin)
	}
	// re-install: already present by (entity, name) -> nothing new
	if err := svc.installViews(ctx, "dst", "admin", &wire, &InstallResult{}); err != nil || len(dst.created) != 1 {
		t.Fatalf("re-install must skip: err=%v created=%d", err, len(dst.created))
	}
}
