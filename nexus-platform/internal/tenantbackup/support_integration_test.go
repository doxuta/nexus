//go:build integration

package tenantbackup

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/storage"
)

// sentinel prefixes every planted secret: after a backup, NO decompressed entry of the zip may
// contain it. Compressed bytes are useless for this search — every entry is inflated first.
const sentinel = "SENTINEL_"

type cust struct{ ID, Slug, Name string }

type env struct {
	t          *testing.T
	db         *sqlx.DB
	gdb        *gorm.DB
	store      *GormStore
	svc        *Service
	runner     *Runner
	blobs      *storage.LocalBackend
	backups    *storage.LocalBackend
	blobsDir   string
	backupsDir string
	aud        *recAudit
	A, B, W    cust
	opUser     string
	uA, uB     string
	objTable   string
	objA       []string // ids of A's rows in the shared obj table (the last one is soft-deleted)
	objB       []string
}

// newEnv builds a Service over the throwaway database with REAL backends on temp dirs and seeds
// customers A and B (plus the workshop W the operator stands in). wrap, when set, decorates the
// snapshot source so a test can hook the moment a backup starts.
func newEnv(t *testing.T, wrap func(SnapshotSource) SnapshotSource) *env {
	t.Helper()
	db := itDB(t)
	gdb, err := gorm.Open(gormmysql.Open(packageTestDSN), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	e := &env{t: t, db: db, gdb: gdb, aud: &recAudit{}}
	e.blobsDir, e.backupsDir = t.TempDir(), t.TempDir()
	if e.blobs, err = storage.NewLocalBackend(e.blobsDir); err != nil {
		t.Fatal(err)
	}
	if e.backups, err = storage.NewLocalBackend(e.backupsDir); err != nil {
		t.Fatal(err)
	}
	e.store = NewGormStore(gdb, db, nil)
	var snaps SnapshotSource = NewSqlxSnapshots(db)
	if wrap != nil {
		snaps = wrap(snaps)
	}
	e.runner = NewRunner(nil)
	e.svc = NewService(e.store, snaps, e.blobs, e.backups, e.runner, e.aud, Config{Dir: e.backupsDir, MinFreeBytes: 1}, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = e.runner.Stop(ctx)
	})

	sfx := randSuffix(t)
	e.A = cust{uuid.NewString(), "tbk-a-" + sfx, "Khách A " + sfx}
	e.B = cust{uuid.NewString(), "tbk-b-" + sfx, "Khách B " + sfx}
	e.W = cust{uuid.NewString(), "tbk-w-" + sfx, "Nexus " + sfx}
	e.opUser = uuid.NewString()
	e.uA, e.uB = uuid.NewString(), uuid.NewString()
	for _, c := range []cust{e.A, e.B, e.W} {
		e.exec("INSERT INTO tenants (id, slug, name, status, branding) VALUES (?, ?, ?, 'active', ?)",
			c.ID, c.Slug, c.Name, `{"appName":"`+c.Slug+`"}`)
	}
	e.exec("INSERT INTO users (id, tenant_id, email, password_hash, full_name, status, is_admin, is_platform_operator) VALUES (?, ?, ?, ?, ?, 'active', 1, 1)",
		e.opUser, e.W.ID, "op-"+sfx+"@northwind.test", sentinel+"OP_PW", "Nguyễn Văn A")

	// One obj_ table shared by A and B, with a unique field so a generated column exists.
	create, err := ddl.BuildCreateTable("tbk_it_"+sfx, []ddl.FieldDef{{Name: "code", FieldType: "text", IsUnique: true}})
	if err != nil {
		t.Fatal(err)
	}
	e.objTable = "obj_tbk_it_" + sfx
	e.exec(create)
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE IF EXISTS `" + e.objTable + "`") })
	for i, owner := range []struct {
		t    cust
		del  bool
		code string
	}{{e.A, false, "A1"}, {e.A, false, "A2"}, {e.A, true, "A3"}, {e.B, false, "B1"}, {e.B, false, "B2"}} {
		id := uuid.NewString()
		var deleted any
		if owner.del {
			deleted = "2026-02-02 02:02:02"
		}
		e.exec("INSERT INTO `"+e.objTable+"` (id, tenant_id, created_by, data, deleted_at) VALUES (?, ?, ?, ?, ?)",
			id, owner.t.ID, e.opUser, fmt.Sprintf(`{"code":"%s","note":"dòng %d \"có\" xuống\ndòng 😀"}`, owner.code+"-"+sfx, i), deleted)
		if owner.t.ID == e.A.ID {
			e.objA = append(e.objA, id)
		} else {
			e.objB = append(e.objB, id)
		}
	}

	e.seedAccess(e.A, e.uA, "a")
	e.seedAccess(e.B, e.uB, "b")
	return e
}

