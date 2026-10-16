package handlers

// specs/050 S3a (T057): every admin read of a rule — the list, one rule, and the rule echoed by a
// create or an update — carries no inline key, header value, userinfo password or query value. The
// mask is applied at this boundary only: the stored row (what the engine, the timer job and the
// dry-run read) keeps the real values.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// redactRouter wires the real handler + WorkflowService over repo, as the admin group does.
func redactRouter(repo *memWorkflowRepo) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewAdminWorkflowHandler(services.NewWorkflowService(repo), zap.NewNop())
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("tenantID", "t1"); c.Next() })
	h.RegisterRoutes(&r.RouterGroup)
	return r
}

// seedRow stores a rule as it sits in sys_workflows (saved before S3a, inline key included).
func seedRow(t *testing.T, repo *memWorkflowRepo, acts []workflow.Action) string {
	t.Helper()
	raw, err := json.Marshal(acts)
	if err != nil {
		t.Fatal(err)
	}
	id, err := repo.Create(context.Background(), workflow.CreateInput{TenantID: "t1", Name: "gọi đối tác",
		EntityName: "deals", Trigger: workflow.TriggerAfterCreate, Actions: raw, IsActive: true})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func serve(t *testing.T, r *gin.Engine, method, path, body string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path, bytes.NewBufferString(body)))
	return w.Code, w.Body.String()
}

func assertNoLeak(t *testing.T, what, body string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(body, s) {
			t.Fatalf("%s leaks %q: %s", what, s, body)
		}
	}
}

const (
	leakKey    = "partner-signing-key-9f3a"
	leakBearer = "Bearer tok-live-77c1"
	leakPass   = "pa55-word-e2"
	leakQuery  = "qtok-4b1d"
)

