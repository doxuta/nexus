package services

// specs/050 S2a (T030) — the old workflow builder only knows {type, url}; saving a rule from it
// used to replace the actions wholesale and silently drop a signing key / auth header that had
// been set on the action by API. The update path now refuses that drop (409) so a partner stops
// receiving unauthenticated calls only when someone decided it.
//
// specs/050 S3a (T058) — no write path accepts an inline key any more (422
// WEBHOOK_INLINE_SECRET_REMOVED), and a url/path still carrying the read-side mask is refused (422
// WEBHOOK_REDACTED_VALUE) instead of overwriting the real value. Rules that still carry an inline
// key therefore exist only as rows saved before S3a: the fixtures below write them straight into the
// store, the way they sit in sys_workflows. All fakes, no DB.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/workflow"
)

// seedLegacyRule stores acts as a rule saved before S3a (no write path accepts an inline key now)
// and returns its id.
func seedLegacyRule(t *testing.T, repo *fakeWFRepo, acts []workflow.Action) string {
	t.Helper()
	raw, err := json.Marshal(acts)
	if err != nil {
		t.Fatalf("marshal legacy actions: %v", err)
	}
	id, err := repo.Create(context.Background(), workflow.CreateInput{
		TenantID: "t1", Name: "gọi đối tác", EntityName: "deals", Trigger: workflow.TriggerAfterCreate,
		Actions: raw, IsActive: true,
	})
	if err != nil {
		t.Fatalf("seed legacy rule: %v", err)
	}
	return id
}

func putActions(svc *WorkflowService, id string, acts []WorkflowActionInput) error {
	_, err := svc.UpdateWorkflow(context.Background(), "t1", id, UpdateWorkflowInput{
		Name: "gọi đối tác", EntityName: "deals", Trigger: "after_create", Actions: acts,
	})
	return err
}

func postActions(svc *WorkflowService, acts []WorkflowActionInput) error {
	_, err := svc.CreateWorkflow(context.Background(), "t1", CreateWorkflowInput{
		Name: "gọi đối tác mới", EntityName: "deals", Trigger: "after_create", Actions: acts,
	})
	return err
}

const legacyHook = "https://erp.example.vn/hook"

// signedLegacy is a webhook action as the API stored it before S3a: its own signing key + header.
func signedLegacy() workflow.Action {
	return workflow.Action{Type: workflow.ActionWebhook, URL: legacyHook, Secret: "partner-signing-key",
		Headers: map[string]string{"Authorization": "Bearer partner-token"}}
}

func bareHook(u string) WorkflowActionInput {
	return WorkflowActionInput{Type: "outbound_webhook", URL: u}
}

// TestUpdateWorkflow_RefusesSilentLegacySecretDrop: the builder's shape — the same URL, no key —
// is refused with 409 WEBHOOK_LEGACY_SECRET_PENDING pointing at the action, and the stored actions
// are left byte for byte. A key-only or a header-only action counts the same. Since S3a the way out
// the message names is the vault (or the Doctor step that moves the key there) — sending the key
// again is itself refused.
func TestUpdateWorkflow_RefusesSilentLegacySecretDrop(t *testing.T) {
	cases := []struct {
		name   string
		stored []workflow.Action
		put    []WorkflowActionInput
		field  string
	}{
		{"builder resave: same url, no key", []workflow.Action{signedLegacy()},
			[]WorkflowActionInput{bareHook(legacyHook)}, "actions[0].url"},
		{"surrounding whitespace is the same url", []workflow.Action{signedLegacy()},
			[]WorkflowActionInput{{Type: " outbound_webhook ", URL: "  " + legacyHook + " "}}, "actions[0].url"},
		{"signing key only", []workflow.Action{{Type: workflow.ActionWebhook, URL: legacyHook, Secret: "k"}},
			[]WorkflowActionInput{bareHook(legacyHook)}, "actions[0].url"},
		{"auth header only", []workflow.Action{{Type: workflow.ActionWebhook, URL: legacyHook, Headers: map[string]string{"X-Api-Key": "abc"}}},
			[]WorkflowActionInput{bareHook(legacyHook)}, "actions[0].url"},
		{"action moved to another slot", []workflow.Action{signedLegacy()},
			[]WorkflowActionInput{
				{Type: "field_update", Field: "probability", Value: 10.0},
				bareHook(legacyHook),
			}, "actions[1].url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeWFRepo()
			svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog())
			id := seedLegacyRule(t, repo, tc.stored)
			before := append([]byte(nil), repo.rows[id].Actions...)

			ve := wantBeforeCode(t, putActions(svc, id, tc.put), "WEBHOOK_LEGACY_SECRET_PENDING")
			if ve.Details["field"] != tc.field {
				t.Fatalf("details.field = %v, want %s", ve.Details["field"], tc.field)
			}
			if !strings.Contains(ve.Msg, "Kho khoá kết nối") || !strings.Contains(ve.Msg, "Bác sĩ hệ thống") ||
				strings.Contains(ve.Msg, "Gửi kèm lại khoá") {
				t.Fatalf("message = %q; want the way out since S3a (Kho khoá kết nối / Bác sĩ hệ thống), not \"send the key again\"", ve.Msg)
			}
			if !bytes.Equal(repo.rows[id].Actions, before) {
				t.Fatalf("a refused save must not touch the stored actions:\n got %s\nwant %s", repo.rows[id].Actions, before)
			}
		})
	}
}

