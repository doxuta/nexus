//go:build integration

// specs/043 S3a2 — `bpm-seats-read-path`: the approver's inbox (GET /approvals/pending) and the
// /approvals/:id visibility gate read the work seats S3a writes, proven against real MySQL.
//
// What S3a2 promises (tasks.md T049, T052, T056, T060; spec "Nợ còn mở" 21):
//   - for every pending instance a binary of S3a or later has touched, the seat read returns
//     exactly the rows the old projection read returned — same person, same id set;
//   - a seat is trusted only while it agrees with its instance (the node the pin says the
//     instance stands on; the holder the projection names, when it names one) — a seat a pre-S3a
//     binary left behind is ignored and the projection answers instead (D-6 fallback);
//   - an instance with no seat at all (armed by a pre-S3a binary during a rolling deploy) still
//     shows through that fallback;
//   - where the projection is NULL (a pool, several seats — D-5) the seats are the only source;
//   - a row that will not decode is skipped AND logged (#68), on this new read too.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run 'TestPending_|TestListPending_|TestMigrationA_BackfillPendingParity|TestApprovalDetail_Seat'
package services

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"nexus/platform/internal/bpm"
)

// legacyPendingIDs is the inbox read as it stood before S3a2 — the current_approver_* projection
// alone, verbatim from bpm.SQLRepository.ListPendingForUser at 3f04ecc9. It is the oracle the
// seat read is judged against: "the approver sees nothing different" (DAILY-QUEUE #113).
func legacyPendingIDs(t *testing.T, db *sqlx.DB, tenant, userID string, roleIDs []string) []string {
	t.Helper()
	args := []any{tenant, userID}
	pred := "(current_approver_type = 'user' AND current_approver_id = ?)"
	if len(roleIDs) > 0 {
		ph := make([]string, len(roleIDs))
		for i, rid := range roleIDs {
			ph[i] = "?"
			args = append(args, rid)
		}
		pred += " OR (current_approver_type = 'role' AND current_approver_id IN (" + strings.Join(ph, ",") + "))"
	}
	var ids []string
	if err := db.Select(&ids, `SELECT id FROM sys_approval_instances
		WHERE tenant_id = ? AND status = 'pending' AND (`+pred+`) ORDER BY created_at DESC, id DESC`, args...); err != nil {
		t.Fatalf("legacy pending read: %v", err)
	}
	return ids
}

func instanceIDs(insts []bpm.Instance) []string {
	out := make([]string, 0, len(insts))
	for _, i := range insts {
		out = append(out, i.ID)
	}
	return out
}

