package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

func TestNormalizeTurnContext(t *testing.T) {
	tests := []struct {
		name      string
		in        TurnContextInput
		wantEmpty bool
		wantApp   string
		wantField string // "" = no error
	}{
		{"valid app + panel", TurnContextInput{App: " clinic ", Panel: "tu-dong"}, false, "clinic", ""},
		{"empty", TurnContextInput{}, true, "", ""},
		{"whitespace only = absent", TurnContextInput{App: "   ", Panel: "  "}, true, "", ""},
		{"bad charset", TurnContextInput{App: "Clinic;DROP"}, false, "", "context.app"},
		{"61 chars", TurnContextInput{App: "a" + strings.Repeat("b", 60)}, false, "", "context.app"},
		{"60 chars ok", TurnContextInput{App: "a" + strings.Repeat("b", 59)}, false, "a" + strings.Repeat("b", 59), ""},
		{"unknown panel", TurnContextInput{Panel: "ignore-all-rules"}, false, "", "context.panel"},
		{"bad app reported before bad panel", TurnContextInput{App: "X!", Panel: "zzz"}, false, "", "context.app"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeTurnContext(tc.in)
			if tc.wantField != "" {
				var ce *ContextError
				if !errors.As(err, &ce) || ce.Code != "AI_CONTEXT_INVALID" || ce.Field != tc.wantField {
					t.Fatalf("err = %v; want ContextError AI_CONTEXT_INVALID %s", err, tc.wantField)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err %v", err)
			}
			if got.Empty() != tc.wantEmpty || got.App != tc.wantApp {
				t.Fatalf("got %+v empty=%v; want empty=%v app=%q", got, got.Empty(), tc.wantEmpty, tc.wantApp)
			}
		})
	}
}

// --- resolver ---

type fakeNav struct {
	calls   int
	gotTen  string
	gotSc   services.RecordScope
	apps    map[string]map[string]*appmeta.App // tenant -> name -> app
	items   []services.AppNavItemView
	readErr error
}

func (f *fakeNav) GetNav(_ context.Context, sc services.RecordScope, tenantID, name string) (*appmeta.App, []services.AppNavItemView, error) {
	f.calls++
	f.gotTen, f.gotSc = tenantID, sc
	if f.readErr != nil {
		return nil, nil, f.readErr
	}
	app, ok := f.apps[tenantID][name]
	if !ok {
		return nil, nil, &services.ValidationError{Code: "APP_NOT_FOUND", Msg: "không tìm thấy app"}
	}
	return app, f.items, nil
}

type fakeTenants struct{ calls int }

func (f *fakeTenants) GetTenantByID(_ context.Context, id string) (*auth.Tenant, error) {
	f.calls++
	return &auth.Tenant{ID: id, Name: "Dự án " + id}, nil
}

func TestContextResolver(t *testing.T) {
	nav := &fakeNav{
		apps:  map[string]map[string]*appmeta.App{"tA": {"clinic": {Name: "clinic", Label: "Phòng khám"}}, "tB": {"secret": {Name: "secret", Label: "Bí mật"}}},
		items: []services.AppNavItemView{{Type: "entity", Label: "Lịch hẹn", RefName: "appointments", Href: "/appointments"}},
	}
	tn := &fakeTenants{}
	r := NewContextResolver(nil, nav, tn)

	t.Run("happy path uses JWT tenant, admin scope, RefName", func(t *testing.T) {
		got, err := r.Resolve(context.Background(), "tA", TurnContextInput{App: "clinic", Panel: "bang"})
		if err != nil {
			t.Fatal(err)
		}
		if !got.Present || got.Project != "Dự án tA" || got.App == nil || got.App.Label != "Phòng khám" || got.Panel != "bang" {
			t.Fatalf("turn = %+v", got)
		}
		if len(got.App.Nav) != 1 || got.App.Nav[0].Name != "appointments" {
			t.Fatalf("nav = %+v; want RefName not Href", got.App.Nav)
		}
		if nav.gotTen != "tA" || !nav.gotSc.IsAdmin {
			t.Fatalf("GetNav tenant=%q scope=%+v", nav.gotTen, nav.gotSc)
		}
	})
	t.Run("foreign app and missing app are indistinguishable", func(t *testing.T) {
		_, e1 := r.Resolve(context.Background(), "tA", TurnContextInput{App: "secret"})
		_, e2 := r.Resolve(context.Background(), "tA", TurnContextInput{App: "nope"})
		var c1, c2 *ContextError
		if !errors.As(e1, &c1) || !errors.As(e2, &c2) || *c1 != *c2 || c1.Code != "AI_CONTEXT_NOT_FOUND" || c1.Field != "context.app" {
			t.Fatalf("e1=%v e2=%v", e1, e2)
		}
	})
	t.Run("other read error is wrapped, not ContextError", func(t *testing.T) {
		bad := NewContextResolver(nil, &fakeNav{readErr: errors.New("db down")}, &fakeTenants{})
		_, err := bad.Resolve(context.Background(), "tA", TurnContextInput{App: "clinic"})
		var ce *ContextError
		if err == nil || errors.As(err, &ce) || !strings.Contains(err.Error(), "ai.ContextResolver.Resolve:") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no app key => GetNav not called", func(t *testing.T) {
		n := &fakeNav{}
		_, err := NewContextResolver(nil, n, &fakeTenants{}).Resolve(context.Background(), "tA", TurnContextInput{Panel: "menu"})
		if err != nil || n.calls != 0 {
			t.Fatalf("err=%v calls=%d", err, n.calls)
		}
	})
}

