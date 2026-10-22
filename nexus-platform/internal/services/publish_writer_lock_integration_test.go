//go:build integration

package services

// publish_writer_lock_integration_test.go — issue #13, forced interleavings on a real MySQL.
//
// (1) A schema write in flight while a publish runs: its INSERT is done and stamped but not committed. The publish
// must not cut off past it unread. Either the publish reads it (and, not having been shown it, refuses), or it
// stays hidden and pending; never released unrecorded, never half released.
// (2) A publish never sleeps holding the tenant lock: the lock's holder is watched in performance_schema while a
// definition stamped ahead of the clock makes the publish wait out a second.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"nexus/platform/internal/metadata"
)

type wlRow struct{ table, id string }

// wlWrite is one schema writer of the metadata repository, with the definitions it inserts.
type wlWrite struct {
	name    string
	docType string     // the row the change list names
	key     string     // its id
	rows    []wlRow    // every definition the write inserts
	stamp   *time.Time // created_at of the first INSERT, set by the repository's hook
	write   func(ctx context.Context, repo *metadata.Repository) error
}

// wlWrites: one write per repository path that stamps created_at (hk_owners must exist for the relations).
func (s *s78Env) wlWrites(t *testing.T) []wlWrite {
	t.Helper()
	deals := s78EntityID(t, s)
	sfx := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	tbl := &metadata.EntityDefinition{ID: uuid.NewString(), TenantID: s.tenant, Name: "wl_t" + sfx, Label: "wl", DataTable: "obj_wl_t" + sfx}
	tf := metadata.FieldDefinition{ID: uuid.NewString(), EntityID: tbl.ID, TenantID: s.tenant, Name: "title", Label: "Title", FieldType: "text"}
	f := &metadata.FieldDefinition{ID: uuid.NewString(), EntityID: deals, TenantID: s.tenant, Name: "wl_f" + sfx, Label: "wl", FieldType: "text"}
	rf := &metadata.FieldDefinition{ID: uuid.NewString(), EntityID: deals, TenantID: s.tenant, Name: "wl_o" + sfx, Label: "owner", FieldType: "relation"}
	r1 := &metadata.RelationshipDefinition{ID: uuid.NewString(), TenantID: s.tenant, Name: "hk_deals_wl_o" + sfx, ParentEntityName: "hk_owners",
		ChildEntityName: "hk_deals", RelationshipType: "1-N", ChildFKColumn: "wl_o" + sfx + "_id"}
	jt := "rel_wl_" + sfx
	nn := &metadata.RelationshipDefinition{ID: uuid.NewString(), TenantID: s.tenant, Name: "wl_nn" + sfx, ParentEntityName: "hk_deals",
		ChildEntityName: "hk_owners", RelationshipType: "N-N", JoinTableName: &jt}
	return []wlWrite{
		{name: "table with a field", docType: DocTypeEntity, key: tbl.ID, rows: []wlRow{{"sys_entities", tbl.ID}, {"sys_fields", tf.ID}}, stamp: &tbl.CreatedAt,
			write: func(ctx context.Context, r *metadata.Repository) error {
				return r.CreateEntityWithFields(ctx, tbl, []metadata.FieldDefinition{tf}, nil)
			}},
		{name: "field", docType: DocTypeField, key: f.ID, rows: []wlRow{{"sys_fields", f.ID}}, stamp: &f.CreatedAt,
			write: func(ctx context.Context, r *metadata.Repository) error { return r.CreateField(ctx, f) }},
		{name: "relation field", docType: DocTypeField, key: rf.ID, rows: []wlRow{{"sys_fields", rf.ID}, {"sys_relationships", r1.ID}}, stamp: &rf.CreatedAt,
			write: func(ctx context.Context, r *metadata.Repository) error {
				return r.CreateFieldWithRelationship(ctx, rf, r1)
			}},
		{name: "N-N relation", docType: DocTypeRelationship, key: nn.ID, rows: []wlRow{{"sys_relationships", nn.ID}}, stamp: &nn.CreatedAt,
			write: func(ctx context.Context, r *metadata.Repository) error { return r.CreateRelationship(ctx, nn) }},
	}
}

