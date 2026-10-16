package tenantbackup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ---- fake PlanStore ---------------------------------------------------------------------------

type fakePlan struct {
	running bool
	refs    []TableRef
	fks     []FK
	cols    map[string][]Column // table -> columns; absent = the table does not exist
	counts  map[string]int64
	access  *AccessSnapshot
	labels  map[string]string
	files   map[string]int64
	jobs    int64
	asked   []string // every call made, to prove the preview only reads
}

func (f *fakePlan) OperationRunning(context.Context, string) (bool, error) { return f.running, nil }
func (f *fakePlan) ExportTables(context.Context) ([]TableRef, []FK, error) {
	return f.refs, f.fks, nil
}
func (f *fakePlan) TableColumns(_ context.Context, names []string) (map[string][]Column, error) {
	out := map[string][]Column{}
	for _, n := range names {
		if c, ok := f.cols[n]; ok {
			out[n] = c
		}
	}
	return out, nil
}
func (f *fakePlan) CountRows(_ context.Context, ref TableRef, _ string) (int64, error) {
	return f.counts[ref.Table], nil
}
func (f *fakePlan) AccessNow(context.Context, string) (*AccessSnapshot, error) { return f.access, nil }
func (f *fakePlan) EntityLabels(context.Context, string) (map[string]string, error) {
	return f.labels, nil
}
func (f *fakePlan) LiveFiles(context.Context, string) (map[string]int64, error) { return f.files, nil }
func (f *fakePlan) PendingJobs(context.Context, string) (int64, error)          { return f.jobs, nil }

// ---- fixture ----------------------------------------------------------------------------------

var idCol = Column{Name: "id", DataType: "char"}

type zipSpec struct {
	tenantID string
	version  int64
	lines    map[string][]string // table -> JSONL lines (without newline)
	class    map[string]string   // table -> manifest class override
	cols     map[string][]Column // table -> manifest columns override
	missing  []string
	blobs    map[string]string
}