// TestUpdateWorkflow_LegacyChangeURLOrDeleteAllowed: the guard only catches the silent drop. A
// deliberate change — another URL, removing the action, a rule that never had a key — saves.
func TestUpdateWorkflow_LegacyChangeURLOrDeleteAllowed(t *testing.T) {
	cases := []struct {
		name   string
		stored []workflow.Action
		put    []WorkflowActionInput
	}{
		{"url changed", []workflow.Action{signedLegacy()}, []WorkflowActionInput{bareHook("https://erp.example.vn/hook-v2")}},
		{"webhook action removed", []workflow.Action{signedLegacy()},
			[]WorkflowActionInput{{Type: "field_update", Field: "probability", Value: 10.0}}},
		{"rule never had a key", []workflow.Action{{Type: workflow.ActionWebhook, URL: legacyHook}},
			[]WorkflowActionInput{bareHook(legacyHook)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeWFRepo()
			svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog())
			id := seedLegacyRule(t, repo, tc.stored)
			if err := putActions(svc, id, tc.put); err != nil {
				t.Fatalf("a deliberate change must save, got %v", err)
			}
			rec, err := repo.Get(context.Background(), "t1", id)
			if err != nil {
				t.Fatal(err)
			}
			wf, derr := rec.ToWorkflow()
			if derr != nil {
				t.Fatalf("decode saved rule: %v", derr)
			}
			if len(wf.Actions) != len(tc.put) {
				t.Fatalf("saved %d actions, want %d", len(wf.Actions), len(tc.put))
			}
		})
	}

	t.Run("unknown rule stays 404", func(t *testing.T) {
		svc := NewWorkflowService(newFakeWFRepo()).WithEntityChecker(beforeCatalog())
		wantBeforeCode(t, putActions(svc, "wf-missing", []WorkflowActionInput{bareHook(legacyHook)}), "WORKFLOW_NOT_FOUND")
	})
}

