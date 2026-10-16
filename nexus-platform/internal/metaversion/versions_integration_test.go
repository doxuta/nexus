//go:build integration

package metaversion

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

func uptr(n uint) *uint { return &n }

func insertVersion(t *testing.T, db *sqlx.DB, repo *VersionRepo, tenant string, no *uint, kind string, covered []string) Version {
	t.Helper()
	ctx := context.Background()
	seq, err := repo.NextSeq(ctx, db, tenant)
	if err != nil {
		t.Fatal(err)
	}
	v := Version{TenantID: tenant, Seq: seq, VersionNo: no, Kind: kind, CreatedBy: "u", CreatedByLabel: "U"}
	if covered != nil {
		v.CoveredTypes, _ = json.Marshal(covered)
	}
	if err := repo.InsertVersion(ctx, db, &v); err != nil {
		t.Fatalf("InsertVersion: %v", err)
	}
	return v
}

func TestVersionRepo_SeqAndVersionNo(t *testing.T) {
	db := mvDB(t)
	repo := NewVersionRepo(db)
	ctx := context.Background()
	tenant := newTenant(t, db)

	if s, _ := repo.NextSeq(ctx, db, tenant); s != 1 {
		t.Fatalf("first seq = %d", s)
	}
	if n, _ := repo.NextVersionNo(ctx, db, tenant); n != 1 {
		t.Fatalf("first version_no = %d", n)
	}
	if ok, _ := repo.HasNumbered(ctx, tenant); ok {
		t.Fatal("empty tenant reported delivered")
	}
	insertVersion(t, db, repo, tenant, uptr(1), KindBaseline, []string{"workflow"})
	// a direct entry takes a seq but no version number: not a delivery on its own.
	insertVersion(t, db, repo, tenant, nil, KindDirect, nil)
	insertVersion(t, db, repo, tenant, uptr(2), KindPublish, nil)
	if s, _ := repo.NextSeq(ctx, db, tenant); s != 4 {
		t.Errorf("seq = %d, want 4 (direct entries count)", s)
	}
	if n, _ := repo.NextVersionNo(ctx, db, tenant); n != 3 {
		t.Errorf("version_no = %d, want 3 (direct entries have none)", n)
	}
	head, err := repo.HeadVersionRead(ctx, tenant)
	if err != nil || head == nil || head.VersionNo == nil || *head.VersionNo != 2 {
		t.Fatalf("HeadVersion = %+v, %v", head, err)
	}

	// UNIQUE (tenant_id, version_no) is the second net behind the row lock.
	dup := Version{TenantID: tenant, Seq: 99, VersionNo: uptr(2), Kind: KindPublish, CreatedBy: "u"}
	if err := repo.InsertVersion(ctx, db, &dup); err == nil {
		t.Fatal("duplicate version_no accepted")
	}
	dupSeq := Version{TenantID: tenant, Seq: 1, VersionNo: uptr(9), Kind: KindPublish, CreatedBy: "u"}
	if err := repo.InsertVersion(ctx, db, &dupSeq); err == nil {
		t.Fatal("duplicate seq accepted")
	}
}

func TestVersionRepo_HasNumberedOnlyDirectIsNotDelivered(t *testing.T) {
	db := mvDB(t)
	repo := NewVersionRepo(db)
	tenant := newTenant(t, db)
	insertVersion(t, db, repo, tenant, nil, KindDirect, nil)
	if ok, err := repo.HasNumbered(context.Background(), tenant); err != nil || ok {
		t.Fatalf("a tenant with only a direct entry is delivered: %v %v", ok, err)
	}
	other := newTenant(t, db)
	insertVersion(t, db, repo, other, uptr(1), KindBaseline, nil)
	if ok, _ := repo.HasNumbered(context.Background(), tenant); ok {
		t.Fatal("another tenant's baseline made this tenant delivered")
	}
}

