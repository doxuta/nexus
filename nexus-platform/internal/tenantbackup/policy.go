// Package tenantbackup backs up (and, in later slices, restores) ONE customer's data as a
// self-contained zip: JSONL per table + the real bytes of attached files + a manifest
// (specs/068-tenant-backup-restore).
//
// policy.go is the single place that decides, table by table, what a backup does with a table.
// It exists because the engine keeps ~50 system tables plus every runtime-built obj_*/rel_* table
// keyed by tenant_id, and "which of them travel with the customer" cannot live in a hand-kept list
// that goes stale the day somebody adds a migration. A table with tenant_id and no rule here makes
// CheckSchema refuse (422) and the integration test fail in the commit that adds the table.
package tenantbackup

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Class is what a backup/restore does with a table.
type Class int

const (
	// Restore: in the file; a restore deletes the customer's rows and writes the file's back.
	Restore Class = iota + 1
	// MergeMax: in the file; a restore only ever raises it (counters never go backwards).
	MergeMax
	// KeepExport: in the file (for the customer to read); a restore never touches it — identities,
	// audit trail, AI sessions: things whose "current" value must not be rewound.
	KeepExport
	// Skip: not in the file, never touched — tokens, MFA, API keys, vault, inbound-hook secrets.
	Skip
	// Clear: not in the file; a restore empties the customer's rows (idempotency cache).
	Clear
)

// String is the manifest spelling of the class.
func (c Class) String() string {
	switch c {
	case Restore:
		return "restore"
	case MergeMax:
		return "merge_max"
	case KeepExport:
		return "keep_export"
	case Skip:
		return "skip"
	case Clear:
		return "clear"
	}
	return "unknown"
}

// InFile reports whether a table of this class is written into the zip.
func (c Class) InFile() bool { return c == Restore || c == MergeMax || c == KeepExport }

// OwnerFK names the parent a table WITHOUT tenant_id hangs off: its rows are reached by joining
// the parent (`p.tenant_id = ?`).
type OwnerFK struct{ Parent, Column string }

// Rekey describes JSON elements whose credentials a restore re-attaches from the live row.
type Rekey struct {
	ElemType string   // value of the element's "type" key
	Keys     []string // keys that are never in the file and are re-attached on restore
	MatchKey string   // key that identifies the same element in the live row
}

// Rule is one table's policy.
type Rule struct {
	Class Class
	// Group is the restore-preview bucket (data | config | access | files); empty outside Restore
	// and MergeMax.
	Group string
	// Owner is set only for tables without tenant_id.
	Owner *OwnerFK
	// SkipOrphan: FK column → referenced table that a restore does NOT rewrite; a row whose
	// referent is gone is dropped (and counted), never kept dangling.
	SkipOrphan map[string]string
	// DropColumns are columns left out of the file, with the reason shown in the manifest.
	DropColumns map[string]string
	// KeepColumns keep the live value after a restore insert (sys_workflows.last_run_at).
	KeepColumns []string
	// RekeyJSON: JSON columns whose stripped credentials are re-attached from the live row.
	RekeyJSON map[string]Rekey
}

// rules is the whole policy. Reviewed table by table against the dev schema (migration 000084);
// keep it sorted by class so a reviewer can see what each class holds.
var rules = buildRules()