// TestCreateWorkflow_RejectsInlineSecret (S3a T058): a signing key or a header on the action itself
// is refused on every write path — POST and PUT on the admin API (the pack installer calls the same
// CreateWorkflow after stripping the pack's keys, T059), and the AI Builder's config-time check
// (ValidateWorkflowConfig) — with details.field on the offending key, and nothing is written.
func TestCreateWorkflow_RejectsInlineSecret(t *testing.T) {
	cases := []struct {
		name  string
		acts  []WorkflowActionInput
		field string
	}{
		{"signing key", []WorkflowActionInput{{Type: "outbound_webhook", URL: legacyHook, Secret: "partner-signing-key"}}, "actions[0].secret"},
		{"auth header", []WorkflowActionInput{{Type: "outbound_webhook", URL: legacyHook,
			Headers: map[string]string{"Authorization": "Bearer partner-token"}}}, "actions[0].headers"},
		{"key + header, second slot", []WorkflowActionInput{
			{Type: "field_update", Field: "probability", Value: 10.0},
			{Type: "outbound_webhook", URL: legacyHook, Secret: "k", Headers: map[string]string{"X-Api-Key": "abc"}},
		}, "actions[1].secret"},
		{"a harmless-looking header is still an inline header", []WorkflowActionInput{{Type: "outbound_webhook", URL: legacyHook,
			Headers: map[string]string{"X-Source": "nexus"}}}, "actions[0].headers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			check := func(path string, err error) {
				t.Helper()
				ve := wantBeforeCode(t, err, "WEBHOOK_INLINE_SECRET_REMOVED")
				if ve.Details["field"] != tc.field {
					t.Fatalf("%s: details.field = %v, want %s", path, ve.Details["field"], tc.field)
				}
				if !strings.Contains(ve.Msg, "Kho khoá kết nối") {
					t.Fatalf("%s: message = %q; want it to point at the vault (Kho khoá kết nối)", path, ve.Msg)
				}
			}

			repo := newFakeWFRepo()
			svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog())
			check("POST", postActions(svc, tc.acts))
			if len(repo.created) != 0 {
				t.Fatalf("POST: a refused create wrote %d rule(s)", len(repo.created))
			}

			id := seedLegacyRule(t, repo, []workflow.Action{signedLegacy()})
			before := append([]byte(nil), repo.rows[id].Actions...)
			check("PUT", putActions(svc, id, tc.acts))
			if !bytes.Equal(repo.rows[id].Actions, before) {
				t.Fatalf("PUT: a refused save must not touch the stored actions:\n got %s\nwant %s", repo.rows[id].Actions, before)
			}

			check("AI config-time", ValidateWorkflowConfig("deals", "after_create", nil, tc.acts, nil))
		})
	}
}

// TestUpdateWorkflow_RedactedRoundTripRejected (S3a T058): GET /admin/workflows shows a masked copy
// (RedactActionsJSON), and the old builder sends url/path back verbatim. A save still carrying the
// mask is refused — 422, the stored value untouched — instead of replacing the real token with
// "••••" and breaking the webhook silently. The S2a 409 guard cannot catch it: the masked URL is
// not the stored URL.
func TestUpdateWorkflow_RedactedRoundTripRejected(t *testing.T) {
	cases := []struct {
		name   string
		stored workflow.Action
		put    WorkflowActionInput
		code   string
		field  string
	}{
		{"legacy url, query token masked",
			workflow.Action{Type: workflow.ActionWebhook, URL: "https://erp.example.vn/x?token=abc&src=nexus"},
			bareHook("https://erp.example.vn/x?token=" + workflow.RedactedMarker + "&src=" + workflow.RedactedMarker),
			"WEBHOOK_REDACTED_VALUE", "actions[0].url"},
		{"legacy url, userinfo masked",
			workflow.Action{Type: workflow.ActionWebhook, URL: "https://shop:pa55@erp.example.vn/x"},
			bareHook("https://" + workflow.RedactedMarker + "@erp.example.vn/x"),
			"WEBHOOK_REDACTED_VALUE", "actions[0].url"},
		// The stored path under ANOTHER key: the mask is not that key's path, so it is refused
		// (the same key's masked path round-trips — TestUpdateWorkflow_KeepsStoredVaultPathBehindMask).
		{"vault path, query value masked, another key",
			workflow.Action{Type: workflow.ActionWebhook, CredentialID: "cred-2", Path: "/orders?key=real-key"},
			vaultHook("cred-1", "/orders?key="+workflow.RedactedMarker),
			"WEBHOOK_REDACTED_VALUE", "actions[0].path"},
		// A client that re-serialises the URL (new URL(x).toString(), a query-string builder) sends the
		// mask percent-encoded; it is still the mask, not a value.
		{"legacy url, query mask percent-encoded",
			workflow.Action{Type: workflow.ActionWebhook, URL: "https://erp.example.vn/x?token=abc"},
			bareHook("https://erp.example.vn/x?token=%E2%80%A2%E2%80%A2%E2%80%A2%E2%80%A2"),
			"WEBHOOK_REDACTED_VALUE", "actions[0].url"},
		{"vault path, query mask percent-encoded in lower-case hex, another key",
			workflow.Action{Type: workflow.ActionWebhook, CredentialID: "cred-2", Path: "/orders?key=real-key"},
			vaultHook("cred-1", "/orders?key=%e2%80%a2%e2%80%a2%e2%80%a2%e2%80%a2"),
			"WEBHOOK_REDACTED_VALUE", "actions[0].path"},
		{"the whole GET body sent back: header masked too",
			signedLegacy(),
			WorkflowActionInput{Type: "outbound_webhook", URL: legacyHook, Headers: map[string]string{"Authorization": workflow.RedactedMarker}},
			"WEBHOOK_INLINE_SECRET_REMOVED", "actions[0].headers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeWFRepo()
			svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog()).WithCredentialChecker(vaultCreds())
			id := seedLegacyRule(t, repo, []workflow.Action{tc.stored})
			before := append([]byte(nil), repo.rows[id].Actions...)

			ve := wantBeforeCode(t, putActions(svc, id, []WorkflowActionInput{tc.put}), tc.code)
			if ve.Details["field"] != tc.field {
				t.Fatalf("details.field = %v, want %s", ve.Details["field"], tc.field)
			}
			if !bytes.Equal(repo.rows[id].Actions, before) {
				t.Fatalf("the real value was overwritten:\n got %s\nwant %s", repo.rows[id].Actions, before)
			}
			// POST carries the same body through the same validator.
			wantBeforeCode(t, postActions(svc, []WorkflowActionInput{tc.put}), tc.code)
		})
	}

	t.Run("a url that merely mentions a dot is not the mask", func(t *testing.T) {
		repo := newFakeWFRepo()
		svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog())
		if err := postActions(svc, []WorkflowActionInput{bareHook("https://erp.example.vn/x?token=a.b.c.d")}); err != nil {
			t.Fatalf("a real query value must save, got %v", err)
		}
	})
}