// wlTenant: a fresh delivered tenant in the same database, with an aged schema and a second old table.
func (s *s78Env) wlTenant(t *testing.T) *s78Env {
	t.Helper()
	n := *s
	n.tenant, n.builder = s.newTenant(t), uuid.NewString()
	n.bctx, n.cctx = builderCtxOf(n.tenant, n.builder), legacyCtxOf(n.tenant)
	s.deliver(t, n.tenant, n.builder)
	n.ageSchema(t)
	n.oldTable(t, "hk_owners")
	return &n
}

// wlPausedRepo is a metadata repository whose write stops after its first INSERT, inside its transaction, until
// resume is called: a schema write in flight (a lone Create too: before gorm commits its own transaction).
// inflight is closed when it stops. A test that fails first resumes it at cleanup, before its database goes.
func wlPausedRepo(t *testing.T, s *s78Env) (repo *metadata.Repository, inflight chan struct{}, resume func()) {
	t.Helper()
	sqlDB, err := s.gdb.DB()
	if err != nil {
		t.Fatal(err)
	}
	gdb, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: sqlDB}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	inflight, release := make(chan struct{}), make(chan struct{})
	var once, resumed sync.Once
	resume = func() { resumed.Do(func() { close(release) }) }
	t.Cleanup(resume)
	if err := gdb.Callback().Create().After("gorm:create").Before("gorm:commit_or_rollback_transaction").Register("wl:pause", func(*gorm.DB) {
		once.Do(func() { close(inflight); <-release })
	}); err != nil {
		t.Fatal(err)
	}
	return metadata.NewRepository(gdb), inflight, resume
}

// wlWaitLockOrDone returns once done is closed or a transaction waits for the tenant's row lock.
func (s *s78Env) wlWaitLockOrDone(t *testing.T, done <-chan struct{}, who string) {
	t.Helper()
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		select {
		case <-done:
			return
		default:
		}
		var n int
		if err := s.sdb.Get(&n, `SELECT COUNT(*) FROM performance_schema.data_lock_waits w
			JOIN performance_schema.data_locks l ON l.ENGINE_LOCK_ID = w.REQUESTING_ENGINE_LOCK_ID
			WHERE l.OBJECT_SCHEMA = DATABASE() AND l.OBJECT_NAME = 'tenants' AND l.LOCK_DATA LIKE ?`, "%"+s.tenant+"%"); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
	}
	t.Fatalf("%s neither finished nor waited for the tenant lock", who)
}

// wlJudge: every definition the write inserted lies on one side of the head's cutoff. Released, a version recorded
// the change list's row (a publish read it); hidden, it is pending (the next publish lists it).
func (s *s78Env) wlJudge(t *testing.T, w wlWrite, when string) (released bool) {
	t.Helper()
	head, err := s.vers.HeadVersion(context.Background(), s.sdb, s.tenant)
	if err != nil || head == nil || head.SchemaCutoffAt == nil {
		t.Fatalf("head: %+v %v", head, err)
	}
	n := 0
	for _, r := range w.rows {
		var at time.Time
		if err := s.sdb.Get(&at, `SELECT created_at FROM `+r.table+` WHERE tenant_id = ? AND id = ?`, s.tenant, r.id); err != nil {
			t.Fatalf("%s %s: %v", r.table, r.id, err)
		}
		if !at.After(*head.SchemaCutoffAt) {
			n++
		}
	}
	switch n {
	case len(w.rows):
		if s.count(t, `SELECT COUNT(*) FROM sys_meta_version_docs WHERE tenant_id = ? AND doc_type = ? AND doc_key = ? AND op = 'create'`, s.tenant, w.docType, w.key) == 0 {
			t.Errorf("%s: the %s reached the customer with no version recording it", when, w.name)
		}
		return true
	case 0:
		items, _, err := s.pub.Changes(s.bctx, s.tenant)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.DocType == w.docType && it.DocKey == w.key {
				return false
			}
		}
		t.Errorf("%s: the %s is hidden and not pending: no publish would ever release it", when, w.name)
	default:
		t.Errorf("%s: the %s is half released: %d of its %d definitions at or before the cutoff", when, w.name, n, len(w.rows))
	}
	return false
}