// A type with zero documents at snapshot time still counts as covered.
func TestVersionRepo_CoveredTypesUpTo(t *testing.T) {
	db := mvDB(t)
	repo := NewVersionRepo(db)
	ctx := context.Background()
	tenant := newTenant(t, db)
	v1 := insertVersion(t, db, repo, tenant, uptr(1), KindBaseline, []string{"workflow", "report"})
	v2 := insertVersion(t, db, repo, tenant, uptr(2), KindPublish, []string{"dashboard"})
	insertVersion(t, db, repo, tenant, uptr(3), KindPublish, nil)

	got, err := repo.CoveredTypesUpTo(ctx, db, tenant, v1.Seq)
	if err != nil || !reflect.DeepEqual(got, []string{"report", "workflow"}) {
		t.Errorf("up to v1 = %v, %v", got, err)
	}
	got, _ = repo.CoveredTypesUpTo(ctx, db, tenant, v2.Seq)
	if !reflect.DeepEqual(got, []string{"dashboard", "report", "workflow"}) {
		t.Errorf("up to v2 = %v", got)
	}
	if got, _ := repo.CoveredTypesUpTo(ctx, db, newTenant(t, db), 99); len(got) != 0 {
		t.Errorf("another tenant's covered types leaked: %v", got)
	}
}

func TestVersionRepo_InsertDocAndActorLabel(t *testing.T) {
	db := mvDB(t)
	repo := NewVersionRepo(db)
	ctx := context.Background()
	tenant := newTenant(t, db)
	v := insertVersion(t, db, repo, tenant, uptr(1), KindBaseline, []string{"workflow"})
	if err := repo.InsertDoc(ctx, db, VersionDoc{TenantID: tenant, VersionID: v.ID, Seq: v.Seq, DocType: "workflow", DocKey: "wf-1", Label: "L", Op: "snapshot",
		AfterJSON: json.RawMessage(`{"a":1}`), CreatedByID: "u"}); err != nil {
		t.Fatalf("InsertDoc: %v", err)
	}
	var before, after *string
	if err := db.QueryRow(`SELECT before_json, after_json FROM sys_meta_version_docs WHERE tenant_id = ? AND version_id = ?`, tenant, v.ID).Scan(&before, &after); err != nil {
		t.Fatal(err)
	}
	if before != nil || after == nil || !strings.Contains(*after, `"a"`) {
		t.Errorf("before/after = %v / %v", before, after)
	}

	// ActorLabel reads the caller's OWN row in their home tenant; another tenant's user is invisible.
	uid := uuid.NewString()
	if _, err := db.Exec(`INSERT INTO users (id, tenant_id, email, password_hash, full_name, status) VALUES (?, ?, ?, 'x', ?, 'active')`,
		uid, tenant, uid[:8]+"@t.vn", "Nguyen Builder"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if got := repo.ActorLabel(ctx, db, tenant, uid); got != "Nguyen Builder" {
		t.Errorf("ActorLabel = %q", got)
	}
	if got := repo.ActorLabel(ctx, db, newTenant(t, db), uid); got != "" {
		t.Errorf("ActorLabel crossed tenants: %q", got)
	}
	if _, err := db.Exec(`UPDATE users SET full_name = '' WHERE id = ?`, uid); err != nil {
		t.Fatal(err)
	}
	if got := repo.ActorLabel(ctx, db, tenant, uid); !strings.HasSuffix(got, "@t.vn") {
		t.Errorf("ActorLabel without a name should fall back to the email, got %q", got)
	}
}

// T008: DeliveredAt = MIN(created_at) of the numbered versions, skips tenants not delivered and
// never reads a tenant outside the list.
func TestDeliveryRepo_DeliveredAt(t *testing.T) {
	db := mvDB(t)
	vr, dr := NewVersionRepo(db), NewDeliveryRepo(db)
	ctx := context.Background()
	t1, t2, t3 := newTenant(t, db), newTenant(t, db), newTenant(t, db)
	insertVersion(t, db, vr, t1, uptr(1), KindBaseline, nil)
	time.Sleep(20 * time.Millisecond)
	insertVersion(t, db, vr, t1, uptr(2), KindPublish, nil)
	insertVersion(t, db, vr, t2, nil, KindDirect, nil) // only a direct entry: not delivered
	insertVersion(t, db, vr, t3, uptr(1), KindBaseline, nil)

	var first time.Time
	if err := db.Get(&first, `SELECT MIN(created_at) FROM sys_meta_versions WHERE tenant_id = ? AND version_no IS NOT NULL`, t1); err != nil {
		t.Fatal(err)
	}
	got, err := dr.DeliveredAt(ctx, []string{t1, t2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[t1].Equal(first) {
		t.Fatalf("DeliveredAt = %v, want only t1 at %v (t2 not delivered, t3 not asked for)", got, first)
	}
	if got, err := dr.DeliveredAt(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("empty list: %v %v", got, err)
	}
}
