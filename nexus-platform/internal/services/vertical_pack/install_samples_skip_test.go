package vertical_pack

// Unit tests for Pass 3's skip-on-duplicate contract over in-memory fakes: a sample row whose
// unique value already exists (RECORD_DUPLICATE from the record service — the re-install-on-a-
// populated-tenant case) is SKIPPED (SamplesSkipped) instead of aborting the install; rows and
// N-N links that reference the skipped row by "@ref" cascade-skip (their real ids are unknown);
// every OTHER record error keeps the abort-and-rollback behavior.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// fakeDupRecordWriter is an in-memory recordWriter: a row whose data["name"] is in dup fails with
// the record service's RECORD_DUPLICATE ValidationError; a row whose data["name"] is in boom fails
// with a non-duplicate ValidationError; every other row gets a minted id.
type fakeDupRecordWriter struct {
	mu      sync.Mutex
	seq     int
	dup     map[string]bool
	boom    map[string]bool
	veto    map[string]error // name → the error a tenant before_save rule answers with
	created []map[string]any
}

func (f *fakeDupRecordWriter) Create(_ context.Context, _ services.RecordScope, _, _, _ string, payload map[string]any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, _ := payload["name"].(string)
	if f.dup[name] {
		return "", invalid("RECORD_DUPLICATE", "field %q đã tồn tại với giá trị này", "name")
	}
	if f.boom[name] {
		return "", invalid("RECORD_REQUIRED_MISSING", "field %q là bắt buộc", "name")
	}
	if err := f.veto[name]; err != nil {
		return "", err
	}
	f.seq++
	f.created = append(f.created, payload)
	return fmt.Sprintf("rec-%d", f.seq), nil
}

// fakeSkipLinker records every Link call (both ends already resolved to real ids by install).
type fakeSkipLinker struct {
	mu    sync.Mutex
	calls [][2]string // {sourceID, targetID}
}

func (f *fakeSkipLinker) Link(_ context.Context, _, _, _ string, _ bool, _, sourceID, _, targetID string,
	_ services.TargetChecker, _ services.TargetACLChecker, _ services.RecordFetcher, _ dynrepo.RowScope) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, [2]string{sourceID, targetID})
	return true, nil
}

// skipTestPack: one entity, five sample rows and three links exercising direct-skip, two-level
// cascade-skip via "@ref", and the still-inserted / still-linked happy rows around them.
func skipTestPack() *Pack {
	return &Pack{
		Entities: []PackEntity{
			{Name: "t_items", Label: "Items", Fields: []PackField{
				{Name: "name", Label: "Name", FieldType: "text", IsUnique: true},
			}},
		},
		Samples: []PackSample{
			{Entity: "t_items", Records: []PackRecord{
				{Ref: "@a", Data: map[string]any{"name": "dup-a"}},             // duplicate → skip
				{Ref: "@b", Data: map[string]any{"name": "b"}},                 // inserted
				{Ref: "@c", Data: map[string]any{"name": "c", "parent": "@a"}}, // refs skipped @a → cascade-skip
				{Ref: "@d", Data: map[string]any{"name": "d", "parent": "@c"}}, // refs skipped @c → cascades further
				{Ref: "@e", Data: map[string]any{"name": "e", "parent": "@b"}}, // refs inserted @b → inserted
			}},
		},
		Links: []PackLink{
			{Relation: "r", FromRef: "@a", ToRef: "@b"}, // from-end skipped → link skipped
			{Relation: "r", FromRef: "@b", ToRef: "@c"}, // to-end skipped → link skipped
			{Relation: "r", FromRef: "@b", ToRef: "@e"}, // both inserted → link created
		},
	}
}

func newSkipTestService(rw *fakeDupRecordWriter, lk *fakeSkipLinker) (*Service, *fakeMintMeta) {
	meta := &fakeMintMeta{}
	d := Deps{
		EntityReader: meta,
		RoleReader:   fakeMintRoles{},
		EntityWriter: meta,
		RecordWriter: rw,
	}
	if lk != nil {
		d.Linker = lk // avoid a typed-nil interface tripping the s.lnk != nil guard
	}
	return NewService(d), meta
}

// TestInstall_SampleDuplicateSkipsRowAndCascades: the duplicate row and its "@ref" dependents are
// skipped (counted), everything else — including the links between INSERTED rows — proceeds, and
// the install returns success (it reaches the passes after seeding).
func TestInstall_SampleDuplicateSkipsRowAndCascades(t *testing.T) {
	rw := &fakeDupRecordWriter{dup: map[string]bool{"dup-a": true}}
	lk := &fakeSkipLinker{}
	svc, _ := newSkipTestService(rw, lk)

	res, err := svc.Install(context.Background(), "tenant-1", "user-1", skipTestPack())
	if err != nil {
		t.Fatalf("Install: %v (a duplicate sample must not abort the install)", err)
	}
	if res.SamplesInserted != 2 {
		t.Errorf("SamplesInserted = %d, want 2 (@b, @e)", res.SamplesInserted)
	}
	if res.SamplesSkipped != 3 {
		t.Errorf("SamplesSkipped = %d, want 3 (@a dup, @c + @d cascade)", res.SamplesSkipped)
	}
	if res.LinksCreated != 1 || res.LinksSkipped != 2 {
		t.Errorf("LinksCreated/LinksSkipped = %d/%d, want 1/2", res.LinksCreated, res.LinksSkipped)
	}
	if len(lk.calls) != 1 {
		t.Fatalf("linker called %d times, want 1 (@b→@e only)", len(lk.calls))
	}
	// The one surviving link runs on the REAL minted ids of @b and @e (rec-1, rec-2).
	if lk.calls[0] != [2]string{"rec-1", "rec-2"} {
		t.Errorf("link ids = %v, want [rec-1 rec-2]", lk.calls[0])
	}
	// @e's "@b" ref resolved to @b's minted id, not the literal "@b".
	if got, _ := rw.created[1]["parent"].(string); got != "rec-1" {
		t.Errorf("@e.parent = %q, want resolved id rec-1", got)
	}
}

