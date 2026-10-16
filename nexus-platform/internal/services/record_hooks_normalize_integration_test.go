//go:build integration

// specs/048 S5 (T093, T094) — before_save normalize + fill on real MySQL, through the real
// WorkflowService + CachedLoader + dynrepo + ImportService + RunSink (hkSetup). Proves the rewritten
// text is what is stored on every create path — the form (Create), the all-or-nothing batch and the
// CSV import — that the duplicate checks and the byte-exact unique index (utf8mb4_bin) see the
// rewritten value, that a refused batch burns no auto_number, and that a legacy record is normalized
// on its next save, in that save's one UPDATE and one audit row.
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true" \
//	  go test -tags=integration ./internal/services/ -run 'Normaliz|TestCreateBulk_Atomic_BeforeSaveMutations_Integration' -race -count=1 -v
package services

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/zap"

	"nexus/platform/internal/workflow"
)

// createNormLeads registers hk_nm_leads: a required name, a UNIQUE text email (the CRM pack's
// crm_contacts shape), a source picklist and an LD- sequence.
func (e hkEnv) createNormLeads(t *testing.T, tenant string) {
	t.Helper()
	if _, _, err := e.ent.CreateEntity(context.Background(), tenant, CreateEntityInput{
		Name: "hk_nm_leads", Label: "Leads",
		Fields: []FieldInput{
			{Name: "full_name", Label: "Name", FieldType: "text", IsRequired: true},
			{Name: "email", Label: "Email", FieldType: "text", IsUnique: true},
			{Name: "source", Label: "Source", FieldType: "picklist", Options: map[string]any{"values": []any{"Web", "Event"}}},
			{Name: "lead_code", Label: "Code", FieldType: "auto_number", Options: map[string]any{"prefix": "LD-", "padding": float64(4)}},
		},
	}); err != nil {
		t.Fatalf("create hk_nm_leads in %s: %v", tenant, err)
	}
}

func (e hkEnv) normRule(t *testing.T, tenant, name string, actions ...WorkflowActionInput) string {
	t.Helper()
	on := true
	wf, err := e.wf.CreateWorkflow(context.Background(), tenant, CreateWorkflowInput{
		Name: name, EntityName: "hk_nm_leads", Trigger: string(workflow.TriggerBeforeSave), Actions: actions, IsActive: &on,
	})
	if err != nil {
		t.Fatalf("create rule %q: %v", name, err)
	}
	return wf.ID
}

// us7Rules switches on the US7 rules (email trim+lower, name trim+collapse) and "no source ⇒ Web".
func (e hkEnv) us7Rules(t *testing.T, tenant string) (email, name, source string) {
	t.Helper()
	email = e.normRule(t, tenant, "Chuẩn hoá email", WorkflowActionInput{Type: workflow.ActionNormalize, Field: "email", Ops: []string{"trim", "lower"}})
	name = e.normRule(t, tenant, "Chuẩn hoá họ tên", WorkflowActionInput{Type: workflow.ActionNormalize, Field: "full_name", Ops: []string{"trim", "collapse_spaces"}})
	source = e.normRule(t, tenant, "Không nguồn là Website", WorkflowActionInput{Type: workflow.ActionFieldUpdate, Field: "source", Value: "Web", OnlyIfEmpty: true})
	return email, name, source
}

// normLeads returns every live hk_nm_leads row of tenant as "full_name|email|source", sorted, and
// the numbers of their LD- codes, sorted.
func (e hkEnv) normLeads(t *testing.T, tenant string) (rows []string, codes []int) {
	t.Helper()
	got := []struct{ Name, Email, Source, Code *string }{}
	if err := e.sdb.Select(&got, `SELECT JSON_UNQUOTE(JSON_EXTRACT(data, '$.full_name')) AS name,
		JSON_UNQUOTE(JSON_EXTRACT(data, '$.email')) AS email, JSON_UNQUOTE(JSON_EXTRACT(data, '$.source')) AS source,
		JSON_UNQUOTE(JSON_EXTRACT(data, '$.lead_code')) AS code FROM obj_hk_nm_leads WHERE tenant_id = ? AND deleted_at IS NULL`, tenant); err != nil {
		t.Fatalf("read hk_nm_leads: %v", err)
	}
	s := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	for _, r := range got {
		rows = append(rows, s(r.Name)+"|"+s(r.Email)+"|"+s(r.Source))
		n, err := strconv.Atoi(strings.TrimPrefix(s(r.Code), "LD-"))
		if err != nil {
			t.Fatalf("lead_code %q of %s: %v", s(r.Code), s(r.Name), err)
		}
		codes = append(codes, n)
	}
	sort.Strings(rows)
	sort.Ints(codes)
	return rows, codes
}