func sortedCopy(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

// plantSeat writes one OPEN seat row directly — the seat a later binary (a pool, a parallel step)
// or an earlier one (left behind) would have left.
func plantSeat(t *testing.T, db *sqlx.DB, tenant, instID, node, assigneeType, assigneeID string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO sys_bpm_tasks
		(id, tenant_id, instance_id, process_id, entity_name, record_id, node_id, entry_seq, kind,
		 assignee_type, assignee_id, status, version)
		SELECT ?, tenant_id, id, process_id, entity_name, record_id, ?, seq, 'approval', ?, ?, 'open', 0
		FROM sys_approval_instances WHERE tenant_id = ? AND id = ?`,
		uuid.NewString(), node, assigneeType, assigneeID, tenant, instID); err != nil {
		t.Fatalf("plant seat on %s: %v", instID, err)
	}
}

// inboxPerson is one reader of the inbox: a user id and the role ids their token carries.
type inboxPerson struct {
	name  string
	user  string
	roles []string
}

var seatReadPeople = []inboxPerson{
	{"ap1", seatAp1, nil},
	{"ap2", seatAp2, nil},
	{"ap3", seatAp3, []string{seatRoleC}},
	{"role holder", seatRoleUser, []string{seatRoleC}},
	{"outsider", seatAp4, []string{"99999999-0000-0000-0000-000000000a99"}},
}

// TestMigrationA_BackfillPendingParity (T049): the rows production holds today — every shape of
// pending instance a pre-A binary left (pinned, unpinned, reordered, escalated, role-targeted,
// no approver) plus terminal ones — are read through the OLD projection before Migration A and
// through the SEAT read after it. For every person the id set is identical.
func TestMigrationA_BackfillPendingParity(t *testing.T) {
	db := preMigADB(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	steps := `[{"name":"A","approverType":"user","approverId":"` + seatAp1 + `"},{"name":"B","approverType":"user","approverId":"` + seatAp2 + `"},{"name":"C","approverType":"role","approverId":"` + seatRoleC + `"}]`
	proc := insertRawProcess(t, db, tenant, "quotes", "after_create", steps)
	pin := `{"steps":` + steps + `}`
	reordered := `{"steps":[{"id":"s3","name":"C","approverType":"role","approverId":"` + seatRoleC + `"},{"id":"s1","name":"A","approverType":"user","approverId":"` + seatAp1 + `"}]}`
	for _, in := range []legacyInstance{
		{approverType: "user", approverID: seatAp1},                                          // pre-000065, step 1
		{step: 1, approverType: "user", approverID: seatAp2, snapshot: pin},                  // pinned, step 2
		{step: 2, approverType: "role", approverID: seatRoleC, snapshot: pin},                // role step
		{step: 1, approverType: "user", approverID: seatAp1, snapshot: reordered},            // reordered pin
		{step: 1, approverType: "user", approverID: seatAp3, snapshot: pin},                  // delegated to ap3
		{step: 1, approverType: "user", approverID: seatAp2, snapshot: pin, escalated: true}, // breached
		{snapshot: pin}, // no approver projected — nobody's
		{approverType: "user", approverID: seatAp1, snapshot: pin, status: "approved"},
		{approverType: "role", approverID: seatRoleC, snapshot: pin, status: "cancelled"},
	} {
		in.tenant, in.process, in.entity = tenant, proc, "quotes"
		plantInstance(t, db, in)
	}
	// Another tenant's instance on the same approver must stay out of tenant A's inbox.
	plantInstance(t, db, legacyInstance{tenant: o3aTenantB, process: proc, entity: "quotes", approverType: "user", approverID: seatAp1, snapshot: pin})

	before := map[string][]string{}
	for _, p := range seatReadPeople {
		before[p.name] = legacyPendingIDs(t, db, tenant, p.user, p.roles)
	}
	if len(before["ap1"]) != 2 || len(before["role holder"]) != 1 || len(before["outsider"]) != 0 {
		t.Fatalf("fixture drifted: legacy inbox sizes %v", before)
	}

	applyMigAAndRest(t, db)

	repo := bpm.NewSQLRepository(db, nil)
	for _, p := range seatReadPeople {
		got, err := repo.ListPendingForUser(ctx, tenant, p.user, p.roles, nil)
		if err != nil {
			t.Fatalf("%s: ListPendingForUser: %v", p.name, err)
		}
		// Same rows in the same order: the list is newest-first and the FE renders it as sent.
		if ids := instanceIDs(got); !reflect.DeepEqual(ids, before[p.name]) && !(len(ids) == 0 && len(before[p.name]) == 0) {
			t.Fatalf("%s: seat read %v; the projection read before Migration A returned %v", p.name, ids, before[p.name])
		}
	}
}

// TestPending_SeatlessInstanceFallback (T052, "Nợ còn mở" 21): in a rolling deploy a pre-S3a
// binary keeps arming and moving instances without writing seats. The inbox must neither lose
// such an instance nor trust the seat it left behind.
func TestPending_SeatlessInstanceFallback(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	st.createEntity(t, ctx, tenant, "seatread", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	proc, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: "read-path", EntityName: "seatread", Trigger: "after_create",
		Steps: []ApprovalStepInput{
			{Name: "A", ApproverType: "user", ApproverID: seatAp1},
			{Name: "B", ApproverType: "user", ApproverID: seatAp2},
		},
		IsActive: boolPtr(true), ActorID: seatSubmitter,
	})
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	pin := `{"steps":[{"id":"s1","name":"A","approverType":"user","approverId":"` + seatAp1 + `"},{"id":"s2","name":"B","approverType":"user","approverId":"` + seatAp2 + `"}]}`
	plant := func(in legacyInstance) string {
		in.tenant, in.process, in.entity = tenant, proc.ID, "seatread"
		if in.snapshot == "" {
			in.snapshot = pin
		}
		return plantInstance(t, st.sdb, in)
	}

	// (a) armed by an old binary after the backfill: no seat at all.
	seatless := plant(legacyInstance{approverType: "user", approverID: seatAp1})
	// (b) armed here (seat s1 for ap1), then approved by an old binary: the instance is on s2 for
	// ap2, the s1 seat is still open.
	rid, err := st.recordSvc.Create(ctx, bpmAdmin(seatSubmitter), tenant, seatSubmitter, "seatread", map[string]any{"status": "draft"})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	st.drain(t)
	movedOn := st.onlyPendingInstance(t, tenant, "seatread", rid).ID
	if _, err := st.sdb.Exec(`UPDATE sys_approval_instances SET current_step = 1, current_approver_id = ?
		WHERE tenant_id = ? AND id = ?`, seatAp2, tenant, movedOn); err != nil {
		t.Fatalf("old-binary approve: %v", err)
	}
	// (c) seat s1 for ap1, then delegated by an old binary to ap3: same node, other holder.
	handedOn := plant(legacyInstance{approverType: "user", approverID: seatAp3})
	plantSeat(t, st.sdb, tenant, handedOn, "s1", "user", seatAp1)
	// (d) NULL projection — a pool or several seats (D-5): the seats are the only source.
	pooled := plant(legacyInstance{})
	plantSeat(t, st.sdb, tenant, pooled, "s1", "user", seatAp1)
	plantSeat(t, st.sdb, tenant, pooled, "s1", "role", seatRoleC)
	// (e) NULL projection, the only seat on a node the instance is not on: nobody's.
	strayPool := plant(legacyInstance{})
	plantSeat(t, st.sdb, tenant, strayPool, "s2", "user", seatAp1)

	want := map[string][]string{
		"ap1":         {pooled},
		"ap2":         {movedOn},
		"ap3":         {handedOn, pooled},
		"role holder": {pooled},
		"outsider":    nil,
	}
	// (a) is ap1's too — but only through the fallback.
	want["ap1"] = append(want["ap1"], seatless)
	for _, p := range seatReadPeople {
		got, err := st.approval.ListPending(ctx, tenant, bpm.Actor{UserID: p.user, RoleIDs: p.roles})
		if err != nil {
			t.Fatalf("%s: ListPending: %v", p.name, err)
		}
		if g, w := sortedCopy(instanceIDs(got)), sortedCopy(want[p.name]); !reflect.DeepEqual(g, w) && !(len(g) == 0 && len(w) == 0) {
			t.Errorf("%s: inbox %v; want %v (seatless=%s movedOn=%s handedOn=%s pooled=%s strayPool=%s)",
				p.name, g, w, seatless, movedOn, handedOn, pooled, strayPool)
		}
	}
}

// TestApprovalDetail_SeatHolderSeesIt: GET /approvals/:id lets a non-admin in on exactly the
// rule of the inbox row they clicked — a seat holder on a NULL-projection step is not turned
// away with a 404, and a seat the instance has left behind opens nothing.
func TestApprovalDetail_SeatHolderSeesIt(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	proc := insertRawProcess(t, st.sdb, tenant, "seatdetail", "manual",
		`[{"id":"s1","name":"A","approverType":"user","approverId":"`+seatAp1+`"}]`)
	pin := `{"steps":[{"id":"s1","name":"A","approverType":"user","approverId":"` + seatAp1 + `"}]}`
	pooled := plantInstance(t, st.sdb, legacyInstance{tenant: tenant, process: proc, entity: "seatdetail", snapshot: pin})
	plantSeat(t, st.sdb, tenant, pooled, "s1", "user", seatAp1)
	stale := plantInstance(t, st.sdb, legacyInstance{tenant: tenant, process: proc, entity: "seatdetail", snapshot: pin,
		approverType: "user", approverID: seatAp3})
	plantSeat(t, st.sdb, tenant, stale, "s1", "user", seatAp1)

	cases := []struct {
		name   string
		inst   string
		actor  bpm.Actor
		wantOK bool
	}{
		{"seat holder on a NULL-projection step", pooled, bpm.Actor{UserID: seatAp1}, true},
		{"someone without a seat", pooled, bpm.Actor{UserID: seatAp2}, false},
		{"holder of a seat the instance left behind", stale, bpm.Actor{UserID: seatAp1}, false},
		{"the holder the projection names", stale, bpm.Actor{UserID: seatAp3}, true},
	}
	for _, c := range cases {
		_, err := st.approval.GetApprovalDetail(ctx, RecordScope{UserID: c.actor.UserID}, tenant, c.inst, c.actor)
		if c.wantOK && err != nil {
			t.Errorf("%s: GetApprovalDetail: %v; want the detail", c.name, err)
		}
		if !c.wantOK && (err == nil || !strings.Contains(err.Error(), "không tìm thấy")) {
			t.Errorf("%s: GetApprovalDetail err = %v; want APPROVAL_INSTANCE_NOT_FOUND", c.name, err)
		}
	}
}

// TestListPending_DecodeErrorLogged (T056, #68 closed by a4a58262 — this pins it on the NEW read):
// a pending row whose history will not decode is left out of the inbox, the rest still listed,
// and exactly one ERROR line names it by identifiers only.
func TestListPending_DecodeErrorLogged(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	proc := insertRawProcess(t, st.sdb, tenant, "seatdecode", "manual",
		`[{"id":"s1","name":"A","approverType":"user","approverId":"`+seatAp1+`"}]`)
	pin := `{"steps":[{"id":"s1","name":"A","approverType":"user","approverId":"` + seatAp1 + `"}]}`
	good := plantInstance(t, st.sdb, legacyInstance{tenant: tenant, process: proc, entity: "seatdecode", snapshot: pin,
		approverType: "user", approverID: seatAp1})
	plantSeat(t, st.sdb, tenant, good, "s1", "user", seatAp1)
	bad := plantInstance(t, st.sdb, legacyInstance{tenant: tenant, process: proc, entity: "seatdecode", snapshot: pin,
		approverType: "user", approverID: seatAp1, history: `{"step":0}`})
	plantSeat(t, st.sdb, tenant, bad, "s1", "user", seatAp1)

	core, logs := observer.New(zapcore.InfoLevel)
	repo := bpm.NewSQLRepository(st.sdb, zap.New(core))
	got, err := repo.ListPendingForUser(ctx, tenant, seatAp1, nil, nil)
	if err != nil {
		t.Fatalf("ListPendingForUser: %v", err)
	}
	if ids := instanceIDs(got); !reflect.DeepEqual(ids, []string{good}) {
		t.Fatalf("inbox %v; want only the decodable row %s", ids, good)
	}
	entries := logs.FilterMessageSnippet("undecodable").AllUntimed()
	if len(entries) != 1 {
		t.Fatalf("log lines = %d (%v); want exactly one naming the skipped row", len(entries), logs.AllUntimed())
	}
	f := entries[0].ContextMap()
	if entries[0].Level != zapcore.ErrorLevel || f["op"] != "ListPendingForUser" || f["instance_id"] != bad || f["tenant_id"] != tenant {
		t.Fatalf("log line %s %v; want ERROR op=ListPendingForUser instance_id=%s tenant_id=%s", entries[0].Level, f, bad, tenant)
	}
	for k, v := range f {
		if s, ok := v.(string); ok && strings.Contains(s, `"step"`) {
			t.Fatalf("log field %s carries row content %q — identifiers only", k, s)
		}
	}
}

// TestSeatDrift_CountsWhatTheFallbackStillCarries (T063): the Doctor's reading — every pending
// instance of the tenant whose inbox rests on the projection fallback, named by kind: no open
// seat at all, or open seats none of which agrees with it. A healthy instance, a finished one
// and another tenant's are not counted.
func TestSeatDrift_CountsWhatTheFallbackStillCarries(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	st.createEntity(t, ctx, tenant, "seatdrift", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	proc, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: "drift", EntityName: "seatdrift", Trigger: "after_create",
		Steps: []ApprovalStepInput{
			{Name: "A", ApproverType: "user", ApproverID: seatAp1},
			{Name: "B", ApproverType: "user", ApproverID: seatAp2},
		},
		IsActive: boolPtr(true), ActorID: seatSubmitter,
	})
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	pin := `{"steps":[{"id":"s1","name":"A","approverType":"user","approverId":"` + seatAp1 + `"},{"id":"s2","name":"B","approverType":"user","approverId":"` + seatAp2 + `"}]}`
	plant := func(tenantID string, in legacyInstance) string {
		in.tenant, in.process, in.entity, in.snapshot = tenantID, proc.ID, "seatdrift", pin
		return plantInstance(t, st.sdb, in)
	}
	rid, err := st.recordSvc.Create(ctx, bpmAdmin(seatSubmitter), tenant, seatSubmitter, "seatdrift", map[string]any{"status": "draft"})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	st.drain(t)
	healthy := st.onlyPendingInstance(t, tenant, "seatdrift", rid).ID

	seatless := plant(tenant, legacyInstance{approverType: "user", approverID: seatAp1})
	wrongNode := plant(tenant, legacyInstance{step: 1, approverType: "user", approverID: seatAp2})
	plantSeat(t, st.sdb, tenant, wrongNode, "s1", "user", seatAp1)
	wrongHolder := plant(tenant, legacyInstance{approverType: "user", approverID: seatAp3})
	plantSeat(t, st.sdb, tenant, wrongHolder, "s1", "user", seatAp1)
	finished := plant(tenant, legacyInstance{approverType: "user", approverID: seatAp1, status: "approved"})
	otherTenant := plant(o3aTenantB, legacyInstance{approverType: "user", approverID: seatAp1})

	rows, err := st.repo.ListSeatDrift(ctx, tenant, 10)
	if err != nil {
		t.Fatalf("ListSeatDrift: %v", err)
	}
	got := map[string]bpm.SeatDrift{}
	for _, r := range rows {
		if r.EntityName != "seatdrift" || r.RecordID == "" {
			t.Fatalf("drift row %+v does not name its record", r)
		}
		got[r.InstanceID] = r.Drift
	}
	want := map[string]bpm.SeatDrift{
		seatless:    bpm.SeatDriftMissing,
		wrongNode:   bpm.SeatDriftMismatch,
		wrongHolder: bpm.SeatDriftMismatch,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("drift %v; want %v (healthy=%s finished=%s otherTenant=%s)", got, want, healthy, finished, otherTenant)
	}
	if rows, err := st.repo.ListSeatDrift(ctx, tenant, 2); err != nil || len(rows) != 2 {
		t.Fatalf("ListSeatDrift limit 2 = %d rows, err %v; want the limit honoured", len(rows), err)
	}
}
