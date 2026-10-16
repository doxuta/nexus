package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/services"
)

type fakeAppNav struct{ items []services.AppNavItemView }

func (f fakeAppNav) GetNav(_ context.Context, _ services.RecordScope, _, name string) (*appmeta.App, []services.AppNavItemView, error) {
	if name != "clinic" {
		return nil, nil, &services.ValidationError{Code: "APP_NOT_FOUND", Msg: "không tìm thấy app"}
	}
	return &appmeta.App{Name: name}, f.items, nil
}

func TestChange_ProposeAppExistingIsUpdateKeepingMenu(t *testing.T) {
	docs := &DocReaders{Apps: fakeAppNav{items: []services.AppNavItemView{
		{Type: "dashboard", RefName: "tong_quan", Label: "Tổng quan"},
		{Type: "entity", RefName: "patients", Label: "Hồ sơ của tôi"},
	}}}
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A", docs: docs}
	args := `{"name":"clinic","label":"Phòng khám","navEntities":["patients"]}`
	if ok, m := parseToolResult(t, e.execute(context.Background(), "propose_app", args)); !ok {
		t.Fatalf("refused: %v", m)
	}
	c := lastChange(t, e)
	if c.Op != "update" || c.DocType != "app" {
		t.Fatalf("want update/app, got %+v", c)
	}
	var before, after struct {
		Nav []ContextNavItem `json:"nav"`
	}
	_ = json.Unmarshal(c.Before, &before)
	_ = json.Unmarshal(c.After, &after)
	if len(before.Nav) != 2 || len(after.Nav) != 2 || after.Nav[1].Label != "Hồ sơ của tôi" {
		t.Fatalf("menu must be kept as is (patients already there): %s -> %s", c.Before, c.After)
	}

	e2 := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A", docs: docs}
	e2.draft = nil
	_ = e2.execute(context.Background(), "propose_entity", `{"name":"visits","label":"Tái khám","fields":[{"name":"note","label":"Ghi chú","fieldType":"text"}]}`)
	if ok, m := parseToolResult(t, e2.execute(context.Background(), "propose_app", `{"name":"clinic","label":"Phòng khám","navEntities":["visits"]}`)); !ok {
		t.Fatalf("refused: %v", m)
	}
	c2 := lastChange(t, e2)
	if c2.Op != "update" || len(c2.ChangedPaths) != 1 || c2.ChangedPaths[0] != "nav[2]" || !strings.Contains(string(c2.After), "visits") {
		t.Fatalf("want update with nav[2], got %+v / %s", c2, c2.After)
	}
}

func TestChange_ProposeAppNewSlugIsCreate(t *testing.T) {
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A", docs: &DocReaders{Apps: fakeAppNav{}}}
	if ok, m := parseToolResult(t, e.execute(context.Background(), "propose_app", `{"name":"spa","label":"Spa","navEntities":["patients"]}`)); !ok {
		t.Fatalf("refused: %v", m)
	}
	if c := lastChange(t, e); c.Op != "create" || len(c.Before) != 0 {
		t.Fatalf("want create, got %+v", c)
	}
}