// TestUpdateWorkflow_KeepsStoredVaultPathBehindMask (specs/050 S3b review): GET masks the query
// values of a vault action's path, and the builder shows that path read-only and sends it back as it
// read it (WorkflowBuilder: {credentialId, path} kept). Doctor C10 moves legacy URLs with a query into
// exactly that shape, so without this every later "Cập nhật" of such a rule was a 422 with no way out
// (the path cannot be retyped, and the action already uses a key). The same key + the mask of the
// stored path ⇒ the stored path is kept, byte for byte; anything else still meets the refusal.
func TestUpdateWorkflow_KeepsStoredVaultPathBehindMask(t *testing.T) {
	const real = "/legacy?src=nexus&k=v1"
	creds := vaultCreds()
	creds.views["t1|cred-2"] = &CredentialView{ID: "cred-2", Name: "khac", BaseURL: "https://other.example.vn", AuthType: "hmac", Status: "ready"}

	// What the builder sends back: the rule as GET shows it, re-saved (a rename, a condition edit…).
	readBack := func(t *testing.T, stored json.RawMessage) []WorkflowActionInput {
		t.Helper()
		var acts []workflow.Action
		if err := json.Unmarshal(workflow.RedactActionsJSON(stored), &acts); err != nil {
			t.Fatal(err)
		}
		out := make([]WorkflowActionInput, len(acts))
		for i, a := range acts {
			out[i] = vaultHook(a.CredentialID, a.Path)
		}
		return out
	}
	pathsOf := func(t *testing.T, raw json.RawMessage) []string {
		t.Helper()
		var acts []workflow.Action
		if err := json.Unmarshal(raw, &acts); err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(acts))
		for i, a := range acts {
			out[i] = a.CredentialID + " " + a.Path
		}
		return out
	}

	t.Run("the builder re-saves a moved rule ⇒ saved, real path kept", func(t *testing.T) {
		repo := newFakeWFRepo()
		svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog()).WithCredentialChecker(creds)
		id := seedLegacyRule(t, repo, []workflow.Action{
			{Type: workflow.ActionWebhook, CredentialID: "cred-1", Path: real},
			{Type: workflow.ActionWebhook, CredentialID: "cred-1", Path: "/other?k=v2"},
		})
		back := readBack(t, repo.rows[id].Actions)
		if !strings.Contains(back[0].Path, workflow.RedactedMarker) {
			t.Fatalf("fixture: the read must mask the query, got %q", back[0].Path)
		}
		if err := putActions(svc, id, back); err != nil {
			t.Fatalf("re-save of the read-back rule = %v; want saved", err)
		}
		if got := pathsOf(t, repo.rows[id].Actions); got[0] != "cred-1 "+real || got[1] != "cred-1 /other?k=v2" {
			t.Fatalf("stored = %v; want the real paths kept", got)
		}
	})

	t.Run("the mask percent-encoded by a re-serialising client ⇒ real path kept too", func(t *testing.T) {
		repo := newFakeWFRepo()
		svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog()).WithCredentialChecker(creds)
		id := seedLegacyRule(t, repo, []workflow.Action{{Type: workflow.ActionWebhook, CredentialID: "cred-1", Path: real}})
		back := readBack(t, repo.rows[id].Actions)
		enc := strings.ReplaceAll(back[0].Path, workflow.RedactedMarker, "%E2%80%A2%E2%80%A2%E2%80%A2%E2%80%A2")
		if enc == back[0].Path {
			t.Fatalf("fixture: no mask to encode in %q", back[0].Path)
		}
		if err := putActions(svc, id, []WorkflowActionInput{vaultHook("cred-1", enc)}); err != nil {
			t.Fatalf("re-save with the encoded mask = %v; want saved", err)
		}
		if got := pathsOf(t, repo.rows[id].Actions); got[0] != "cred-1 "+real {
			t.Fatalf("stored = %v; want the real path kept", got)
		}
	})

	t.Run("two stored paths masking alike ⇒ each position keeps its own", func(t *testing.T) {
		repo := newFakeWFRepo()
		svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog()).WithCredentialChecker(creds)
		id := seedLegacyRule(t, repo, []workflow.Action{
			{Type: workflow.ActionWebhook, CredentialID: "cred-1", Path: "/a?k=1"},
			{Type: workflow.ActionWebhook, CredentialID: "cred-1", Path: "/a?k=2"},
		})
		if err := putActions(svc, id, readBack(t, repo.rows[id].Actions)); err != nil {
			t.Fatalf("re-save = %v", err)
		}
		if got := pathsOf(t, repo.rows[id].Actions); got[0] != "cred-1 /a?k=1" || got[1] != "cred-1 /a?k=2" {
			t.Fatalf("stored = %v; want each slot's own path", got)
		}
	})

	for _, tc := range []struct {
		name   string
		stored []workflow.Action
		put    []WorkflowActionInput
	}{
		{"same mask, another key (the key fixes the host)",
			[]workflow.Action{{Type: workflow.ActionWebhook, CredentialID: "cred-1", Path: real}},
			[]WorkflowActionInput{vaultHook("cred-2", workflow.RedactTarget(real))}},
		{"a mask that is not the stored path's",
			[]workflow.Action{{Type: workflow.ActionWebhook, CredentialID: "cred-1", Path: real}},
			[]WorkflowActionInput{vaultHook("cred-1", "/legacy?src="+workflow.RedactedMarker)}},
		{"ambiguous: a new slot whose mask two different stored paths share",
			[]workflow.Action{
				{Type: workflow.ActionWebhook, CredentialID: "cred-1", Path: "/a?k=1"},
				{Type: workflow.ActionWebhook, CredentialID: "cred-1", Path: "/a?k=2"},
			},
			[]WorkflowActionInput{bareHook("https://erp.example.vn/x"), bareHook("https://erp.example.vn/y"),
				vaultHook("cred-1", "/a?k="+workflow.RedactedMarker)}},
	} {
		t.Run(tc.name+" ⇒ still refused", func(t *testing.T) {
			repo := newFakeWFRepo()
			svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog()).WithCredentialChecker(creds)
			id := seedLegacyRule(t, repo, tc.stored)
			before := append([]byte(nil), repo.rows[id].Actions...)
			wantBeforeCode(t, putActions(svc, id, tc.put), "WEBHOOK_REDACTED_VALUE")
			if !bytes.Equal(repo.rows[id].Actions, before) {
				t.Fatalf("a refused save changed the rule: %s", repo.rows[id].Actions)
			}
		})
	}

	t.Run("POST has no stored path to keep ⇒ refused", func(t *testing.T) {
		svc := NewWorkflowService(newFakeWFRepo()).WithEntityChecker(beforeCatalog()).WithCredentialChecker(creds)
		wantBeforeCode(t, postActions(svc, []WorkflowActionInput{vaultHook("cred-1", workflow.RedactTarget(real))}), "WEBHOOK_REDACTED_VALUE")
	})
}