func buildRules() map[string]Rule {
	m := map[string]Rule{}
	add := func(class Class, group string, tables ...string) {
		for _, t := range tables {
			m[t] = Rule{Class: class, Group: group}
		}
	}
	// --- Restore -------------------------------------------------------------------------------
	add(Restore, "config",
		"sys_entities", "sys_fields", "sys_relationships", "sys_layouts", "sys_views", "sys_apps",
		"sys_app_nav_items", "sys_workflow_runs", "sys_approval_processes", "sys_approval_process_versions", "sys_approval_instances",
		"sys_bpm_tasks", "sys_work_queues", "sys_assignment_cursors", "sys_business_calendars",
		"sys_reports", "sys_dashboards", "sys_pack_installs", "sys_pack_items")
	add(Restore, "data",
		"sys_record_notes", "sys_notifications", "sys_unique_index", "sys_sample_records")
	add(Restore, "access", "acl_roles", "sys_field_acl", "sys_record_shares", "teams", "team_sets")
	add(Restore, "files", "sys_files")

	wf := m["sys_workflows"]
	wf.Class, wf.Group = Restore, "config"
	wf.KeepColumns = []string{"last_run_at"}
	wf.RekeyJSON = map[string]Rekey{
		"actions": {ElemType: "outbound_webhook", Keys: []string{"secret", "headers"}, MatchKey: "url"},
	}
	m["sys_workflows"] = wf

	// Tables of lanes not on main yet, declared in advance on the lanes' own proposal (specs/068
	// D-20; specs/063 T022b, 066 T022a/T097a, 062 L5, 069 T044): the publish history, the project
	// phase, the e-mail/usage logs and the pack-upgrade history must not rewind on a restore, and a
	// DRAFT is configuration like any other. A lane that lands one confirms it here in its own commit;
	// RuleFor for a name that has no table yet is harmless (CheckSchema reads the real schema).
	add(Restore, "config", "sys_meta_drafts")
	add(KeepExport, "", "sys_meta_versions", "sys_meta_version_docs", "sys_project_profiles",
		"sys_usage_daily", "sys_pack_upgrades")

	// Children without tenant_id — reached through their parent.
	m["acl_role_actions"] = Rule{Class: Restore, Group: "access", Owner: &OwnerFK{"acl_roles", "role_id"}}
	m["team_memberships"] = Rule{Class: Restore, Group: "access", Owner: &OwnerFK{"teams", "team_id"},
		SkipOrphan: map[string]string{"user_id": "users"}}
	m["team_sets_teams"] = Rule{Class: Restore, Group: "access", Owner: &OwnerFK{"team_sets", "team_set_id"}}

	// --- MergeMax ------------------------------------------------------------------------------
	add(MergeMax, "data", "sys_sequences")

	// --- KeepExport ----------------------------------------------------------------------------
	add(KeepExport, "",
		"sso_identities", "audit_log", "security_alerts", "sys_ai_sessions", "sys_ai_messages",
		"sys_ai_runs", "sys_hook_deliveries", "sys_email_log")
	m["users"] = Rule{Class: KeepExport, DropColumns: map[string]string{
		"password_hash": "mật khẩu băm không bao giờ vào tệp sao lưu",
	}}
	m["sso_providers"] = Rule{Class: KeepExport, DropColumns: map[string]string{
		"client_secret_enc": "khoá bí mật đăng nhập SSO không bao giờ vào tệp sao lưu",
	}}

	// --- Skip ----------------------------------------------------------------------------------
	add(Skip, "",
		"refresh_tokens", "password_reset_tokens", "mfa_pending_tokens", "sso_handoff_tokens", "user_mfa",
		"mfa_backup_codes", "sys_api_keys", "sys_named_credentials", "sys_inbound_hooks", "sys_jobs",
		"sys_tenant_backups", "sys_tenant_backup_schedules", "sys_workspace_sessions", "sys_preview_sessions")

	// --- Clear ---------------------------------------------------------------------------------
	add(Clear, "", "sys_idempotency_keys")
	return m
}

// dynamicTable matches the runtime-built entity (obj_) and join (rel_) tables.
var dynamicTable = regexp.MustCompile(`^(obj|rel)_[a-z0-9_]+$`)

// RuleFor returns the table's rule; obj_*/rel_* tables are Restore/data by name pattern. ok=false
// means the table is unclassified.
func RuleFor(table string) (Rule, bool) {
	if r, ok := rules[table]; ok {
		return r, true
	}
	if dynamicTable.MatchString(table) {
		return Rule{Class: Restore, Group: "data"}, true
	}
	return Rule{}, false
}

// IsDynamic reports whether the table is a runtime-built obj_/rel_ table (its JSON `data` column
// is the customer's business data and is never run through RedactSecrets).
func IsDynamic(table string) bool { return dynamicTable.MatchString(table) }