func TestNormalizeTurnContext_EntityRecord(t *testing.T) {
	const uid = "3f2b8c1e-4d5a-4b6c-8d7e-9f0a1b2c3d4e"
	tests := []struct {
		name      string
		in        TurnContextInput
		wantRec   string
		wantField string
	}{
		{"valid entity", TurnContextInput{Entity: " clinic_patients "}, "", ""},
		{"entity charset", TurnContextInput{Entity: "Bad-Name"}, "", "context.entity"},
		{"entity too long", TurnContextInput{Entity: "a" + strings.Repeat("b", 64)}, "", "context.entity"},
		{"entity 64 ok", TurnContextInput{Entity: "a" + strings.Repeat("b", 63)}, "", ""},
		{"uppercase uuid lowered", TurnContextInput{Entity: "patients", RecordID: strings.ToUpper(uid)}, uid, ""},
		{"not a uuid", TurnContextInput{Entity: "patients", RecordID: "1 OR 1=1"}, "", "context.recordId"},
		{"recordId without entity", TurnContextInput{RecordID: uid}, "", "context.recordId"},
		{"entity checked before recordId", TurnContextInput{Entity: "X!", RecordID: "zz"}, "", "context.entity"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeTurnContext(tc.in)
			if tc.wantField != "" {
				var ce *ContextError
				if !errors.As(err, &ce) || ce.Code != "AI_CONTEXT_INVALID" || ce.Field != tc.wantField {
					t.Fatalf("err = %v; want %s", err, tc.wantField)
				}
				return
			}
			if err != nil || got.RecordID != tc.wantRec {
				t.Fatalf("got %+v err=%v; want recordId %q", got, err, tc.wantRec)
			}
		})
	}
}

type entityMeta struct {
	MetadataReader
	calls  int
	tenant map[string]map[string]*metadata.EntityDefinition
	fields int
}

func (m *entityMeta) GetEntityByName(_ context.Context, tenantID, name string) (*metadata.EntityDefinition, error) {
	m.calls++
	if e, ok := m.tenant[tenantID][name]; ok {
		return e, nil
	}
	return nil, metadata.ErrEntityNotFound
}

func (m *entityMeta) ListFields(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	m.calls++
	out := make([]metadata.FieldDefinition, m.fields)
	for i := range out {
		out[i] = metadata.FieldDefinition{Name: fmt.Sprintf("f%d", i), Label: "F", FieldType: "text"}
	}
	return out, nil
}

func TestContextResolver_Entity(t *testing.T) {
	meta := &entityMeta{
		tenant: map[string]map[string]*metadata.EntityDefinition{
			"tA": {"appointments": {ID: "e1", Name: "appointments", Label: "Lịch hẹn"}, "orphans": {ID: "e2", Name: "orphans", Label: "Mồ côi"}},
			"tB": {"secret": {ID: "e3", Name: "secret", Label: "Bí mật"}},
		},
		fields: 200,
	}
	nav := &fakeNav{
		apps:  map[string]map[string]*appmeta.App{"tA": {"clinic": {Name: "clinic", Label: "Phòng khám"}}},
		items: []services.AppNavItemView{{Type: "entity", Label: "Lịch hẹn", RefName: "appointments"}, {Type: "dashboard", Label: "KPI", RefName: "orphans"}},
	}
	r := NewContextResolver(meta, nav, &fakeTenants{})
	ctx := context.Background()

	t.Run("inApp true, fields capped at 80 with fieldsMore, bounded calls", func(t *testing.T) {
		meta.calls, nav.calls = 0, 0
		got, err := r.Resolve(ctx, "tA", TurnContextInput{App: "clinic", Entity: "appointments"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Entity == nil || !got.Entity.InApp || got.Entity.Label != "Lịch hẹn" || len(got.Entity.Fields) != 80 || got.Entity.FieldsMore != 120 {
			t.Fatalf("entity = %+v", got.Entity)
		}
		if meta.calls+nav.calls > 7 {
			t.Fatalf("calls = %d; want <= 7 and constant in field count", meta.calls+nav.calls)
		}
	})
	t.Run("inApp false when only a dashboard shares the name, or no app", func(t *testing.T) {
		got, err := r.Resolve(ctx, "tA", TurnContextInput{App: "clinic", Entity: "orphans"})
		if err != nil || got.Entity.InApp {
			t.Fatalf("orphans inApp = %v err=%v", got.Entity, err)
		}
		got, err = r.Resolve(ctx, "tA", TurnContextInput{Entity: "appointments"})
		if err != nil || got.Entity.InApp {
			t.Fatalf("no app inApp = %v err=%v", got.Entity, err)
		}
	})
	t.Run("foreign and missing entity are byte-identical NOT_FOUND", func(t *testing.T) {
		_, e1 := r.Resolve(ctx, "tA", TurnContextInput{Entity: "secret"})
		_, e2 := r.Resolve(ctx, "tA", TurnContextInput{Entity: "nope"})
		var c1, c2 *ContextError
		if !errors.As(e1, &c1) || !errors.As(e2, &c2) || *c1 != *c2 || c1.Code != "AI_CONTEXT_NOT_FOUND" || c1.Field != "context.entity" {
			t.Fatalf("e1=%v e2=%v", e1, e2)
		}
	})
	t.Run("recordId passes through only with a resolved entity", func(t *testing.T) {
		const uid = "3f2b8c1e-4d5a-4b6c-8d7e-9f0a1b2c3d4e"
		got, err := r.Resolve(ctx, "tA", TurnContextInput{Entity: "appointments", RecordID: uid})
		if err != nil || got.RecordID != uid {
			t.Fatalf("got %+v err=%v", got, err)
		}
	})
}