func (e *env) exec(q string, args ...any) {
	e.t.Helper()
	if _, err := e.db.Exec(q, args...); err != nil {
		e.t.Fatalf("exec %.80s: %v", q, err)
	}
}

// seedAccess plants one customer's identity, access and secret rows. Every secret carries the
// sentinel; the rows that ARE meant to be in the file carry none.
func (e *env) seedAccess(c cust, uid, tag string) {
	t := e.t
	role, team, tset := uuid.NewString(), uuid.NewString(), uuid.NewString()
	e.exec("INSERT INTO acl_roles (id, tenant_id, name, label) VALUES (?, ?, ?, ?)", role, c.ID, "staff", "Nhân viên")
	for _, ent := range []string{"accounts", "contacts"} {
		e.exec("INSERT INTO acl_role_actions (id, role_id, entity_name, action, access_level) VALUES (?, ?, ?, 'view', 'all')",
			uuid.NewString(), role, ent+"_"+tag)
	}
	e.exec("INSERT INTO users (id, tenant_id, email, password_hash, full_name, status, is_admin, role_id) VALUES (?, ?, ?, ?, ?, 'active', 1, ?)",
		uid, c.ID, "u-"+tag+"@khach.test", sentinel+"PWHASH_"+tag, "Người dùng "+tag, role)
	e.exec("INSERT INTO teams (id, tenant_id, name) VALUES (?, ?, ?)", team, c.ID, "Đội "+tag)
	e.exec("INSERT INTO team_sets (id, tenant_id, team_md5) VALUES (?, ?, ?)", tset, c.ID, strings.Repeat(tag, 32)[:32])
	e.exec("INSERT INTO team_memberships (id, user_id, team_id) VALUES (?, ?, ?)", uuid.NewString(), uid, team)
	e.exec("INSERT INTO team_sets_teams (team_set_id, team_id) VALUES (?, ?)", tset, team)

	// --- secrets that must never reach the file --------------------------------------------
	e.exec("INSERT INTO user_mfa (id, tenant_id, user_id, secret_enc) VALUES (?, ?, ?, ?)", uuid.NewString(), c.ID, uid, sentinel+"MFA_"+tag)
	e.exec("INSERT INTO refresh_tokens (id, user_id, tenant_id, token_hash, expires_at) VALUES (?, ?, ?, ?, NOW() + INTERVAL 1 DAY)",
		uuid.NewString(), uid, c.ID, (sentinel + "REFRESH_" + tag + strings.ReplaceAll(c.ID, "-", "") + strings.Repeat("0", 64))[:64])
	e.exec("INSERT INTO sys_api_keys (id, tenant_id, role_id, name, key_prefix, key_hash) VALUES (?, ?, ?, ?, ?, ?)",
		uuid.NewString(), c.ID, role, sentinel+"KEYNAME_"+tag, "nxk_"+tag, (sentinel + "APIKEY_" + tag + strings.ReplaceAll(c.ID, "-", "") + strings.Repeat("0", 64))[:64])
	e.exec("INSERT INTO sys_named_credentials (id, tenant_id, name, label, base_url, auth_type) VALUES (?, ?, ?, ?, ?, 'header')",
		uuid.NewString(), c.ID, "cred-"+tag, sentinel+"CRED_"+tag, "https://p.example/"+sentinel+tag)
	e.exec("INSERT INTO sys_inbound_hooks (id, tenant_id, name, entity_name, secret, mapping, created_by) VALUES (?, ?, ?, 'accounts', ?, '{}', ?)",
		uuid.NewString(), c.ID, "hook-"+tag, sentinel+"HOOK_"+tag, uid)
	e.exec("INSERT INTO sso_providers (id, tenant_id, issuer, client_id, client_secret_enc) VALUES (?, ?, 'https://idp.example', 'cid', ?)",
		uuid.NewString(), c.ID, sentinel+"SSO_"+tag)

	// --- JSON that carries a webhook credential (workflow + the pre-0fb48a8e provenance copy) --
	hook := fmt.Sprintf(`[{"type":"outbound_webhook","url":"https://partner.example/%s","secret":"%sWF_SECRET_%s","headers":{"Authorization":"Bearer %sWF_HDR_%s"},"keepme":"%s-keep"},{"type":"email_alert","to":"x@y.vn"}]`,
		tag, sentinel, tag, sentinel, tag, tag)
	e.exec("INSERT INTO sys_workflows (id, tenant_id, name, entity_name, trigger_event, actions) VALUES (?, ?, ?, 'accounts', 'after_create', ?)",
		uuid.NewString(), c.ID, "wf-"+tag, hook)
	packHook := strings.ReplaceAll(strings.ReplaceAll(hook, "WF_SECRET", "PACK_SECRET"), "WF_HDR", "PACK_HDR")
	e.exec("INSERT INTO sys_pack_items (id, tenant_id, pack_slug, pack_version, item_type, item_key, content) VALUES (?, ?, 'crm', '1.0.0', 'workflow', ?, ?)",
		uuid.NewString(), c.ID, "wf-"+tag, `{"name":"wf","actions":`+packHook+`}`)
	_ = t
}