func TestPublishSchemaWriteInFlight_Integration(t *testing.T) {
	root := s78Setup(t)
	for i := range 4 {
		// The write stops after its first INSERT, stamped in second S. A field the builder was shown is born in S
		// too, so a publish that cannot see the write cuts off at S and the write commits after it, released unread.
		t.Run("in flight before the publish locks/"+[]string{"table", "field", "relation field", "N-N"}[i], func(t *testing.T) {
			s := root.wlTenant(t)
			w := s.wlWrites(t)[i]
			repo, inflight, resume := wlPausedRepo(t, s)
			wrote := make(chan struct{})
			var werr error
			go func() { defer close(wrote); werr = w.write(context.Background(), repo) }()
			<-inflight
			s.rawField(t, s78EntityID(t, s), "seen_"+w.key[:8], *w.stamp)
			_, hash, err := s.pub.Changes(s.bctx, s.tenant)
			if err != nil {
				t.Fatal(err)
			}
			published := make(chan struct{})
			var perr error
			go func() {
				defer close(published)
				_, perr = s.pub.Publish(s.bctx, s.tenant, PublishRequest{ChangesetHash: hash})
			}()
			s.wlWaitLockOrDone(t, published, "the publish")
			resume()
			<-wrote
			<-published
			if werr != nil {
				t.Fatalf("write: %v", werr)
			}
			if perr != nil {
				wantCode(t, perr, "DRAFTS_CHANGED") // it read the write it was not shown
			}
			if s.wlJudge(t, w, "after the publish") {
				return
			}
			if _, err := s.publishWithCurrentHash(s.bctx, s.tenant, false); err != nil {
				t.Fatalf("publish of the current list: %v", err)
			}
			if !s.wlJudge(t, w, "after a publish that listed it") {
				t.Errorf("a publish that listed the %s did not release it", w.name)
			}
		})
	}
	for i := range 4 {
		// The publish holds the tenant lock (it has read what it will release) when the write starts.
		t.Run("started while the publish holds the lock/"+[]string{"table", "field", "relation field", "N-N"}[i], func(t *testing.T) {
			s := root.wlTenant(t)
			w := s.wlWrites(t)[i]
			s.rawField(t, s78EntityID(t, s), "seen_"+w.key[:8], time.Now().UTC().Add(-10*time.Second).Truncate(time.Second))
			_, hash, err := s.pub.Changes(s.bctx, s.tenant)
			if err != nil {
				t.Fatal(err)
			}
			wrote := make(chan struct{})
			var werr error
			s.pub.afterPendingRead = func() {
				go func() { defer close(wrote); werr = w.write(context.Background(), s.meta) }()
				s.wlWaitLockOrDone(t, wrote, "the write")
			}
			_, perr := s.pub.Publish(s.bctx, s.tenant, PublishRequest{ChangesetHash: hash})
			s.pub.afterPendingRead = nil
			<-wrote
			if perr != nil || werr != nil {
				t.Fatalf("publish: %v, write: %v", perr, werr)
			}
			if s.wlJudge(t, w, "after the publish") {
				t.Errorf("the publish released the %s born after it read", w.name)
			}
		})
	}
}

