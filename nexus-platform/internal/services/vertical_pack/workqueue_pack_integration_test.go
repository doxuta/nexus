//go:build integration

// PACK-QUEUES integration (../specs/012-work-queues/spec.md §5): builtin packs ship "Việc Hôm Nay" queues.
//
//  1. Installing the builtin Ticketing pack seeds its two queues through the validated
//     WorkQueueService path, with the role-scoped queue's RoleName resolved to the role id the
//     SAME install created (Pass 6 → Pass 8c handoff).
//  2. Re-install is idempotent: both queues skip (list-then-skip; the table UNIQUE would 409).
//  3. A pack queue naming a role that exists neither in the pack nor the tenant HARD-fails the
//     install (a role-scoped queue silently degrading to visible-to-everyone would widen access).
//  4. Provenance records the queues under item type "work_queue" (fresh-install-unchanged is
//     asserted pack-wide by TestProvenance_FreshBuiltinInstallDiffsUnchanged).
package vertical_pack

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workqueue"
)

func TestPackQueues_BuiltinTicketingSeedsQueues(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, _ := vpBuildWF(gdb, sdb)
	store := newFakeItemStore()
	svc.WithItemStore(store)
	ctx := context.Background()

	if err := gdb.Exec(
		"INSERT INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, ?, 'active')",
		vpSeedUserID, vpTenantA, "packq@it.example.com", "x",
	).Error; err != nil {
		t.Fatalf("seed install user: %v", err)
	}

	pack, ok := BuiltinPack("ticketing")
	if !ok {
		t.Fatal("builtin ticketing pack not found")
	}
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("install builtin ticketing: %v", err)
	}
	if len(res.WorkQueuesCreated) != 2 || len(res.WorkQueuesSkipped) != 0 {
		t.Fatalf("created=%v skipped=%v, want 2 created / 0 skipped", res.WorkQueuesCreated, res.WorkQueuesSkipped)
	}

	queues, err := workqueue.New(gdb).ListAll(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list queues: %v", err)
	}
	byName := map[string]workqueue.Queue{}
	for _, q := range queues {
		byName[q.Name] = q
	}
	agentQ, ok := byName["ticket_toi_han_hom_nay"]
	if !ok {
		t.Fatalf("role-scoped queue not seeded; have %v", res.WorkQueuesCreated)
	}
	// RoleName "tk_agent" resolved to the role id THIS install created.
	roles, err := auth.NewRoleRepository(gdb).ListRoles(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	agentRoleID := ""
	for _, r := range roles {
		if r.Name == "tk_agent" {
			agentRoleID = r.ID
		}
	}
	if agentRoleID == "" {
		t.Fatal("tk_agent role not created by install")
	}
	if agentQ.RoleID == nil || *agentQ.RoleID != agentRoleID {
		t.Fatalf("queue role_id = %v, want %s", agentQ.RoleID, agentRoleID)
	}
	if everyoneQ, ok := byName["ticket_khan_dang_mo"]; !ok || everyoneQ.RoleID != nil {
		t.Fatalf("everyone queue wrong: present=%v roleId=%v", ok, byName["ticket_khan_dang_mo"].RoleID)
	}
	// Filters survived the round-trip (validated tokens ride verbatim into storage).
	if len(agentQ.Filters) != 2 || agentQ.Filters[0].Value != "today" {
		t.Fatalf("agent queue filters = %v", agentQ.Filters)
	}

	// Provenance rows landed under the work_queue item type.
	items, err := store.ListItems(ctx, vpTenantA, "ticketing")
	if err != nil {
		t.Fatalf("list provenance: %v", err)
	}
	wq := 0
	for _, it := range items {
		if it.Type == ItemWorkQueue {
			wq++
		}
	}
	if wq != 2 {
		t.Fatalf("work_queue provenance rows = %d, want 2", wq)
	}

	// Re-install: idempotent skip, still exactly two queues.
	res2, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("re-install: %v", err)
	}
	if len(res2.WorkQueuesCreated) != 0 || len(res2.WorkQueuesSkipped) != 2 {
		t.Fatalf("re-install created=%v skipped=%v", res2.WorkQueuesCreated, res2.WorkQueuesSkipped)
	}
	if queues, err = workqueue.New(gdb).ListAll(ctx, vpTenantA); err != nil || len(queues) != 2 {
		t.Fatalf("after re-install: queues=%d err=%v", len(queues), err)
	}
}