// TestInstall_SampleNonDuplicateErrorStillAborts: only RECORD_DUPLICATE is a skip — any other
// record ValidationError keeps the abort + rollback behavior (the entity this install created is
// soft-deleted again).
func TestInstall_SampleNonDuplicateErrorStillAborts(t *testing.T) {
	rw := &fakeDupRecordWriter{boom: map[string]bool{"dup-a": true}}
	svc, meta := newSkipTestService(rw, &fakeSkipLinker{})

	_, err := svc.Install(context.Background(), "tenant-1", "user-1", skipTestPack())
	if err == nil {
		t.Fatal("Install: want error for a non-duplicate sample failure, got nil")
	}
	var ve *services.ValidationError
	if !errors.As(err, &ve) || ve.Code != "RECORD_REQUIRED_MISSING" {
		t.Fatalf("Install error = %v, want the underlying RECORD_REQUIRED_MISSING ValidationError", err)
	}
	// Rollback: the entity created by THIS install was soft-deleted again.
	defs, _ := meta.ListEntities("tenant-1")
	if len(defs) != 0 {
		t.Errorf("after abort, tenant still has %d entities, want 0 (rollback)", len(defs))
	}
}

// hookVeto is what RecordService.Create answers when a before_save rule refuses a row.
func hookVeto(msg string) error {
	return &services.ValidationError{Code: "RECORD_HOOK_VETO", Msg: msg, Field: "name",
		Details: map[string]any{"violations": []workflow.Violation{{RuleID: "r1", Field: "name", Message: msg}}}}
}

// Queue #253: re-installing into a tenant that already runs a before_save rule (the pack's own,
// switched on since S2) must not abort on a sample row that rule refuses. Like RECORD_DUPLICATE the
// row is skipped and counted, rows and links that reference it cascade-skip, and the install tells
// the installer which rule's words stopped how many rows. Everything else in the pack still lands.
func TestInstall_SampleVetoSkipsRowCascadesAndWarns(t *testing.T) {
	rw := &fakeDupRecordWriter{veto: map[string]error{"dup-a": hookVeto("Tên mẫu không hợp lệ")}}
	lk := &fakeSkipLinker{}
	svc, meta := newSkipTestService(rw, lk)

	res, err := svc.Install(context.Background(), "tenant-1", "user-1", skipTestPack())
	if err != nil {
		t.Fatalf("Install: %v (a sample row a before rule refuses must not cancel the install)", err)
	}
	if res.SamplesInserted != 2 || res.SamplesSkipped != 3 {
		t.Errorf("SamplesInserted/Skipped = %d/%d, want 2/3 (@a vetoed, @c + @d cascade)", res.SamplesInserted, res.SamplesSkipped)
	}
	if res.LinksCreated != 1 || res.LinksSkipped != 2 {
		t.Errorf("LinksCreated/LinksSkipped = %d/%d, want 1/2", res.LinksCreated, res.LinksSkipped)
	}
	if defs, _ := meta.ListEntities("tenant-1"); len(defs) != 1 {
		t.Errorf("tenant has %d entities, want 1 (no rollback)", len(defs))
	}
	var warned bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "Tên mẫu không hợp lệ") && strings.Contains(w, "t_items") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("warnings = %q, want one naming the entity and the refusing rule's message", res.Warnings)
	}
}

// Only a REFUSAL is a skip. "Rules unreadable" (RECORD_HOOKS_UNAVAILABLE) means nobody knows whether
// the row is allowed, so the install keeps failing closed and rolls back.
func TestInstall_SampleHooksUnavailableStillAborts(t *testing.T) {
	rw := &fakeDupRecordWriter{veto: map[string]error{"dup-a": fmt.Errorf("services.RecordService.loadBeforeRules: %w", services.ErrRecordHooksUnavailable)}}
	svc, meta := newSkipTestService(rw, &fakeSkipLinker{})

	_, err := svc.Install(context.Background(), "tenant-1", "user-1", skipTestPack())
	if !errors.Is(err, services.ErrRecordHooksUnavailable) {
		t.Fatalf("Install error = %v, want ErrRecordHooksUnavailable", err)
	}
	if defs, _ := meta.ListEntities("tenant-1"); len(defs) != 0 {
		t.Errorf("after abort, tenant still has %d entities, want 0 (rollback)", len(defs))
	}
}