func (e hkEnv) importLeadsCSV(tenant, csv, dedup string, scope RecordScope) (*ImportResult, error) {
	return NewImportService(e.meta, e.dyn, zap.NewNop()).WithSequenceClaimer(e.dyn).WithRelationChecker(e.dyn).WithBeforeHooks(e.cache, e.runs).
		Import(context.Background(), ImportInput{
			TenantID: tenant, UserID: scope.UserID, RoleID: scope.RoleID, IsAdmin: scope.IsAdmin, EntityName: "hk_nm_leads",
			Mapping: map[string]string{"full_name": "full_name", "email": "email"}, DedupField: dedup,
			Reader: strings.NewReader(csv), FileSize: int64(len(csv)),
		})
}

// US7 scenario 1 + the legacy record: the form create stores the rewritten text; a record stored
// before the rules existed is normalized by its next save — the same UPDATE (version + 1) and the
// same audit row as the user's own change.
func TestRecordCreate_BeforeSaveNormalizes_Integration(t *testing.T) {
	env := hkSetup(t)
	ctx := context.Background()
	env.createNormLeads(t, engTenantA)
	rep := RecordScope{UserID: hkUser, AccessLevel: "all", RoleID: "role-rep"}

	legacy, err := env.rec.Create(ctx, rep, engTenantA, hkUser, "hk_nm_leads", map[string]any{"full_name": "Cũ", "email": " Old@EXAMPLE.COM"})
	if err != nil {
		t.Fatalf("legacy create: %v", err)
	}
	env.us7Rules(t, engTenantA)

	id, err := env.rec.Create(ctx, rep, engTenantA, hkUser, "hk_nm_leads",
		map[string]any{"full_name": "  Nguyễn   Văn  An ", "email": "  An.Nguyen@EXAMPLE.COM "})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := env.storedIn(t, engTenantA, "hk_nm_leads", id).Data; got["full_name"] != "Nguyễn Văn An" || got["email"] != "an.nguyen@example.com" || got["source"] != "Web" {
		t.Fatalf("stored = %v, want the US7 lead normalized and filled", got)
	}

	before := env.storedIn(t, engTenantA, "hk_nm_leads", legacy)
	audits := len(env.updateAuditDiffs())
	if err := env.rec.Update(ctx, rep, engTenantA, "hk_nm_leads", legacy, map[string]any{"source": "Event"}, nil); err != nil {
		t.Fatalf("update legacy: %v", err)
	}
	after := env.storedIn(t, engTenantA, "hk_nm_leads", legacy)
	if after.Data["email"] != "old@example.com" || after.Data["source"] != "Event" || after.Version != before.Version+1 {
		t.Fatalf("legacy after = %v v%d (was v%d), want email normalized in the same UPDATE", after.Data, after.Version, before.Version)
	}
	diffs := env.updateAuditDiffs()
	if len(diffs) != audits+1 || diffs[len(diffs)-1].After["email"] != "old@example.com" || diffs[len(diffs)-1].After["source"] != "Event" {
		t.Fatalf("audit = %d new rows, last %+v; want one row with source + email", len(diffs)-audits, diffs[len(diffs)-1])
	}
}

// T093: an all-or-nothing batch stores the rewritten rows; rows the rules make equal on the unique
// email refuse the whole batch before any sequence claim; one summary run per rule for the batch it
// wrote, no value in it.
func TestCreateBulk_Atomic_BeforeSaveMutations_Integration(t *testing.T) {
	env := hkSetup(t)
	ctx := context.Background()
	env.createNormLeads(t, engTenantA)
	emailRule, nameRule, sourceRule := env.us7Rules(t, engTenantA)
	rep := RecordScope{UserID: hkUser, AccessLevel: "all", RoleID: "role-rep"}

	res, err := env.rec.CreateBulk(ctx, rep, engTenantA, hkUser, "hk_nm_leads", []map[string]any{
		{"full_name": " An  Nguyễn", "email": " AN@EXAMPLE.COM "},
		{"full_name": "Bình", "email": "binh@example.com", "source": "Event"},
		{"full_name": "Chi", "email": "Chi@example.com"},
	}, BulkModeAtomic)
	if err != nil || res.Created != 3 {
		t.Fatalf("atomic bulk = %+v, %v; want 3 created", res, err)
	}
	collide, err := env.rec.CreateBulk(ctx, rep, engTenantA, hkUser, "hk_nm_leads", []map[string]any{
		{"full_name": "Dũng", "email": "Dung@example.com"}, {"full_name": "Dung 2", "email": " dung@example.com"},
	}, BulkModeAtomic)
	if err != nil || collide.Created != 0 || collide.Results[1].Code != "RECORD_DUPLICATE" {
		t.Fatalf("colliding batch = %+v, %v; want row 1 RECORD_DUPLICATE, nothing created", collide, err)
	}
	if _, err := env.rec.Create(ctx, rep, engTenantA, hkUser, "hk_nm_leads", map[string]any{"full_name": "Em", "email": "em@example.com"}); err != nil {
		t.Fatalf("create after the refused batch: %v", err)
	}

	want := []string{"An Nguyễn|an@example.com|Web", "Bình|binh@example.com|Event", "Chi|chi@example.com|Web", "Em|em@example.com|Web"}
	rows, codes := env.normLeads(t, engTenantA)
	if strings.Join(rows, " ; ") != strings.Join(want, " ; ") {
		t.Fatalf("stored = %v\nwant     %v", rows, want)
	}
	if len(codes) != 4 || codes[3] != codes[0]+3 {
		t.Fatalf("LD- numbers = %v, want four in a row (the refused batch burned none)", codes)
	}

	// One summary run per rule for the stored batch (+ the single create's own runs), never a value.
	wantRuns := map[string]int{emailRule: 2, nameRule: 1, sourceRule: 2}
	for wf, applied := range wantRuns {
		var summaries []any
		for _, r := range env.drainRuns(t, engTenantA, wf) {
			if raw := string(*r.ActionsResult); strings.Contains(strings.ToLower(raw), "northwind") || strings.Contains(raw, "Nguyễn") {
				t.Fatalf("run carries a value: %s", raw)
			}
			if r.RecordID == nil {
				if r.Status != "success" || r.TriggerType != "before_save" {
					t.Fatalf("summary run = %+v, want a before_save success", r)
				}
				summaries = append(summaries, actionsOf(t, r))
			}
		}
		if want := []any{map[string]any{"applied": applied, "blocked": 0, "failed": 0}}; !jsonEqual(summaries, want) {
			t.Fatalf("summary runs of %s = %v, want %v", wf, summaries, want)
		}
	}
}