// putBlob writes bytes under the attachment backend and returns the sys_files id (row inserted).
func (e *env) putFile(c cust, content string, deleted bool, createdAt string) string {
	e.t.Helper()
	id := uuid.NewString()
	if content != "" {
		if _, err := e.blobs.Put(context.Background(), c.ID+"/"+id, strings.NewReader(content)); err != nil {
			e.t.Fatal(err)
		}
	}
	var del any
	if deleted {
		del = "2026-01-01 00:00:00"
	}
	var created any = createdAt
	if createdAt == "" {
		created = time.Now().UTC().Format("2006-01-02 15:04:05.000")
	}
	e.exec(`INSERT INTO sys_files (id, tenant_id, entity_name, record_id, field_name, name, size, mime, path, created_by, sha256, deleted_at, created_at)
		VALUES (?, ?, 'accounts', 'r1', 'doc', 'a.txt', ?, 'text/plain', ?, ?, ?, ?, ?)`,
		id, c.ID, len(content), c.ID+"/"+id, e.opUser, sha(content), del, created)
	return id
}

// actor is the operator standing in the workshop.
func (e *env) actor() Actor { return Actor{TenantID: e.W.ID, UserID: e.opUser} }

// create starts a backup of c and waits until it leaves "running".
func (e *env) create(c cust) *Backup {
	e.t.Helper()
	v, err := e.svc.Create(context.Background(), CreateInput{TenantID: c.ID, Note: "it", Actor: e.actor()})
	if err != nil {
		e.t.Fatalf("Create: %v", err)
	}
	return e.waitDone(c, v.ID)
}

func (e *env) waitDone(c cust, id string) *Backup {
	e.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		b, err := e.store.Get(context.Background(), c.ID, id)
		if err == nil && b.Status != StatusRunning {
			return b
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("backup %s still running after 60s", id)
	return nil
}

// zipBytes returns the stored zip of a backup row.
func (e *env) zipBytes(b *Backup) []byte {
	e.t.Helper()
	if b.StorageKey == nil {
		e.t.Fatal("no storage key")
	}
	rc, err := e.backups.Open(context.Background(), *b.StorageKey)
	if err != nil {
		e.t.Fatalf("open zip: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		e.t.Fatal(err)
	}
	return body
}

// entries inflates every entry of a zip into name -> content.
func entries(t *testing.T, body []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("inflate %s: %v", f.Name, err)
		}
		out[f.Name] = string(b)
	}
	return out
}

// gatedSnaps blocks every Open until released — to hold a backup in "running" deterministically.
type gatedSnaps struct {
	inner   SnapshotSource
	release chan struct{}
	opened  chan struct{}
	once    sync.Once
	after   func(Snapshot) // called right after the real Open (the snapshot is established)
}

func newGated(inner SnapshotSource) *gatedSnaps {
	return &gatedSnaps{inner: inner, release: make(chan struct{}), opened: make(chan struct{})}
}

func (g *gatedSnaps) Open(ctx context.Context) (Snapshot, error) {
	g.once.Do(func() { close(g.opened) })
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s, err := g.inner.Open(ctx)
	if err == nil && g.after != nil {
		g.after(s)
	}
	return s, err
}