// TestAdminWorkflowHandler_RedactsInlineSecretOnRead: a rule saved before S3a with a signing key and
// an auth header reads back without either, on every route that returns a rule; the stored row is
// untouched.
func TestAdminWorkflowHandler_RedactsInlineSecretOnRead(t *testing.T) {
	repo := &memWorkflowRepo{rows: map[string]*workflow.WorkflowRecord{}}
	r := redactRouter(repo)
	id := seedRow(t, repo, []workflow.Action{{Type: workflow.ActionWebhook, URL: "https://erp.example.vn/hook",
		Secret: leakKey, Headers: map[string]string{"Authorization": leakBearer}}})
	stored := string(repo.rows[id].Actions)

	for _, path := range []string{"/admin/workflows", "/admin/workflows/" + id} {
		code, body := serve(t, r, http.MethodGet, path, "")
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, code, body)
		}
		assertNoLeak(t, "GET "+path, body, leakKey, "tok-live-77c1", `"secret"`)
		if !strings.Contains(body, `"Authorization":"`+workflow.RedactedMarker+`"`) || !strings.Contains(body, "https://erp.example.vn/hook") {
			t.Fatalf("GET %s: want the header named with a masked value and the url intact: %s", path, body)
		}
	}
	if string(repo.rows[id].Actions) != stored {
		t.Fatalf("a read changed the stored row:\n got %s\nwant %s", repo.rows[id].Actions, stored)
	}

	// Create and update echo the rule: a url with a password + a query token saves (the key in a URL is
	// only masked, spec Nợ 9) but the echo is masked while the row keeps the real value.
	target := "https://shop:" + leakPass + "@erp.example.vn/in?token=" + leakQuery
	body := `{"name":"echo","entityName":"deals","trigger":"after_create","actions":[{"type":"outbound_webhook","url":"` + target + `"}]}`
	code, resp := serve(t, r, http.MethodPost, "/admin/workflows", body)
	if code != http.StatusCreated {
		t.Fatalf("POST = %d %s", code, resp)
	}
	assertNoLeak(t, "POST echo", resp, leakPass, leakQuery)
	var created struct {
		Data struct {
			Workflow struct{ ID string } `json:"workflow"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(resp), &created); err != nil || created.Data.Workflow.ID == "" {
		t.Fatalf("decode POST: %v %s", err, resp)
	}
	newID := created.Data.Workflow.ID
	if !strings.Contains(string(repo.rows[newID].Actions), leakPass) || !strings.Contains(string(repo.rows[newID].Actions), leakQuery) {
		t.Fatalf("the stored row must keep the real url, got %s", repo.rows[newID].Actions)
	}
	code, resp = serve(t, r, http.MethodPut, "/admin/workflows/"+newID, strings.Replace(body, `"echo"`, `"echo 2"`, 1))
	if code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, resp)
	}
	assertNoLeak(t, "PUT echo", resp, leakPass, leakQuery)
	if !strings.Contains(string(repo.rows[newID].Actions), leakPass) {
		t.Fatalf("the stored row must keep the real url after PUT, got %s", repo.rows[newID].Actions)
	}
}

// TestAdminWorkflowHandler_RedactsLegacyURLCredentials: a url carrying a password and query tokens
// (and a vault path carrying one) reads back with the values masked and the names kept.
func TestAdminWorkflowHandler_RedactsLegacyURLCredentials(t *testing.T) {
	repo := &memWorkflowRepo{rows: map[string]*workflow.WorkflowRecord{}}
	r := redactRouter(repo)
	id := seedRow(t, repo, []workflow.Action{
		{Type: workflow.ActionWebhook, URL: "https://user:" + leakPass + "@127.0.0.1:9/x?token=" + leakQuery + "&src=nexus"},
		{Type: workflow.ActionWebhook, CredentialID: "cred-1", Path: "/orders?key=" + leakQuery},
	})
	code, body := serve(t, r, http.MethodGet, "/admin/workflows/"+id, "")
	if code != http.StatusOK {
		t.Fatalf("GET = %d %s", code, body)
	}
	assertNoLeak(t, "GET", body, leakPass, leakQuery, "src=nexus")
	var got struct {
		Data struct {
			Workflow struct {
				Actions []struct{ URL, Path string } `json:"actions"`
			} `json:"workflow"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	m := workflow.RedactedMarker
	acts := got.Data.Workflow.Actions
	if len(acts) != 2 || acts[0].URL != "https://"+m+"@127.0.0.1:9/x?token="+m+"&src="+m || acts[1].Path != "/orders?key="+m {
		t.Fatalf("actions = %+v; want %s@… ?token=%s&src=%s and /orders?key=%s", acts, m, m, m, m)
	}
}

// TestAdminWorkflowHandler_SetActive: PATCH /admin/workflows/:id/active pauses a rule that still holds
// a pre-vault key — the exit the Doctor C10 refusal names (a PUT of it is a 409) — leaves the stored
// actions byte for byte, answers with the masked rule, and stays tenant-pinned.
func TestAdminWorkflowHandler_SetActive(t *testing.T) {
	repo := &memWorkflowRepo{rows: map[string]*workflow.WorkflowRecord{}}
	r := redactRouter(repo)
	id := seedRow(t, repo, []workflow.Action{{Type: workflow.ActionWebhook, URL: "https://erp.example.vn/hook",
		Secret: leakKey, Headers: map[string]string{"Authorization": leakBearer}}})
	stored := string(repo.rows[id].Actions)

	put := `{"name":"gọi đối tác","entityName":"deals","trigger":"after_create","isActive":false,` +
		`"actions":[{"type":"outbound_webhook","url":"https://erp.example.vn/hook"}]}`
	if code, body := serve(t, r, http.MethodPut, "/admin/workflows/"+id, put); code != http.StatusConflict {
		t.Fatalf("the builder's PUT of a keyed rule = %d %s, want 409", code, body)
	}

	cases := []struct {
		name, id, body string
		want           int
	}{
		{"pause keyed rule", id, `{"isActive":false}`, http.StatusOK},
		{"resume", id, `{"isActive":true}`, http.StatusOK},
		{"unknown id", "nope", `{"isActive":false}`, http.StatusNotFound},
		{"missing flag", id, `{}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := serve(t, r, http.MethodPatch, "/admin/workflows/"+tc.id+"/active", tc.body)
			if code != tc.want {
				t.Fatalf("PATCH = %d %s, want %d", code, body, tc.want)
			}
			if code == http.StatusOK {
				assertNoLeak(t, "PATCH echo", body, leakKey, "tok-live-77c1")
			}
		})
	}
	if string(repo.rows[id].Actions) != stored {
		t.Fatalf("the switch changed the stored actions:\n got %s\nwant %s", repo.rows[id].Actions, stored)
	}
	if code, _ := serve(t, r, http.MethodPatch, "/admin/workflows/"+id+"/active", `{"isActive":false}`); code != http.StatusOK || repo.rows[id].IsActive {
		t.Fatalf("pause: code=%d active=%v", code, repo.rows[id].IsActive)
	}
}