// T094: a CSV of leads is stored normalized, and every duplicate check reads the normalized email —
// the file's own rows and what is already stored (dedupField), or the byte-exact unique index that
// refuses the whole file without it. Without the rules the same two emails are two leads.
func TestImport_NormalizeBeforeUniqueCheck_Integration(t *testing.T) {
	env := hkSetup(t)
	env.createNormLeads(t, engTenantA)
	env.createNormLeads(t, engTenantB)
	rep := RecordScope{UserID: hkUser, AccessLevel: "all", RoleID: "role-rep"}
	pair := "full_name,email\nAn,An@EXAMPLE.COM\nAn 2, an@example.com \n"

	// Tenant B has no rule: utf8mb4_bin tells the two emails apart, both are stored.
	if res, err := env.importLeadsCSV(engTenantB, pair, "", rep); err != nil || res.Inserted != 2 {
		t.Fatalf("tenant B import = %+v, %v; want both rows stored", res, err)
	}

	env.us7Rules(t, engTenantA)
	res, err := env.importLeadsCSV(engTenantA, "full_name,email\n  Nguyễn   Văn  An ,  An.Nguyen@EXAMPLE.COM \nBình,Binh@example.com\nChi,chi@example.com\n", "", rep)
	if err != nil || res.Inserted != 3 {
		t.Fatalf("import = %+v, %v; want 3 inserted", res, err)
	}
	if got, _ := env.normLeads(t, engTenantA); strings.Join(got, " ; ") != "Bình|binh@example.com|Web ; Chi|chi@example.com|Web ; Nguyễn Văn An|an.nguyen@example.com|Web" {
		t.Fatalf("stored = %v, want every lead normalized and filled", got)
	}

	res, err = env.importLeadsCSV(engTenantA, pair, "email", rep)
	if err != nil || res.Inserted != 1 || len(res.Errors) != 1 || res.Errors[0].Row != 2 || res.Errors[0].Code != "RECORD_DUPLICATE" {
		t.Fatalf("dedup import = %+v, %v; want row 2 RECORD_DUPLICATE (same email once normalized)", res, err)
	}
	res, err = env.importLeadsCSV(engTenantA, "full_name,email\nChi 2, CHI@EXAMPLE.COM\n", "email", rep)
	if err != nil || res.Inserted != 0 || len(res.Errors) != 1 || res.Errors[0].Code != "RECORD_DUPLICATE" {
		t.Fatalf("already-stored import = %+v, %v; want the row reported as already stored", res, err)
	}
	// queue #358: no dedupField — the unique field is probed anyway, so the second row is named by its
	// line (it reads the normalized value) instead of the unique index refusing the whole file.
	res, err = env.importLeadsCSV(engTenantA, "full_name,email\nDũng,Dung@example.com\nDung 2, DUNG@example.com\n", "", rep)
	if err != nil || res.Inserted != 1 || len(res.Errors) != 1 || res.Errors[0].Row != 2 || res.Errors[0].Code != "RECORD_DUPLICATE" {
		t.Fatalf("no-dedup import = %+v, %v; want row 2 RECORD_DUPLICATE (same email once normalized)", res, err)
	}
	rows, _ := env.normLeads(t, engTenantA)
	stored := strings.Join(rows, " ; ")
	if !strings.Contains(stored, "Dũng|dung@example.com|") || strings.Contains(stored, "Dung 2|") {
		t.Fatalf("stored = %v; want Dũng stored and Dung 2 refused", rows)
	}
}
