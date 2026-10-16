//go:build integration

// O3a metadata-cache — CACHE-WIRED end-to-end integration test (LENS S: tenant isolation).
//
// WHY THIS FILE EXISTS: the existing integration harness wires services WITHOUT the cache, so
// the cached path (RecordService reading THROUGH MetaCache; entity/field WRITE services
// invalidating the SAME cache instance after commit) was only unit-tested against fakes. This
// test wires the PRODUCTION composition against REAL MySQL:
//
//	real metadata.Repository (GORM) + real dynrepo (sqlx)
//	  -> wrapped by a counting EntityCatalog decorator (counts real DB hits, passes through)
//	  -> wrapped by ONE shared *MetaCache
//	  -> EntityService/FieldService .WithMetaCache(mc) (write side: invalidate-after-commit)
//	  -> NewRecordService(mc, dynRepo)               (read side: resolve THROUGH the cache)
//
// It proves the four LENS-S attacks on the REAL composition:
//  1. CROSS-TENANT SERVE: two tenants, SAME entity name "accounts", DIFFERENT fields. Prime A's
//     cache, then read as B -> B MUST get B's own definition/fields, never A's. (countingCatalog
//     proves A's repeat read is a true cache hit, so the leak surface is actually exercised.)
//  2. SINGLE-FLIGHT CROSS-TENANT NON-MERGE: race a tenantA load and a tenantB load on the same
//     name concurrently (barrier-synchronized so they truly overlap) -> loader runs TWICE and each
//     goroutine gets ITS OWN tenant's value (keys discriminate; no coalesce-across-tenant).
//  3. STALE-AFTER-INVALIDATE: start a load, Invalidate mid-flight, confirm next read RELOADS
//     (generation guard; end-to-end mirror of cache.TestInvalidate_DuringLoad_NotCached).
//  4. SOFT-DELETE NOT-FOUND: prime an entity's cache, SoftDeleteEntity, re-resolve -> the cached
//     resolve MUST return ErrEntityNotFound (no stale grant to a gone entity; errors not cached).
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestO3aWired -race -v
//
// Each test creates and drops its OWN throwaway database (never nexus_platform). Requires a
// reachable MySQL 8 root connection and the golang-migrate CLI on PATH (or ~/go/bin/migrate).
package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

const o3aMigrationsDir = "../../migrations"

const (
	o3aTenantA = "aaaaaaaa-0a0a-0a0a-0a0a-aaaaaaaaaaaa"
	o3aTenantB = "bbbbbbbb-0b0b-0b0b-0b0b-bbbbbbbbbbbb"
)

var o3aDBCounter int64

func o3aUniqueDBName() string {
	n := atomic.AddInt64(&o3aDBCounter, 1)
	return fmt.Sprintf("nexus_o3a_it_%d_%d", os.Getpid(), n)
}

// o3aSplitDSN mirrors the rm18 helper: split TEST_DATABASE_URL so GORM, sqlx, and migrate all
// target the SAME throwaway DB (no split-brain).
func o3aSplitDSN(t *testing.T) (creds, host, params string) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		base = "root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local"
	}
	at := strings.Index(base, "@tcp(")
	if at < 0 {
		t.Fatalf("o3aSplitDSN: unexpected DSN shape %q", base)
	}
	creds = base[:at]
	rest := base[at:]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		t.Fatalf("o3aSplitDSN: no db segment in DSN %q", base)
	}
	host = rest[:slash+1]
	if q := strings.Index(rest, "?"); q >= 0 {
		params = rest[q:]
	}
	return creds, host, params
}

func o3aAdminDSN(t *testing.T) string {
	creds, host, params := o3aSplitDSN(t)
	return creds + host + params
}

// o3aDSN is the DSN the tests OPEN (GORM + sqlx). It pins loc=UTC (last value wins), production's
// setting: with loc=Local on a +07 host over a UTC MySQL, Go-clock writers (GORM created_at) and
// DB-clock writers (CURRENT_TIMESTAMP draft stamps) disagree by 7h, which flipped every
// "created after the draft save / publish" comparison (the nexus-mac CI runner is +07).
func o3aDSN(t *testing.T, dbName string) string {
	creds, host, params := o3aSplitDSN(t)
	sep := "?"
	if params != "" {
		sep = "&"
	}
	return creds + host + dbName + params + sep + "loc=UTC"
}
func o3aMigrateDSN(t *testing.T, dbName string) string {
	creds, host, params := o3aSplitDSN(t)
	return "mysql://" + creds + host + dbName + params
}