// TestSetWorkflowActive_PausesKeyedRuleWithoutTouchingActions: the Doctor C10 refusal tells the
// admin to pause a rule that never idles. A PUT cannot do it (the old builder resends {type,url}
// and meets 409 WEBHOOK_LEGACY_SECRET_PENDING — a loop), so the switch is its own write: it flips
// the flag and leaves every stored byte of the actions, key and headers included.
func TestSetWorkflowActive_PausesKeyedRuleWithoutTouchingActions(t *testing.T) {
	repo := newFakeWFRepo()
	svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog())
	id := seedLegacyRule(t, repo, []workflow.Action{signedLegacy()})
	before := append([]byte(nil), repo.rows[id].Actions...)

	// The loop this closes: the same rule, switched off through PUT with the builder's body.
	_, err := svc.UpdateWorkflow(context.Background(), "t1", id, UpdateWorkflowInput{
		Name: "gọi đối tác", EntityName: "deals", Trigger: "after_create",
		Actions: []WorkflowActionInput{bareHook(legacyHook)}, IsActive: boolPtr(false),
	})
	wantBeforeCode(t, err, "WEBHOOK_LEGACY_SECRET_PENDING")

	rec, err := svc.SetWorkflowActive(context.Background(), "t1", id, false, "u1")
	if err != nil {
		t.Fatalf("pausing a keyed rule must work, got %v", err)
	}
	if rec.IsActive || repo.rows[id].IsActive {
		t.Fatalf("rule still active after pause: %+v", rec)
	}
	if !bytes.Equal(repo.rows[id].Actions, before) {
		t.Fatalf("pause must not touch the stored actions:\n got %s\nwant %s", repo.rows[id].Actions, before)
	}
	if _, err := svc.SetWorkflowActive(context.Background(), "t1", id, true, "u1"); err != nil || !repo.rows[id].IsActive {
		t.Fatalf("resume: err=%v active=%v", err, repo.rows[id].IsActive)
	}
}

