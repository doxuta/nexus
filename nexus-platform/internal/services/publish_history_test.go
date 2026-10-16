package services

// publish_history_test.go — specs/063-draft-publish S7-6 (T097-T099): changedPaths, redaction of
// version documents, and the boundary of the two history reads.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/metaversion"
)

func TestChangedPaths(t *testing.T) {
	tests := []struct {
		name, typ, before, after string
		want                     []string
		more                     int
	}{
		{"saved as-is reads empty", "workflow",
			`{"name":"r","entityName":"p","trigger":"after_create","actions":[{"type":"x"}]}`,
			`{"name":"r","entityName":"p","trigger":"after_create","actions":[{"type":"x"}],"isActive":true,"intervalSeconds":0}`, nil, 0},
		{"one field", "workflow", `{"name":"a","isActive":true}`, `{"name":"b","isActive":true}`, []string{"name"}, 0},
		{"create has no before", "workflow", ``, `{"name":"b"}`, nil, 0},
		{"delete has no after", "workflow", `{"name":"a"}`, ``, nil, 0},
		{"unreadable body is not an error", "workflow", `{bad`, `{"name":"b"}`, nil, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, more := changedPaths(tc.typ, json.RawMessage(tc.before), json.RawMessage(tc.after))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") || more != tc.more {
				t.Fatalf("got %v +%d, want %v +%d", got, more, tc.want, tc.more)
			}
		})
	}
}

// Over the cap: 50 kept, the rest counted (SC-006).
func TestChangedPaths_Cap(t *testing.T) {
	var a, b strings.Builder
	a.WriteString("{")
	b.WriteString("{")
	for i := 0; i < 60; i++ {
		sep := ","
		if i == 59 {
			sep = ""
		}
		a.WriteString(`"k` + strings.Repeat("a", i) + `":1` + sep)
		b.WriteString(`"k` + strings.Repeat("a", i) + `":2` + sep)
	}
	a.WriteString("}")
	b.WriteString("}")
	got, more := changedPaths("report", json.RawMessage(a.String()), json.RawMessage(b.String()))
	if len(got) != 50 || more != 10 {
		t.Fatalf("got %d paths +%d, want 50 +10", len(got), more)
	}
}

// A webhook key never leaves through versions/*, in either side of a doc; an unknown type's body is
// never echoed (T098).
func TestVersionDocView_RedactsSecrets(t *testing.T) {
	reg := metaversion.NewRegistry(NewWorkflowDocAdapter())
	wf := `{"name":"r","entityName":"p","trigger":"after_create","isActive":true,"intervalSeconds":0,` +
		`"actions":[{"type":"outbound_webhook","url":"https://x.test","secret":"TOP-SECRET-KEY","headers":{"Authorization":"Bearer ABC123"}}]}`
	wf2 := strings.Replace(wf, "https://x.test", "https://y.test", 1)
	d := metaversion.VersionDoc{DocType: "workflow", DocKey: "w1", Label: "r", Op: "update", BeforeJSON: json.RawMessage(wf), AfterJSON: json.RawMessage(wf2)}
	v := versionDocView(reg, d)
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"TOP-SECRET-KEY", "Bearer ABC123"} {
		if strings.Contains(string(out), leak) {
			t.Fatalf("view leaks %q: %s", leak, out)
		}
	}
	if len(v.ChangedPaths) == 0 {
		t.Fatal("a url edit should be listed in changedPaths")
	}

	unk := versionDocView(reg, metaversion.VersionDoc{DocType: "mystery", DocKey: "k", Op: "update", AfterJSON: json.RawMessage(`{"secret":"S"}`)})
	if b, _ := json.Marshal(unk); strings.Contains(string(b), `"S"`) {
		t.Fatalf("unknown type body echoed: %s", b)
	}
}

// Both reads: builder only, and an id that is not a UUID is the same 404 as an unknown or foreign one,
// decided before any SQL.
func TestPublishService_HistoryBoundary(t *testing.T) {
	s := nilDepsPublishService()
	for name, ctx := range map[string]context.Context{
		"no caller": context.Background(), "legacy customer admin": legacyAdminCtx(),
		"builder of another tenant": metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: "b", TenantID: "other", IsBuilder: true}),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := s.ListVersions(ctx, dwTenant, 20, 0)
			wantCode(t, err, "FORBIDDEN")
			_, err = s.GetVersion(ctx, dwTenant, "00000000-0000-0000-0000-000000000001")
			wantCode(t, err, "FORBIDDEN")
		})
	}
	_, err := s.GetVersion(builderCtx(), dwTenant, "not-a-uuid")
	wantCode(t, err, "VERSION_NOT_FOUND")
}