func o3aMigrateBin(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("migrate"); err == nil {
		return p
	}
	if home, _ := os.UserHomeDir(); home != "" {
		cand := home + "/go/bin/migrate"
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return ""
}

// o3aSetup builds a fresh throwaway DB, runs ALL migrations up, and returns a GORM handle + sqlx
// handle bound to it. Skips (not fails) when MySQL or migrate is unavailable.
func o3aSetup(t *testing.T) (*gorm.DB, *sqlx.DB) {
	t.Helper()
	dbName := o3aUniqueDBName()

	admin, err := gorm.Open(gormmysql.Open(o3aAdminDSN(t)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Skipf("o3a: MySQL not reachable (%v) — skipping", err)
	}
	if sqlDB, err := admin.DB(); err != nil || sqlDB.Ping() != nil {
		t.Skipf("o3a: MySQL not pingable — skipping")
	}
	if err := admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`").Error; err != nil {
		t.Fatalf("o3a: drop throwaway DB: %v", err)
	}
	if err := admin.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci").Error; err != nil {
		t.Fatalf("o3a: create throwaway DB: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	migrateBin := o3aMigrateBin(t)
	if migrateBin == "" {
		t.Skip("o3a: migrate binary not found — skipping")
	}
	cmd := exec.Command(migrateBin, "-path", o3aMigrationsDir, "-database", o3aMigrateDSN(t, dbName), "up")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("o3a: migrate up: %v\n%s", err, out)
	}

	gdb, err := gorm.Open(gormmysql.Open(o3aDSN(t, dbName)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("o3a: open gorm: %v", err)
	}
	sdb, err := sqlx.Connect("mysql", o3aDSN(t, dbName))
	if err != nil {
		t.Fatalf("o3a: open sqlx: %v", err)
	}
	t.Cleanup(func() {
		if d, err := gdb.DB(); err == nil {
			_ = d.Close()
		}
		_ = sdb.Close()
	})
	return gdb, sdb
}

// countingCatalogReal wraps the REAL metadata repository as an EntityCatalog, passing every call
// through to MySQL while counting how many actually hit the DB. It is what MetaCache wraps, so a
// repeat read served from cache shows as ZERO extra hits — proving the cache really served (load
// stays real). Optional barrier (hold/release) lets the concurrency tests force two loads to
// truly overlap without time.Sleep flakiness.
type countingCatalogReal struct {
	inner       EntityCatalog
	entityCalls int64
	fieldCalls  int64

	// barrier: when hold != nil, every entity load blocks on <-hold after signaling started, so
	// the test can guarantee N loads are concurrently in flight before releasing them.
	mu      sync.Mutex
	hold    chan struct{}
	started chan struct{}
}

func newCountingCatalog(inner EntityCatalog) *countingCatalogReal {
	return &countingCatalogReal{inner: inner}
}

func (c *countingCatalogReal) GetEntityByName(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error) {
	atomic.AddInt64(&c.entityCalls, 1)
	c.mu.Lock()
	hold, started := c.hold, c.started
	c.mu.Unlock()
	if hold != nil {
		if started != nil {
			started <- struct{}{}
		}
		<-hold
	}
	return c.inner.GetEntityByName(ctx, tenantID, name)
}

func (c *countingCatalogReal) ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error) {
	atomic.AddInt64(&c.fieldCalls, 1)
	return c.inner.ListFields(ctx, tenantID, entityID)
}

func (c *countingCatalogReal) setBarrier(hold, started chan struct{}) {
	c.mu.Lock()
	c.hold, c.started = hold, started
	c.mu.Unlock()
}

// o3aWire builds the full production composition over the real repos, sharing ONE MetaCache.
func o3aWire(t *testing.T, ttl time.Duration) (*countingCatalogReal, *MetaCache, *EntityService, *FieldService, *RecordService) {
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	counter := newCountingCatalog(metaRepo)
	mc := NewMetaCache(counter, ttl)
	entitySvc := NewEntityService(dynRepo, metaRepo).WithMetaCache(mc)
	fieldSvc := NewFieldService(dynRepo, metaRepo).WithMetaCache(mc)
	recordSvc := NewRecordService(mc, dynRepo)
	_ = recordSvc
	return counter, mc, entitySvc, fieldSvc, recordSvc
}

// -----------------------------------------------------------------------------
// Attack 1: CROSS-TENANT SERVE — same name, different fields; prime A, read B.
// -----------------------------------------------------------------------------

func TestO3aWired_CrossTenantServe(t *testing.T) {
	counter, mc, entitySvc, _, recordSvc := o3aWire(t, DefaultMetaTTL)
	ctx := context.Background()

	// Tenant A: entity "accounts" with field a_only.
	entA, _, err := entitySvc.CreateEntity(ctx, o3aTenantA, CreateEntityInput{
		Name: "accounts", Label: "A Accounts",
		Fields: []FieldInput{{Name: "a_only", Label: "A Only", FieldType: "text"}},
	})
	if err != nil {
		t.Fatalf("create A.accounts: %v", err)
	}
	// Tenant B: SAME name "accounts" but field b_only (shares the physical obj_accounts table;
	// reconcile ALTERs b_only in). Definitions DIFFER per tenant on purpose.
	entB, _, err := entitySvc.CreateEntity(ctx, o3aTenantB, CreateEntityInput{
		Name: "accounts", Label: "B Accounts",
		Fields: []FieldInput{{Name: "b_only", Label: "B Only", FieldType: "text"}},
	})
	if err != nil {
		t.Fatalf("create B.accounts: %v", err)
	}
	if entA.ID == entB.ID {
		t.Fatalf("entity IDs collided across tenants: %s — fieldsKey would collide", entA.ID)
	}

	// PRIME tenant A's cache (entity + fields) by resolving as A through the cache.
	a1, _ := mc.GetEntityByName(ctx, o3aTenantA, "accounts")
	fA1, _ := mc.ListFields(ctx, o3aTenantA, a1.ID)
	if a1.TenantID != o3aTenantA || a1.Label != "A Accounts" {
		t.Fatalf("A prime got wrong def: tenant=%s label=%s", a1.TenantID, a1.Label)
	}
	if !hasField(fA1, "a_only") || hasField(fA1, "b_only") {
		t.Fatalf("A fields wrong: %v", fieldNames(fA1))
	}
	entHitsAfterPrime := atomic.LoadInt64(&counter.entityCalls)

	// Repeat read as A -> MUST be a cache hit (0 extra entity DB hits) — proves cache serves.
	a2, _ := mc.GetEntityByName(ctx, o3aTenantA, "accounts")
	if got := atomic.LoadInt64(&counter.entityCalls); got != entHitsAfterPrime {
		t.Fatalf("A repeat read hit DB (%d -> %d); cache did not serve — leak surface not exercised", entHitsAfterPrime, got)
	}
	if a2.Label != "A Accounts" {
		t.Fatalf("A repeat read label=%s; want A Accounts", a2.Label)
	}

	// NOW read as tenant B with A's entry hot. B MUST get B's own def, NEVER A's.
	b1, err := mc.GetEntityByName(ctx, o3aTenantB, "accounts")
	if err != nil {
		t.Fatalf("B read: %v", err)
	}
	if b1.TenantID != o3aTenantB {
		t.Fatalf("CROSS-TENANT LEAK: B received tenant=%s (want %s)", b1.TenantID, o3aTenantB)
	}
	if b1.Label != "B Accounts" {
		t.Fatalf("CROSS-TENANT LEAK: B received label=%q (A's def?), want B Accounts", b1.Label)
	}
	if b1.ID != entB.ID {
		t.Fatalf("CROSS-TENANT LEAK: B received entity ID %s, want B's %s", b1.ID, entB.ID)
	}

	// B's fields keyed by B's UUID -> B's own field list, never A's.
	fB, err := mc.ListFields(ctx, o3aTenantB, b1.ID)
	if err != nil {
		t.Fatalf("B ListFields: %v", err)
	}
	if !hasField(fB, "b_only") || hasField(fB, "a_only") {
		t.Fatalf("CROSS-TENANT LEAK: B fields = %v; want [b_only] only", fieldNames(fB))
	}

	// End-to-end via RecordService.resolve as B: insert+read a B record and confirm it never
	// surfaces A's data shape. (resolve is unexported; exercise it via a record round-trip.)
	scope := RecordScope{UserID: "u-b", IsAdmin: true}
	id, err := recordSvc.Create(ctx, scope, o3aTenantB, "u-b", "accounts", map[string]any{"b_only": "hello-b"})
	if err != nil {
		t.Fatalf("B Create through cached resolve: %v", err)
	}
	rec, err := recordSvc.Get(ctx, scope, o3aTenantB, "accounts", id, nil, nil)
	if err != nil {
		t.Fatalf("B GetRecord: %v", err)
	}
	if rec.Data["b_only"] != "hello-b" {
		t.Fatalf("B record data wrong: %v", rec.Data)
	}
}

// -----------------------------------------------------------------------------
// Attack 2: SINGLE-FLIGHT CROSS-TENANT NON-MERGE — race two tenants, same name.
// -----------------------------------------------------------------------------

func TestO3aWired_SingleflightNoCrossTenantMerge(t *testing.T) {
	counter, mc, entitySvc, _, _ := o3aWire(t, DefaultMetaTTL)
	ctx := context.Background()

	if _, _, err := entitySvc.CreateEntity(ctx, o3aTenantA, CreateEntityInput{
		Name: "leads", Label: "A Leads",
		Fields: []FieldInput{{Name: "a_only", FieldType: "text"}},
	}); err != nil {
		t.Fatalf("create A.leads: %v", err)
	}
	if _, _, err := entitySvc.CreateEntity(ctx, o3aTenantB, CreateEntityInput{
		Name: "leads", Label: "B Leads",
		Fields: []FieldInput{{Name: "b_only", FieldType: "text"}},
	}); err != nil {
		t.Fatalf("create B.leads: %v", err)
	}

	// Arm a barrier: both loads must be in flight before either is released, so they truly
	// overlap (single-flight window). Two distinct tenant keys => loader must run for EACH.
	hold := make(chan struct{})
	started := make(chan struct{}, 2)
	counter.setBarrier(hold, started)
	base := atomic.LoadInt64(&counter.entityCalls)

	type res struct {
		def *metadata.EntityDefinition
		err error
	}
	resA := make(chan res, 1)
	resB := make(chan res, 1)
	go func() { d, e := mc.GetEntityByName(ctx, o3aTenantA, "leads"); resA <- res{d, e} }()
	go func() { d, e := mc.GetEntityByName(ctx, o3aTenantB, "leads"); resB <- res{d, e} }()

	// Wait until BOTH loads are concurrently in flight, then release.
	<-started
	<-started
	close(hold)

	ra := <-resA
	rb := <-resB
	if ra.err != nil || rb.err != nil {
		t.Fatalf("load errs: A=%v B=%v", ra.err, rb.err)
	}
	// Distinct keys => loader ran TWICE (no cross-tenant coalesce).
	if got := atomic.LoadInt64(&counter.entityCalls) - base; got != 2 {
		t.Fatalf("entity loader ran %d times for 2 distinct tenants; want 2 (keys must NOT coalesce across tenant)", got)
	}
	// Each goroutine got ITS OWN tenant's value — not the other's.
	if ra.def.TenantID != o3aTenantA || ra.def.Label != "A Leads" {
		t.Fatalf("CROSS-TENANT MERGE: A goroutine got tenant=%s label=%s", ra.def.TenantID, ra.def.Label)
	}
	if rb.def.TenantID != o3aTenantB || rb.def.Label != "B Leads" {
		t.Fatalf("CROSS-TENANT MERGE: B goroutine got tenant=%s label=%s", rb.def.TenantID, rb.def.Label)
	}

	// Mirror: SAME key concurrent must COALESCE to ONE load (key both discriminates AND coalesces).
	// tenantA "leads" is now cached from the race above; invalidate it so there is exactly one
	// cold load for the leader to perform (followers must coalesce onto it, not reload).
	mc.InvalidateEntityDef(o3aTenantA, "leads")
	hold2 := make(chan struct{})
	started2 := make(chan struct{}, 8)
	counter.setBarrier(hold2, started2)
	mirrorBase := atomic.LoadInt64(&counter.entityCalls)
	var wg sync.WaitGroup
	const n = 8
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() { defer wg.Done(); mc.GetEntityByName(ctx, o3aTenantA, "leads") }()
	}
	// Only ONE goroutine becomes the single-flight leader and reaches the barrier; the rest wait
	// on its in-flight call. Drain its started signal, then release.
	<-started2
	close(hold2)
	wg.Wait()
	if got := atomic.LoadInt64(&counter.entityCalls) - mirrorBase; got != 1 {
		t.Fatalf("same-key concurrent loader ran %d times; want 1 (single-flight coalesce)", got)
	}
}

// -----------------------------------------------------------------------------
// Attack 3: STALE-AFTER-INVALIDATE RACE — invalidate mid-load, next read reloads.
// -----------------------------------------------------------------------------

func TestO3aWired_InvalidateMidLoadReloads(t *testing.T) {
	counter, mc, entitySvc, _, _ := o3aWire(t, DefaultMetaTTL)
	ctx := context.Background()

	if _, _, err := entitySvc.CreateEntity(ctx, o3aTenantA, CreateEntityInput{
		Name: "deals", Label: "Deals v1",
		Fields: []FieldInput{{Name: "f1", FieldType: "text"}},
	}); err != nil {
		t.Fatalf("create deals: %v", err)
	}

	hold := make(chan struct{})
	started := make(chan struct{}, 1)
	counter.setBarrier(hold, started)
	base := atomic.LoadInt64(&counter.entityCalls)

	done := make(chan *metadata.EntityDefinition, 1)
	go func() { d, _ := mc.GetEntityByName(ctx, o3aTenantA, "deals"); done <- d }()

	<-started                                   // load is in flight
	mc.InvalidateEntityDef(o3aTenantA, "deals") // invalidate mid-flight (generation bump)
	close(hold)                                 // let the load finish
	<-done

	counter.setBarrier(nil, nil) // disarm barrier

	// The mid-flight-invalidated result must NOT have been cached -> next read reloads.
	if _, err := mc.GetEntityByName(ctx, o3aTenantA, "deals"); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := atomic.LoadInt64(&counter.entityCalls) - base; got != 2 {
		t.Fatalf("entity loader ran %d times; want 2 (mid-flight invalidate must not cache stale)", got)
	}
}

// -----------------------------------------------------------------------------
// Attack 4: SOFT-DELETE NOT-FOUND — primed entity must resolve not-found after delete.
// -----------------------------------------------------------------------------

func TestO3aWired_SoftDeleteResolvesNotFound(t *testing.T) {
	counter, mc, entitySvc, _, recordSvc := o3aWire(t, DefaultMetaTTL)
	ctx := context.Background()

	ent, _, err := entitySvc.CreateEntity(ctx, o3aTenantA, CreateEntityInput{
		Name: "cases", Label: "Cases",
		Fields: []FieldInput{{Name: "f1", FieldType: "text"}},
	})
	if err != nil {
		t.Fatalf("create cases: %v", err)
	}

	// PRIME the cache: resolve entity + fields as A so there IS a hot entry to go stale.
	primed, _ := mc.GetEntityByName(ctx, o3aTenantA, "cases")
	if _, err := mc.ListFields(ctx, o3aTenantA, primed.ID); err != nil {
		t.Fatalf("prime fields: %v", err)
	}
	// Confirm it's hot (repeat read = 0 extra hits).
	hot := atomic.LoadInt64(&counter.entityCalls)
	mc.GetEntityByName(ctx, o3aTenantA, "cases")
	if got := atomic.LoadInt64(&counter.entityCalls); got != hot {
		t.Fatalf("expected cached read; entity DB hits %d -> %d", hot, got)
	}
	_ = ent

	// Soft-delete the entity (invalidates entity def + fields after commit).
	if err := entitySvc.SoftDeleteEntity(ctx, o3aTenantA, "cases"); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	// Cached resolve MUST now see the entity gone (ErrEntityNotFound) — no stale grant.
	if _, err := mc.GetEntityByName(ctx, o3aTenantA, "cases"); !errors.Is(err, metadata.ErrEntityNotFound) {
		t.Fatalf("after soft-delete, GetEntityByName = %v; want ErrEntityNotFound (stale cache!)", err)
	}
	// End-to-end via RecordService: a record op on the deleted entity returns ErrEntityNotFound.
	_, err = recordSvc.Create(ctx, RecordScope{UserID: "u-a", IsAdmin: true}, o3aTenantA, "u-a", "cases", map[string]any{"f1": "x"})
	if !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("CreateRecord on soft-deleted entity = %v; want ErrEntityNotFound", err)
	}

	// And the not-found itself is NOT cached: re-creating the entity is seen immediately.
	if _, _, err := entitySvc.CreateEntity(ctx, o3aTenantA, CreateEntityInput{
		Name: "cases", Label: "Cases v2",
		Fields: []FieldInput{{Name: "f1", FieldType: "text"}},
	}); err != nil {
		t.Fatalf("recreate cases: %v", err)
	}
	again, err := mc.GetEntityByName(ctx, o3aTenantA, "cases")
	if err != nil {
		t.Fatalf("resolve recreated cases = %v; want it found (not-found must not be cached)", err)
	}
	if again.Label != "Cases v2" {
		t.Fatalf("resolve recreated label=%q; want Cases v2", again.Label)
	}
}

// hasField / fieldNames are tiny helpers for field-set assertions.
func hasField(fs []metadata.FieldDefinition, name string) bool {
	for _, f := range fs {
		if f.Name == name {
			return true
		}
	}
	return false
}
func fieldNames(fs []metadata.FieldDefinition) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return out
}