func TestSetWorkflowActive_Errors(t *testing.T) {
	repo := newFakeWFRepo()
	svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog())
	id := seedLegacyRule(t, repo, []workflow.Action{signedLegacy()})
	cases := []struct {
		name, tenant, id, code string
	}{
		{"unknown id", "t1", "nope", "WORKFLOW_NOT_FOUND"},
		{"another tenant's rule", "t2", id, "WORKFLOW_NOT_FOUND"},
		{"blank id", "t1", " ", "WORKFLOW_ID_REQUIRED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.SetWorkflowActive(context.Background(), tc.tenant, tc.id, false, "u1")
			wantBeforeCode(t, err, tc.code)
		})
	}
	if !repo.rows[id].IsActive {
		t.Fatal("a refused switch must leave the rule as it was")
	}
}

// Switching a before rule back on goes through the same per-(entity, trigger) cap as a save does.
func TestSetWorkflowActive_ResumeHonoursBeforeRuleCap(t *testing.T) {
	repo := newFakeWFRepo()
	svc := NewWorkflowService(repo).WithEntityChecker(beforeCatalog())
	for i := 0; i < maxActiveBeforeRules; i++ {
		if _, err := repo.Create(context.Background(), workflow.CreateInput{
			TenantID: "t1", Name: "r", EntityName: "deals", Trigger: workflow.TriggerBeforeSave,
			Actions: []byte(`[]`), IsActive: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	paused, err := repo.Create(context.Background(), workflow.CreateInput{
		TenantID: "t1", Name: "paused", EntityName: "deals", Trigger: workflow.TriggerBeforeSave,
		Actions: []byte(`[]`), IsActive: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.SetWorkflowActive(context.Background(), "t1", paused, true, "u1")
	wantBeforeCode(t, err, "WORKFLOW_BEFORE_RULE_LIMIT")
	if repo.rows[paused].IsActive {
		t.Fatal("a refused resume must leave the rule paused")
	}
}