// FK is one foreign key read from information_schema.
type FK struct {
	Table      string `db:"table"`
	Column     string `db:"column"`
	RefTable   string `db:"ref_table"`
	DeleteRule string `db:"delete_rule"`
}

// reattachFKs are the FKs from a non-restored table into a restored one that a restore handles
// by "capture, delete, insert, re-attach" (users.role_id: deleting acl_roles SET NULLs the user's
// role; the restore puts it back when the role still exists).
var reattachFKs = map[string]bool{"users.role_id": true}

// classOf resolves the class of a table, a child inheriting its parent's.
func classOf(table string) (Class, bool) {
	r, ok := RuleFor(table)
	if !ok {
		return 0, false
	}
	return r.Class, true
}

// CheckRules is the pure half of CheckSchema: given every table that has a tenant_id column and the
// schema's foreign keys, it verifies the policy covers them (FR-014, FR-068). Returns *Error.
func CheckRules(tenantTables []string, fks []FK) error {
	hasTenant := make(map[string]bool, len(tenantTables))
	for _, t := range tenantTables {
		hasTenant[t] = true
	}

	// 1 + 2: every tenant table, and every tenant-less child of one, needs a rule.
	missing := map[string]bool{}
	for _, t := range tenantTables {
		if _, ok := RuleFor(t); !ok {
			missing[t] = true
		}
	}
	ownerSeen := map[string]bool{}
	for _, fk := range fks {
		if hasTenant[fk.Table] || !hasTenant[fk.RefTable] {
			continue
		}
		r, ok := RuleFor(fk.Table)
		if !ok || r.Owner == nil {
			missing[fk.Table] = true
			continue
		}
		if r.Owner.Parent == fk.RefTable && r.Owner.Column == fk.Column {
			ownerSeen[fk.Table] = true
		}
	}
	for tbl := range ownerSeen {
		delete(missing, tbl)
	}
	if len(missing) > 0 {
		names := sortedKeys(missing)
		return &Error{
			Code: "TENANT_BACKUP_SCHEMA_UNCLASSIFIED",
			Msg: "có bảng chưa được phân lớp sao lưu: " + strings.Join(names, ", ") +
				" — khai lớp trong internal/tenantbackup/policy.go",
			Details: map[string]any{"tables": names},
		}
	}

	// 4: FKs between classes (FKs into `tenants` are exempt: a restore never deletes a tenants row).
	var bad []string
	for _, fk := range fks {
		if fk.RefTable == "tenants" || fk.Table == fk.RefTable {
			continue
		}
		tc, tok := classOf(fk.Table)
		rc, rok := classOf(fk.RefTable)
		if !tok || !rok {
			continue
		}
		switch {
		case tc != Restore && rc == Restore:
			if fk.DeleteRule != "SET NULL" || !reattachFKs[fk.Table+"."+fk.Column] {
				bad = append(bad, fmt.Sprintf("%s.%s -> %s (%s)", fk.Table, fk.Column, fk.RefTable, fk.DeleteRule))
			}
		case tc == Restore && rc != Restore:
			if r, _ := RuleFor(fk.Table); r.SkipOrphan[fk.Column] != fk.RefTable {
				bad = append(bad, fmt.Sprintf("%s.%s -> %s (%s)", fk.Table, fk.Column, fk.RefTable, fk.DeleteRule))
			}
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return &Error{
			Code: "TENANT_RESTORE_SCHEMA_DRIFT",
			Msg: "khoá ngoại giữa các lớp sao lưu sai luật: " + strings.Join(bad, ", ") +
				" — khôi phục sẽ xoá lan hoặc để lại dòng mồ côi",
			Details: map[string]any{"constraints": bad},
		}
	}
	return nil
}

// LoadSchema reads, from information_schema of the CURRENT schema, the tables that carry a
// tenant_id column and every foreign key (same source and query shape as the tenant purge's
// tenantScopedTables/purgeSchemaDrift — one definition of "a table that holds customer data").
func LoadSchema(ctx context.Context, q Querier) ([]string, []FK, error) {
	var tables []string
	if err := q.SelectContext(ctx, &tables, `
		SELECT DISTINCT c.TABLE_NAME FROM information_schema.COLUMNS c
		JOIN information_schema.TABLES t ON t.TABLE_SCHEMA = c.TABLE_SCHEMA AND t.TABLE_NAME = c.TABLE_NAME
		WHERE c.TABLE_SCHEMA = DATABASE() AND c.COLUMN_NAME = 'tenant_id' AND t.TABLE_TYPE = 'BASE TABLE'
		ORDER BY c.TABLE_NAME`); err != nil {
		return nil, nil, fmt.Errorf("tenantbackup.LoadSchema: tables: %w", err)
	}
	if len(tables) == 0 {
		// Zero is never right on a migrated schema; reading it as "nothing to back up" would
		// produce an empty file that looks like a success.
		return nil, nil, &Error{Code: "TENANT_BACKUP_SCHEMA_UNCLASSIFIED",
			Msg: "không đọc được danh sách bảng theo khách — dừng để tránh bản sao lưu rỗng"}
	}
	var fks []FK
	if err := q.SelectContext(ctx, &fks, `
		SELECT k.TABLE_NAME AS `+"`table`"+`, k.COLUMN_NAME AS `+"`column`"+`,
		       k.REFERENCED_TABLE_NAME AS ref_table, rc.DELETE_RULE AS delete_rule
		FROM information_schema.KEY_COLUMN_USAGE k
		JOIN information_schema.REFERENTIAL_CONSTRAINTS rc
		  ON rc.CONSTRAINT_SCHEMA = k.TABLE_SCHEMA AND rc.CONSTRAINT_NAME = k.CONSTRAINT_NAME
		     AND rc.TABLE_NAME = k.TABLE_NAME
		WHERE k.TABLE_SCHEMA = DATABASE() AND k.REFERENCED_TABLE_NAME IS NOT NULL
		ORDER BY k.TABLE_NAME, k.COLUMN_NAME`); err != nil {
		return nil, nil, fmt.Errorf("tenantbackup.LoadSchema: foreign keys: %w", err)
	}
	return tables, fks, nil
}

// CheckSchema is called at the head of every backup (and, later, every restore preview): it
// refuses (422 TENANT_BACKUP_SCHEMA_UNCLASSIFIED / TENANT_RESTORE_SCHEMA_DRIFT) when the live
// schema has a table or foreign key this policy has not classified.
func CheckSchema(ctx context.Context, q Querier) error {
	tables, fks, err := LoadSchema(ctx, q)
	if err != nil {
		return err
	}
	return CheckRules(tables, fks)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RedactSecrets walks a decoded JSON value and removes the credentials of every outbound-webhook
// element, at any depth: any object whose "type" is "outbound_webhook" loses its "secret" and
// "headers" keys (the same set vertical_pack.stripWorkflowCredentials drops). It is deliberately
// NOT keyed on a column list: a bank/partner Bearer token sitting in `sys_pack_items.content` of an
// install older than commit 0fb48a8e is exactly as live as one in `sys_workflows.actions`, and a
// future table that snapshots rules (specs/063 draft-publish) is covered without anyone remembering
// to declare it. Unknown keys and every other object survive untouched. Returns the number of
// objects that had a credential removed. The value is mutated in place (the caller owns it).
func RedactSecrets(v any) (any, int) {
	n := 0
	var walk func(x any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			if typ, _ := t["type"].(string); strings.TrimSpace(typ) == "outbound_webhook" {
				_, hasSecret := t["secret"]
				_, hasHeaders := t["headers"]
				if hasSecret || hasHeaders {
					delete(t, "secret")
					delete(t, "headers")
					n++
				}
			}
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(v)
	return v, n
}

// Error is the package's typed refusal. The handler maps Code to an HTTP status through the
// shared statusForCode; this package never imports services or net/http.
type Error struct {
	Code    string
	Msg     string
	Details map[string]any
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }
