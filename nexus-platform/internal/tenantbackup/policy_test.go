package tenantbackup

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// devTables is the snapshot of every non-obj_ table that carries tenant_id (plus the three children
// that hang off a parent) on the dev schema at migration 000089. Every one MUST have a rule: a
// table this list names and the policy does not is a table a customer backup would silently miss.
var devTables = []string{
	"acl_roles", "audit_log", "mfa_backup_codes", "mfa_pending_tokens", "password_reset_tokens",
	"refresh_tokens", "security_alerts", "sso_handoff_tokens", "sso_identities", "sso_providers",
	"sys_ai_messages", "sys_ai_runs", "sys_ai_sessions", "sys_api_keys", "sys_app_nav_items",
	"sys_approval_instances", "sys_approval_process_versions", "sys_approval_processes", "sys_apps", "sys_assignment_cursors",
	"sys_bpm_tasks", "sys_business_calendars", "sys_dashboards", "sys_email_log", "sys_entities",
	"sys_field_acl", "sys_fields", "sys_files", "sys_hook_deliveries", "sys_idempotency_keys",
	"sys_inbound_hooks", "sys_jobs", "sys_layouts", "sys_named_credentials", "sys_notifications",
	"sys_pack_installs", "sys_pack_items", "sys_preview_sessions", "sys_project_profiles", "sys_record_notes",
	"sys_record_shares", "sys_relationships", "sys_reports", "sys_sample_records", "sys_sequences",
	"sys_unique_index", "sys_views", "sys_work_queues", "sys_workflow_runs", "sys_workflows",
	"sys_workspace_sessions", "team_sets", "teams", "user_mfa", "users",
	// children without tenant_id
	"acl_role_actions", "team_memberships", "team_sets_teams",
}

func TestRuleFor_DevSchemaFullyClassified(t *testing.T) {
	for _, tbl := range devTables {
		if _, ok := RuleFor(tbl); !ok {
			t.Errorf("RuleFor(%q) = unclassified", tbl)
		}
	}
}

func TestRuleFor_Patterns(t *testing.T) {
	tests := []struct {
		table  string
		known  bool
		class  Class
		group  string
		reason string
	}{
		{"obj_crm_contacts", true, Restore, "data", "dynamic entity table by name pattern"},
		{"rel_a_b_c", true, Restore, "data", "join table with nested underscores"},
		{"users", true, KeepExport, "", "identity is exported, never restored"},
		{"sys_sequences", true, MergeMax, "data", "counter never goes back"},
		{"sys_idempotency_keys", true, Clear, "", "wiped"},
		{"refresh_tokens", true, Skip, "", "never in the file"},
		{"sys_mcp_tokens", true, Skip, "", "an agent key never travels with a backup"},
		{"totally_new_table", false, 0, "", "unknown table"},
		{"objx", false, 0, "", "pattern needs the underscore"},
		{"obj_", false, 0, "", "pattern needs a name"},
		{"obj_Bad-Name", false, 0, "", "pattern is lowercase [a-z0-9_]"},
	}
	for _, tc := range tests {
		t.Run(tc.table, func(t *testing.T) {
			r, ok := RuleFor(tc.table)
			if ok != tc.known {
				t.Fatalf("RuleFor(%q) known = %v, want %v (%s)", tc.table, ok, tc.known, tc.reason)
			}
			if !ok {
				return
			}
			if r.Class != tc.class || r.Group != tc.group {
				t.Errorf("RuleFor(%q) = {%v %q}, want {%v %q}", tc.table, r.Class, r.Group, tc.class, tc.group)
			}
		})
	}
}

func TestRuleFor_SecretsAndChildren(t *testing.T) {
	if _, ok := RuleFor("users"); !ok {
		t.Fatal("users unclassified")
	}
	u, _ := RuleFor("users")
	if _, ok := u.DropColumns["password_hash"]; !ok {
		t.Error("users.password_hash must be in DropColumns")
	}
	s, _ := RuleFor("sso_providers")
	if _, ok := s.DropColumns["client_secret_enc"]; !ok {
		t.Error("sso_providers.client_secret_enc must be in DropColumns")
	}
	owners := map[string]OwnerFK{
		"acl_role_actions": {Parent: "acl_roles", Column: "role_id"},
		"team_memberships": {Parent: "teams", Column: "team_id"},
		"team_sets_teams":  {Parent: "team_sets", Column: "team_set_id"},
	}
	for tbl, want := range owners {
		r, ok := RuleFor(tbl)
		if !ok || r.Owner == nil || *r.Owner != want {
			t.Errorf("RuleFor(%q).Owner = %+v, want %+v", tbl, r.Owner, want)
		}
	}
	tm, _ := RuleFor("team_memberships")
	if tm.SkipOrphan["user_id"] != "users" {
		t.Errorf("team_memberships.user_id must be SkipOrphan(users), got %v", tm.SkipOrphan)
	}
	wf, _ := RuleFor("sys_workflows")
	rk, ok := wf.RekeyJSON["actions"]
	if !ok || rk.ElemType != "outbound_webhook" || !reflect.DeepEqual(rk.Keys, []string{"secret", "headers"}) || rk.MatchKey != "url" {
		t.Errorf("sys_workflows.actions RekeyJSON = %+v", wf.RekeyJSON)
	}
	if !reflect.DeepEqual(wf.KeepColumns, []string{"last_run_at"}) {
		// the restore keeps the live last-run clock so a restored rule is not re-fired
		t.Errorf("sys_workflows.KeepColumns = %v", wf.KeepColumns)
	}
}

// realFKs is the FK set of the dev schema (queried from information_schema on migration 000084).
func realFKs() []FK {
	return []FK{
		{"acl_role_actions", "role_id", "acl_roles", "CASCADE"},
		{"acl_roles", "tenant_id", "tenants", "NO ACTION"},
		{"mfa_backup_codes", "tenant_id", "tenants", "NO ACTION"},
		{"mfa_backup_codes", "user_id", "users", "CASCADE"},
		{"refresh_tokens", "tenant_id", "tenants", "NO ACTION"},
		{"refresh_tokens", "user_id", "users", "CASCADE"},
		{"sso_identities", "provider_id", "sso_providers", "CASCADE"},
		{"sso_identities", "user_id", "users", "CASCADE"},
		{"sso_providers", "tenant_id", "tenants", "NO ACTION"},
		{"sys_field_acl", "role_id", "acl_roles", "CASCADE"},
		{"sys_fields", "entity_id", "sys_entities", "CASCADE"},
		{"team_memberships", "team_id", "teams", "CASCADE"},
		{"team_memberships", "user_id", "users", "CASCADE"},
		{"team_sets", "tenant_id", "tenants", "NO ACTION"},
		{"team_sets_teams", "team_id", "teams", "CASCADE"},
		{"team_sets_teams", "team_set_id", "team_sets", "CASCADE"},
		{"teams", "tenant_id", "tenants", "NO ACTION"},
		{"user_mfa", "user_id", "users", "CASCADE"},
		{"users", "role_id", "acl_roles", "SET NULL"},
		{"users", "tenant_id", "tenants", "NO ACTION"},
	}
}

func tenantTables() []string {
	var out []string
	child := map[string]bool{"acl_role_actions": true, "team_memberships": true, "team_sets_teams": true}
	for _, t := range devTables {
		if !child[t] {
			out = append(out, t)
		}
	}
	return append(out, "obj_crm_contacts", "rel_crm_a_crm_b")
}

func TestCheckRules_RealSchemaIsGreen(t *testing.T) {
	if err := CheckRules(tenantTables(), realFKs()); err != nil {
		t.Fatalf("real schema must pass: %v", err)
	}
}

func TestCheckRules_Violations(t *testing.T) {
	tests := []struct {
		name     string
		tables   []string
		fks      []FK
		wantCode string
		wantIn   string
	}{
		{
			name:     "unclassified table lists its name",
			tables:   append(tenantTables(), "sys_brand_new"),
			fks:      realFKs(),
			wantCode: "TENANT_BACKUP_SCHEMA_UNCLASSIFIED",
			wantIn:   "sys_brand_new",
		},
		{
			name:     "child without Owner rule",
			tables:   tenantTables(),
			fks:      append(realFKs(), FK{"mystery_children", "team_id", "teams", "CASCADE"}),
			wantCode: "TENANT_BACKUP_SCHEMA_UNCLASSIFIED",
			wantIn:   "mystery_children",
		},
		{
			name:     "keep table CASCADE-deleted by a restored parent",
			tables:   append(tenantTables(), "tbk_cascade"),
			fks:      append(realFKs(), FK{"tbk_cascade", "role_id", "acl_roles", "CASCADE"}),
			wantCode: "TENANT_RESTORE_SCHEMA_DRIFT",
			wantIn:   "tbk_cascade",
		},
		{
			name:     "users.role_id is SET NULL but a new SET NULL FK is not in the re-attach set",
			tables:   append(tenantTables(), "tbk_setnull"),
			fks:      append(realFKs(), FK{"tbk_setnull", "role_id", "acl_roles", "SET NULL"}),
			wantCode: "TENANT_RESTORE_SCHEMA_DRIFT",
			wantIn:   "tbk_setnull",
		},
		{
			name:     "restored table pointing at a non-restored one without SkipOrphan",
			tables:   tenantTables(),
			fks:      append(realFKs(), FK{"sys_views", "owner_id", "users", "CASCADE"}),
			wantCode: "TENANT_RESTORE_SCHEMA_DRIFT",
			wantIn:   "sys_views",
		},
	}
	// tbk_cascade / tbk_setnull are test-only names: the test registers them as KeepExport for the
	// duration (the integration test of the plan does the same with a real table).
	restore := registerTestRule("tbk_cascade", Rule{Class: KeepExport})
	defer restore()
	restore2 := registerTestRule("tbk_setnull", Rule{Class: KeepExport})
	defer restore2()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckRules(tc.tables, tc.fks)
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("want *Error, got %v", err)
			}
			if e.Code != tc.wantCode {
				t.Errorf("code = %s, want %s (%v)", e.Code, tc.wantCode, e)
			}
			if !strings.Contains(e.Msg+" "+toJSON(e.Details), tc.wantIn) {
				t.Errorf("error must name %q: %v %v", tc.wantIn, e.Msg, e.Details)
			}
		})
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// registerTestRule adds a table to the policy for one test and returns the undo.
func registerTestRule(name string, r Rule) func() {
	rules[name] = r
	return func() { delete(rules, name) }
}

func TestRedactSecrets(t *testing.T) {
	parse := func(s string) any {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	tests := []struct {
		name  string
		in    string
		want  string
		count int
	}{
		{"root object", `{"type":"outbound_webhook","url":"https://x","secret":"S","headers":{"Authorization":"B"},"extra":1}`,
			`{"type":"outbound_webhook","url":"https://x","extra":1}`, 1},
		{"in array", `[{"type":"outbound_webhook","secret":"S"},{"type":"set_field","secret":"keep"}]`,
			`[{"type":"outbound_webhook"},{"type":"set_field","secret":"keep"}]`, 1},
		{"nested under actions (pack content shape)", `{"actions":[{"type":"outbound_webhook","url":"u","headers":{"k":"v"}}],"name":"n"}`,
			`{"actions":[{"type":"outbound_webhook","url":"u"}],"name":"n"}`, 1},
		{"deeply nested + two hits", `{"a":{"b":[{"c":{"type":"outbound_webhook","secret":"1"}},{"type":"outbound_webhook","secret":"2"}]}}`,
			`{"a":{"b":[{"c":{"type":"outbound_webhook"}},{"type":"outbound_webhook"}]}}`, 2},
		{"other type untouched", `{"type":"send_email","secret":"x","headers":{"a":"b"}}`,
			`{"type":"send_email","secret":"x","headers":{"a":"b"}}`, 0},
		{"no type", `{"secret":"x"}`, `{"secret":"x"}`, 0},
		{"scalar", `"hello"`, `"hello"`, 0},
		{"outbound webhook without keys counts zero", `{"type":"outbound_webhook","url":"u"}`, `{"type":"outbound_webhook","url":"u"}`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, n := RedactSecrets(parse(tc.in))
			if n != tc.count {
				t.Errorf("stripped = %d, want %d", n, tc.count)
			}
			if !reflect.DeepEqual(got, parse(tc.want)) {
				gb, _ := json.Marshal(got)
				t.Errorf("got %s, want %s", gb, tc.want)
			}
		})
	}
}