func buildPlanZip(t *testing.T, sp zipSpec) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf)
	m := Manifest{FormatVersion: FormatVersion, Tenant: ManifestTenant{ID: sp.tenantID, Slug: "qa-x", Name: "QA X"},
		Kind: "manual", SchemaVersion: sp.version, Files: ManifestFiles{Missing: sp.missing}}
	for name, lines := range sp.lines {
		tw, err := w.Table(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range lines {
			if _, err := tw.Write([]byte(l + "\n")); err != nil {
				t.Fatal(err)
			}
		}
		r, _ := RuleFor(name)
		cls := r.Class.String()
		if c, ok := sp.class[name]; ok {
			cls = c
		}
		cols := []Column{idCol}
		if c, ok := sp.cols[name]; ok {
			cols = c
		}
		m.Tables = append(m.Tables, ManifestTable{Name: name, Class: cls, Rows: int64(len(lines)), Columns: cols})
	}
	for id, content := range sp.blobs {
		if _, _, err := w.Blob(id, strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Finish("x", m); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const (
	bkID     = "11111111-0000-4000-8000-000000000001"
	fileA    = "f0000000-0000-4000-8000-00000000000a"
	fileB    = "f0000000-0000-4000-8000-00000000000b"
	roleKept = "r0000000-0000-4000-8000-000000000001"
	roleNew  = "r0000000-0000-4000-8000-000000000002"
)

type planFx struct {
	*fixture
	plan *fakePlan
}

func newPlanFx(t *testing.T, sp zipSpec) *planFx {
	t.Helper()
	f := newFixture(t)
	if sp.tenantID == "" {
		sp.tenantID = tenantA
	}
	if sp.version == 0 {
		sp.version = 84
	}
	body := buildPlanZip(t, sp)
	sum := sha256.Sum256(body)
	hexSum := hex.EncodeToString(sum[:])
	key := tenantA + "/" + bkID + ".zip"
	f.backups.objs[key] = body
	size := int64(len(body))
	f.store.rows[bkID] = &Backup{ID: bkID, TenantID: tenantA, Kind: KindManual, Status: StatusReady, StorageKey: &key,
		SizeBytes: &size, SHA256: &hexSum, SchemaVersion: sp.version, FormatVersion: 1, CreatedAt: time.Now()}
	f.store.version = 84
	p := &fakePlan{
		cols:   map[string][]Column{"acl_roles": {idCol}, "acl_role_actions": {idCol}, "team_memberships": {idCol}, "sys_files": {idCol}, "sys_workflows": {idCol}, "sys_entities": {idCol}},
		counts: map[string]int64{}, access: NewAccessSnapshot(), labels: map[string]string{}, files: map[string]int64{},
	}
	for _, n := range []string{"acl_roles", "acl_role_actions", "team_memberships", "sys_files", "sys_workflows", "sys_entities"} {
		r, _ := RuleFor(n)
		p.refs = append(p.refs, TableRef{Table: n, Rule: r})
	}
	f.svc.WithPlan(p, 1000)
	return &planFx{fixture: f, plan: p}
}

func (p *planFx) preview() (*RestorePlan, error) {
	return p.svc.Preview(context.Background(), PreviewInput{TenantID: tenantA, BackupID: bkID, CallerTenantID: opTen})
}

func rowsOf(lines ...string) []string { return lines }

// ---- Validate order (FR-033) ------------------------------------------------------------------

func TestPreview_RefusalOrder(t *testing.T) {
	good := zipSpec{lines: map[string][]string{"acl_roles": rowsOf(`{"id":"` + roleKept + `"}`)}}
	tests := []struct {
		name  string
		spec  zipSpec
		setup func(p *planFx)
		in    *PreviewInput
		code  string
	}{
		{"unknown customer", good, nil, &PreviewInput{TenantID: "nope", BackupID: bkID, CallerTenantID: opTen}, "TENANT_NOT_FOUND"},
		{"retired customer", good, func(p *planFx) { p.store.tenants[tenantA].Retired = true }, nil, "TENANT_RESTORE_RETIRED"},
		{"the workshop the caller stands in", good, nil, &PreviewInput{TenantID: tenantA, BackupID: bkID, CallerTenantID: tenantA}, "TENANT_RESTORE_SELF"},
		{"self wins over a missing backup", good, nil, &PreviewInput{TenantID: tenantA, BackupID: "nope", CallerTenantID: tenantA}, "TENANT_RESTORE_SELF"},
		{"another customer's backup id", good, func(p *planFx) { p.store.rows[bkID].TenantID = tenantB }, nil, "BACKUP_NOT_FOUND"},
		{"backup not ready", good, func(p *planFx) { p.store.rows[bkID].Status = StatusRunning }, nil, "BACKUP_NOT_READY"},
		{"another operation running", good, func(p *planFx) { p.plan.running = true }, nil, "BACKUP_IN_PROGRESS"},
		{"file gone", good, func(p *planFx) { delete(p.backups.objs, *p.store.rows[bkID].StorageKey) }, nil, "BACKUP_FILE_MISSING"},
		{"one byte flipped", good, func(p *planFx) {
			b := p.backups.objs[*p.store.rows[bkID].StorageKey]
			b[len(b)/2] ^= 0xff
		}, nil, "TENANT_BACKUP_CORRUPT"},
		{"manifest names another customer", zipSpec{tenantID: tenantB, lines: good.lines}, nil, nil, "TENANT_RESTORE_TENANT_MISMATCH"},
		{"schema older than the code", zipSpec{version: 83, lines: good.lines}, nil, nil, "TENANT_RESTORE_SCHEMA_MISMATCH"},
		{"table class changed by a deploy", zipSpec{lines: good.lines, class: map[string]string{"acl_roles": "keep_export"}}, nil, nil, "TENANT_RESTORE_SCHEMA_DRIFT"},
		{"column the table no longer has", zipSpec{lines: good.lines, cols: map[string][]Column{"acl_roles": {idCol, {Name: "gone", DataType: "char"}}}}, nil, nil, "TENANT_RESTORE_SCHEMA_DRIFT"},
		{"a limit of 0 means the default, not zero rows", good, func(p *planFx) { p.svc.WithPlan(p.plan, 0) }, nil, ""},
		{"too many rows live", good, func(p *planFx) { p.plan.counts["acl_roles"] = 5000 }, nil, "TENANT_RESTORE_TOO_LARGE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlanFx(t, tc.spec)
			if tc.setup != nil {
				tc.setup(p)
			}
			in := PreviewInput{TenantID: tenantA, BackupID: bkID, CallerTenantID: opTen}
			if tc.in != nil {
				in = *tc.in
			}
			_, err := p.svc.Preview(context.Background(), in)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				return
			}
			asErr(t, err, tc.code)
		})
	}
}

func TestPreview_SchemaMismatchDetails(t *testing.T) {
	p := newPlanFx(t, zipSpec{version: 83, lines: map[string][]string{"acl_roles": rowsOf(`{"id":"` + roleKept + `"}`)}})
	_, err := p.preview()
	e := asErr(t, err, "TENANT_RESTORE_SCHEMA_MISMATCH")
	if e.Details["backup"] != int64(83) || e.Details["current"] != int64(84) {
		t.Errorf("details = %v", e.Details)
	}
}

func TestPreview_DriftNamesTheClassPair(t *testing.T) {
	p := newPlanFx(t, zipSpec{lines: map[string][]string{"acl_roles": rowsOf(`{"id":"` + roleKept + `"}`)}, class: map[string]string{"acl_roles": "keep_export"}})
	_, err := p.preview()
	e := asErr(t, err, "TENANT_RESTORE_SCHEMA_DRIFT")
	if e.Details["table"] != "acl_roles" || e.Details["backupClass"] != "keep_export" || e.Details["currentClass"] != "restore" {
		t.Errorf("details = %v", e.Details)
	}
}

func TestPreview_TooLargeDetails(t *testing.T) {
	p := newPlanFx(t, zipSpec{lines: map[string][]string{"acl_roles": rowsOf(`{"id":"a"}`, `{"id":"b"}`, `{"id":"c"}`)}})
	p.svc.WithPlan(p.plan, 2)
	_, err := p.preview()
	e := asErr(t, err, "TENANT_RESTORE_TOO_LARGE")
	if e.Details["rows"] != int64(3) || e.Details["limit"] != 2 {
		t.Errorf("details = %v", e.Details)
	}
}

// ---- Plan content -----------------------------------------------------------------------------

func TestPreview_Plan(t *testing.T) {
	p := newPlanFx(t, zipSpec{
		lines: map[string][]string{
			"acl_roles":        rowsOf(`{"id":"` + roleKept + `"}`),
			"acl_role_actions": rowsOf(`{"id":"x1","role_id":"` + roleKept + `","entity_name":"accounts","action":"delete","access_level":"all"}`),
			"team_memberships": rowsOf(`{"id":"m1","team_id":"t1","user_id":"u-in"}`, `{"id":"m2","team_id":"t1","user_id":"u-gone"}`),
			"sys_files": rowsOf(`{"id":"`+fileA+`","size":"3","deleted_at":null}`,
				`{"id":"`+fileB+`","size":"5","deleted_at":null}`),
			"sys_entities": rowsOf(`{"id":"e1","name":"crm_x","table_name":"obj_crm_x","label":"Nhãn trong bản"}`),
			"sys_workflows": rowsOf(`{"id":"w1","is_active":"1","deleted_at":null,` +
				`"actions":[{"type":"outbound_webhook","url":"https://partner.example/h?token=SECRET123"}]}`),
		},
		missing: []string{"f-missing"},
		blobs:   map[string]string{fileA: "abc"}, // fileB has no bytes in the file
	})
	pl := p.plan
	pl.counts = map[string]int64{"acl_roles": 2, "acl_role_actions": 4, "team_memberships": 3, "sys_files": 1}
	pl.fks = []FK{{Table: "acl_role_actions", Column: "role_id", RefTable: "acl_roles", DeleteRule: "CASCADE"}}
	pl.labels = map[string]string{"obj_crm_x": "Khách hàng"}
	pl.refs = append(pl.refs, TableRef{Table: "obj_crm_x", Rule: Rule{Class: Restore, Group: "data"}})
	pl.cols["obj_crm_x"] = []Column{idCol}
	pl.counts["obj_crm_x"] = 7
	pl.jobs = 3
	pl.files = map[string]int64{fileA: 3, "f-live-only": 10}
	// live: u1 holds a role the backup does not have; u2 holds the kept role; 'u-in' is a live user.
	pl.access.AddRole(roleKept)
	pl.access.AddRole(roleNew)
	pl.access.AddUser("u1", roleNew, "active", false)
	pl.access.AddUser("u2", roleKept, "active", false)
	pl.access.AddUser("u-in", "", "active", false)
	pl.access.AddAPIKey(roleKept)
	p.svc.freeBytes = func(string) (uint64, error) { return 999, nil }

	got, err := p.preview()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Integrity.OK || got.Schema.Backup != 84 || got.Schema.Current != 84 || !got.Schema.OK {
		t.Errorf("integrity/schema = %+v %+v", got.Integrity, got.Schema)
	}
	// parent before child inside the "access" group, label from the live entity.
	var accessOrder []string
	labels := map[string]string{}
	for _, g := range got.Groups {
		for _, tb := range g.Tables {
			labels[tb.Table] = tb.Label
			if g.Group == "access" {
				accessOrder = append(accessOrder, tb.Table)
			}
		}
	}
	idx := func(n string) int {
		for i, x := range accessOrder {
			if x == n {
				return i
			}
		}
		return -1
	}
	if idx("acl_roles") < 0 || idx("acl_role_actions") < 0 || idx("acl_roles") > idx("acl_role_actions") {
		t.Errorf("access order = %v (parent must come first)", accessOrder)
	}
	if labels["obj_crm_x"] != "Khách hàng" {
		t.Errorf("label = %q, want the CURRENT entity label", labels["obj_crm_x"])
	}
	if got.Groups[0].Group != "data" {
		t.Errorf("first group = %s", got.Groups[0].Group)
	}
	for _, g := range got.Groups {
		for _, tb := range g.Tables {
			if tb.Table == "acl_roles" && (tb.RowsInBackup != 1 || tb.RowsNow != 2) {
				t.Errorf("acl_roles = %+v", tb)
			}
		}
	}
	w := map[string]int{}
	for _, x := range got.Warnings {
		w[x.Code] = x.Count
	}
	if w["ROLE_LOST"] != 1 || w["MEMBERSHIP_SKIPPED"] != 1 || w["PENDING_JOBS_KEPT"] != 3 ||
		w["BLOB_MISSING_IN_BACKUP"] != 1 || w["ACCESS_WIDENED"] != got.AccessChanges.Total || got.AccessChanges.Total == 0 {
		t.Errorf("warnings = %v / %+v", w, got.AccessChanges)
	}
	// fileA is live already; fileB has no bytes; live-only file would be removed.
	if got.Blobs.ToWrite != 0 || got.Blobs.ToRemove != 1 {
		t.Errorf("blobs = %+v", got.Blobs)
	}
	if got.AccessChanges.RoleGrantsWidened != 1 || got.AccessChanges.APIKeysAffected != 1 || got.AccessChanges.UsersAffected != 2 {
		t.Errorf("access = %+v", got.AccessChanges)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "SECRET123") || strings.Contains(string(raw), "token=") {
		t.Errorf("the plan leaks an URL query: %s", raw)
	}
	if len(got.Kept) == 0 {
		t.Errorf("kept list is empty")
	}
	if got.PreRestoreEstimate.FreeBytes != 999 || got.PreRestoreEstimate.Files != 2 || got.PreRestoreEstimate.Bytes != 13 {
		t.Errorf("estimate = %+v", got.PreRestoreEstimate)
	}
}

func TestPreview_LabelFallsBackToTheBackupThenTheName(t *testing.T) {
	p := newPlanFx(t, zipSpec{lines: map[string][]string{
		"sys_entities": rowsOf(`{"id":"e1","name":"crm_x","table_name":"obj_crm_x","label":"Nhãn trong bản"}`),
	}})
	p.plan.refs = append(p.plan.refs, TableRef{Table: "obj_crm_x", Rule: Rule{Class: Restore, Group: "data"}})
	got, err := p.preview()
	if err != nil {
		t.Fatal(err)
	}
	// obj_crm_x is not in the file and has no rows live: the plan skips an empty table.
	for _, g := range got.Groups {
		for _, tb := range g.Tables {
			if tb.Table == "obj_crm_x" {
				t.Errorf("an empty table must not be listed: %+v", tb)
			}
		}
	}
	p.plan.counts["obj_crm_x"] = 1
	got, err = p.preview()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, g := range got.Groups {
		for _, tb := range g.Tables {
			if tb.Table == "obj_crm_x" {
				found = true
				if tb.Label != "Nhãn trong bản" {
					t.Errorf("label = %q", tb.Label)
				}
			}
		}
	}
	if !found {
		t.Errorf("table missing")
	}
}

func TestPreview_NothingNarrowedMeansNoWarning(t *testing.T) {
	p := newPlanFx(t, zipSpec{lines: map[string][]string{"acl_roles": rowsOf(`{"id":"` + roleKept + `"}`)}})
	p.plan.access.AddRole(roleKept)
	got, err := p.preview()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessChanges.Total != 0 || len(got.Warnings) != 0 {
		t.Errorf("want none, got %+v / %v", got.AccessChanges, got.Warnings)
	}
}

// The preview must not write anything: the fake store records no Insert, the backend no Put/Delete.
func TestPreview_WritesNothing(t *testing.T) {
	p := newPlanFx(t, zipSpec{lines: map[string][]string{"acl_roles": rowsOf(`{"id":"` + roleKept + `"}`)}})
	if _, err := p.preview(); err != nil {
		t.Fatal(err)
	}
	if p.store.insertCalls != 0 || len(p.backups.deletedKeys()) != 0 || len(p.blobs.objs) != 0 {
		t.Errorf("the preview wrote: inserts=%d deleted=%v", p.store.insertCalls, p.backups.deletedKeys())
	}
}