// Upgrade path (review SHOULD-FIX 1): a tenant that installed an OLDER pack version has the
// entity WITHOUT the sort-column index the new version's queue expects. Pass 1 skips the entity
// (options never reconciled), so the queue can never validate — the install must COMPLETE with a
// warned skip, not wedge on 422 forever.
func TestPackQueues_UpgradePathDegradesToSkip(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, _ := vpBuildWF(gdb, sdb)
	ctx := context.Background()

	if err := gdb.Exec(
		"INSERT INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, ?, 'active')",
		vpSeedUserID, vpTenantA, "packq3@it.example.com", "x",
	).Error; err != nil {
		t.Fatalf("seed install user: %v", err)
	}

	entity := PackEntity{Name: "u_items", Label: "Items", Fields: []PackField{
		{Name: "title", Label: "Title", FieldType: "text"},
		{Name: "due", Label: "Due", FieldType: "date"}, // v1: NOT indexed
	}}
	v1 := &Pack{Manifest: &Manifest{Name: "U", Slug: "utest", Version: "1.0.0"}, Entities: []PackEntity{entity}}
	if _, err := svc.Install(ctx, vpTenantA, vpSeedUserID, v1); err != nil {
		t.Fatalf("install v1: %v", err)
	}

	v2entity := entity
	v2entity.Fields = []PackField{
		{Name: "title", Label: "Title", FieldType: "text"},
		{Name: "due", Label: "Due", FieldType: "date", Options: map[string]any{"indexed": true}}, // v2 indexes it…
	}
	v2 := &Pack{
		Manifest: &Manifest{Name: "U", Slug: "utest", Version: "2.0.0"},
		Entities: []PackEntity{v2entity}, // …but Pass 1 skips the pre-existing entity, options unreconciled
		WorkQueues: []PackWorkQueue{{
			Name: "due_queue", Label: "Sắp đến hạn", EntityName: "u_items", SortField: "due",
		}},
	}
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, v2)
	if err != nil {
		t.Fatalf("v2 re-install must complete, got: %v", err)
	}
	if len(res.WorkQueuesCreated) != 0 || len(res.WorkQueuesSkipped) != 1 {
		t.Fatalf("created=%v skipped=%v, want degrade-to-skip", res.WorkQueuesCreated, res.WorkQueuesSkipped)
	}
	warned := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "due_queue") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("degrade must be warned, warnings=%v", res.Warnings)
	}
}

func TestPackQueues_UnknownRoleNameFailsInstall(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, _ := vpBuildWF(gdb, sdb)
	ctx := context.Background()

	if err := gdb.Exec(
		"INSERT INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, ?, 'active')",
		vpSeedUserID, vpTenantA, "packq2@it.example.com", "x",
	).Error; err != nil {
		t.Fatalf("seed install user: %v", err)
	}

	pack := &Pack{
		Manifest: &Manifest{Name: "Q", Slug: "qtest", Version: "1.0.0"},
		Entities: []PackEntity{{Name: "q_items", Label: "Items", Fields: []PackField{
			{Name: "title", Label: "Title", FieldType: "text"},
		}}},
		WorkQueues: []PackWorkQueue{{
			Name: "ghost_queue", Label: "Ghost", EntityName: "q_items", RoleName: "ghost_role",
		}},
	}
	_, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	var ve *services.ValidationError
	if !errors.As(err, &ve) || ve.Code != "WORKQUEUE_ROLE_NOT_FOUND" {
		t.Fatalf("install err = %v, want WORKQUEUE_ROLE_NOT_FOUND", err)
	}
}