// wlLockHeld runs f and returns the longest stretch for which some transaction held the tenant's row lock
// exclusively, as performance_schema shows it every few milliseconds.
func wlLockHeld(t *testing.T, sdb *sqlx.DB, tenant string, f func()) time.Duration {
	t.Helper()
	stop, done := make(chan struct{}), make(chan time.Duration, 1)
	go func() {
		var longest time.Duration
		var since time.Time
		for {
			var n int
			if err := sdb.Get(&n, `SELECT COUNT(*) FROM performance_schema.data_locks WHERE OBJECT_SCHEMA = DATABASE() AND OBJECT_NAME = 'tenants'
				AND LOCK_TYPE = 'RECORD' AND LOCK_MODE LIKE 'X%' AND LOCK_STATUS = 'GRANTED' AND LOCK_DATA LIKE ?`, "%"+tenant+"%"); err != nil {
				t.Errorf("performance_schema.data_locks: %v", err)
			}
			now := time.Now()
			switch {
			case n > 0 && since.IsZero():
				since = now
			case n == 0 && !since.IsZero():
				longest, since = max(longest, now.Sub(since)), time.Time{}
			}
			select {
			case <-stop:
				if !since.IsZero() {
					longest = max(longest, now.Sub(since))
				}
				done <- longest
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	func() { defer close(stop); f() }() // a Fatalf in f still stops the watcher
	return <-done
}

// A definition stamped ahead of this clock (another replica's) makes the version wait out the running second,
// and the wait must not hold the tenant lock: every schema write and draft save of the tenant queues behind it.
func TestPublishNeverSleepsUnderTheTenantLock_Integration(t *testing.T) {
	const most = 700 * time.Millisecond // the old wait alone held it a full second
	t.Run("baseline", func(t *testing.T) {
		s := csSetup(t)
		ahead := s.table(t, time.Now().Add(30*time.Second))
		held := wlLockHeld(t, s.sdb, s.tenant, func() {
			if _, err := s.pub.Baseline(s.bctx, s.tenant, ""); err != nil {
				t.Fatalf("Baseline: %v", err)
			}
		})
		t.Logf("the baseline held the tenant lock for %v", held)
		if held > most {
			t.Errorf("the baseline held the tenant lock for %v", held)
		}
		s.wantUnreleased(t, ahead, "the baseline")
	})
	t.Run("publish", func(t *testing.T) {
		s := s78Setup(t)
		s.ageSchema(t)
		ent := s78EntityID(t, s)
		s.rawField(t, ent, "seen", time.Now().UTC().Add(-10*time.Second).Truncate(time.Second))
		s.rawField(t, ent, "ahead", time.Now().UTC().Add(30*time.Second).Truncate(time.Second))
		held := wlLockHeld(t, s.sdb, s.tenant, func() {
			if _, err := s.publishWithCurrentHash(s.bctx, s.tenant, false); err != nil {
				t.Fatalf("Publish: %v", err)
			}
		})
		t.Logf("the publish held the tenant lock for %v", held)
		if held > most {
			t.Errorf("the publish held the tenant lock for %v", held)
		}
		if !s.formHas(t, s.cctx, "seen") || s.formHas(t, s.cctx, "ahead") {
			t.Errorf("released: seen %v, ahead %v; want only the field born before the publish", s.formHas(t, s.cctx, "seen"), s.formHas(t, s.cctx, "ahead"))
		}
	})
}

// wlStraddle runs write stopped after its first INSERT (stamped in second S) until the next second has begun, then
// runs version while the write still holds its lock, lets the write go, and waits for both. A writer that stamps each
// row as it inserts it puts the later rows in S+1; the version, waiting for the write and taking its cutoff in S+1,
// then releases the rows of S and hides those of S+1 (#59, from the verifier's probe).
func (s *s78Env) wlStraddle(t *testing.T, stamp *time.Time, write func(context.Context, *metadata.Repository) error, version func() error) {
	t.Helper()
	repo, inflight, resume := wlPausedRepo(t, s)
	wrote := make(chan struct{})
	var werr error
	go func() { defer close(wrote); werr = write(context.Background(), repo) }()
	<-inflight
	time.Sleep(time.Until(stamp.Add(time.Second + 20*time.Millisecond))) // S+1 has begun
	ran := make(chan struct{})
	var verr error
	go func() { defer close(ran); verr = version() }()
	s.wlWaitLockOrDone(t, ran, "the version")
	resume()
	<-wrote
	<-ran
	if werr != nil || verr != nil {
		t.Fatalf("write: %v, version: %v", werr, verr)
	}
}

func TestPublishSchemaWriteStraddlingASecond_Integration(t *testing.T) {
	// A relation field added to a table the builder was shown as new folds into that table's row: the change list
	// and its hash do not move, so the publish goes ahead with the write it waited for.
	t.Run("publish: a relation field on a new table", func(t *testing.T) {
		s := s78Setup(t).wlTenant(t)
		xid, sfx := uuid.NewString(), strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
		born := time.Now().UTC().Add(-10 * time.Second).Truncate(time.Second)
		if _, err := s.sdb.Exec(`INSERT INTO sys_entities (id, tenant_id, name, label, table_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			xid, s.tenant, "vx_"+sfx, "vx", "obj_vx_"+sfx, born, born); err != nil {
			t.Fatal(err)
		}
		_, hash, err := s.pub.Changes(s.bctx, s.tenant)
		if err != nil {
			t.Fatal(err)
		}
		rf := &metadata.FieldDefinition{ID: uuid.NewString(), EntityID: xid, TenantID: s.tenant, Name: "vx_owner", Label: "owner", FieldType: "relation"}
		r1 := &metadata.RelationshipDefinition{ID: uuid.NewString(), TenantID: s.tenant, Name: "vx_" + sfx + "_owner", ParentEntityName: "hk_owners",
			ChildEntityName: "vx_" + sfx, RelationshipType: "1-N", ChildFKColumn: "vx_owner_id"}
		s.wlStraddle(t, &rf.CreatedAt, func(ctx context.Context, r *metadata.Repository) error {
			return r.CreateFieldWithRelationship(ctx, rf, r1)
		},
			func() error {
				_, err := s.pub.Publish(s.bctx, s.tenant, PublishRequest{ChangesetHash: hash})
				return err
			})
		w := wlWrite{name: "relation field folded into the new table", docType: DocTypeEntity, key: xid,
			rows: []wlRow{{"sys_entities", xid}, {"sys_fields", rf.ID}, {"sys_relationships", r1.ID}}}
		if !s.wlJudge(t, w, "after the publish") {
			t.Errorf("the publish of the table it was shown did not release it")
		}
	})
	// The baseline has no change list to refuse with: it releases every definition it read.
	t.Run("baseline: a table with its required field", func(t *testing.T) {
		cs := csSetup(t)
		sfx := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
		tbl := &metadata.EntityDefinition{ID: uuid.NewString(), TenantID: cs.tenant, Name: "vb_t" + sfx, Label: "vb", DataTable: "obj_vb_t" + sfx}
		f := metadata.FieldDefinition{ID: uuid.NewString(), EntityID: tbl.ID, TenantID: cs.tenant, Name: "code", Label: "Code", FieldType: "text", IsRequired: true}
		(&s78Env{dpEnv: cs.dpEnv, tenant: cs.tenant}).wlStraddle(t, &tbl.CreatedAt,
			func(ctx context.Context, r *metadata.Repository) error {
				return r.CreateEntityWithFields(ctx, tbl, []metadata.FieldDefinition{f}, nil)
			},
			func() error { _, err := cs.pub.Baseline(cs.bctx, cs.tenant, ""); return err })
		head, err := cs.vers.HeadVersion(context.Background(), cs.sdb, cs.tenant)
		if err != nil || head == nil || head.SchemaCutoffAt == nil {
			t.Fatalf("head: %+v %v", head, err)
		}
		var tAt, fAt time.Time
		if err := cs.sdb.Get(&tAt, `SELECT created_at FROM sys_entities WHERE tenant_id = ? AND id = ?`, cs.tenant, tbl.ID); err != nil {
			t.Fatal(err)
		}
		if err := cs.sdb.Get(&fAt, `SELECT created_at FROM sys_fields WHERE tenant_id = ? AND id = ?`, cs.tenant, f.ID); err != nil {
			t.Fatal(err)
		}
		if tAt.After(*head.SchemaCutoffAt) != fAt.After(*head.SchemaCutoffAt) {
			t.Errorf("the baseline split one CreateEntityWithFields: table %v, its required field %v, cutoff %v", tAt, fAt, *head.SchemaCutoffAt)
		}
		if tAt.After(*head.SchemaCutoffAt) && !cs.pending(t, tbl.ID) {
			t.Error("the table the baseline did not release is not pending")
		}
	})
}
