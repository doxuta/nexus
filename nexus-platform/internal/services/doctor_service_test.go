package services

// Unit tests for DOCTOR-CORE (../specs/008-nexus-doctor/spec.md §4 lát 1): each check's
// detect → fix → re-detect-clean loop over an in-memory fake world, plus the service-level
// contracts (unknown checkId, audit rows per fix, MetaCache invalidation after a fix batch).
// The runner mechanics themselves (truncation, confirm gate, per-item continue) are pinned in
// internal/doctor/doctor_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/ddl"
	"nexus/platform/internal/doctor"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// fakeDoctorWorld implements doctorStore + doctorSchemaRepo + doctorRelRepo +
// doctorLayoutRepo + LayoutMetaReader over in-memory maps, so one fixture drives every check
// end-to-end (a fix mutates the same state the next detect reads — mirroring the real DB
// coupling; for the schema checks the "physical" side is the tables map).
type fakeDoctorWorld struct {
	phantoms map[string]doctor.RelationshipRow // relID → row (B1 candidates)
	orphans  map[string]doctor.RelationshipRow // relID → row (B2 candidates)
	stale    map[string]bool                   // relIDs the LIST reports but the per-item re-detect denies
	layouts  map[string]*metadata.LayoutDefinition
	entities map[string]*metadata.EntityDefinition // name → def
	fields   map[string][]metadata.FieldDefinition // entityID → live fields
	navRows  map[string]int                        // "type:ref" → orphan row count

	// DOCTOR-SCHEMA state: the declared side (physFields/nnRels + entities above) vs the
	// "physical" side (tables — the fake information_schema).
	physFields map[string]doctor.PhysicalFieldRow  // fieldID → row (A1/A2a candidates)
	nnRels     map[string]doctor.NNRelationshipRow // relID → row (B4 candidates)
	tables     map[string]*fakeTableShape          // physical table name → shape
	dupUnique  map[string]bool                     // "entity.field" → Add/Guard returns ErrDuplicateUnique (1062)

	// DOCTOR-OPS state: C4 zombie workflows (wfID → row; a fix flips wfDisabled), B5 orphan
	// rel_* tables and C8 dead relation targets (both report-only — detect input only).
	zombies      map[string]doctor.ZombieWorkflowRow
	wfDisabled   map[string]bool
	orphanRelTbl []doctor.OrphanRelTable
	deadTargets  map[string]doctor.DeadRelationTargetRow

	// D2 (queue #63): the stored currency code per live record — "entity.field" → recordID → code.
	currencyCodes map[string]map[string]string
	gotWantCodes  []string // the configured code each D2 query compared against, in call order
}

// fakeTableShape mirrors doctorTableShape: column → generation expression, index → columns.
type fakeTableShape struct {
	columns map[string]string
	indexes map[string][]string
}

func newFakeTableShape() *fakeTableShape {
	return &fakeTableShape{columns: map[string]string{}, indexes: map[string][]string{}}
}

// fakeGuardedExpr / fakeUnguardedExpr stand in for information_schema GENERATION_EXPRESSION
// values — the A2a predicate only cares whether "deleted_at" is referenced.
const (
	fakeGuardedExpr   = "case when (`deleted_at` is null) then json_unquote(json_extract(`data`,_utf8mb4'$.x')) else NULL end"
	fakeUnguardedExpr = "json_unquote(json_extract(`data`,_utf8mb4'$.x'))"
)

// addMandatoryShape stamps the mandatory columns/indexes onto a fake table (what a fresh
// BuildCreateTable materializes).
func (t *fakeTableShape) addMandatoryShape() {
	for _, c := range ddl.MandatoryColumns {
		t.columns[c.Name] = ""
	}
	for _, ix := range ddl.MandatoryIndexes {
		t.indexes[ix.Name] = append([]string{}, ix.Columns...)
	}
}

func newFakeDoctorWorld() *fakeDoctorWorld {
	return &fakeDoctorWorld{
		phantoms:    map[string]doctor.RelationshipRow{},
		orphans:     map[string]doctor.RelationshipRow{},
		stale:       map[string]bool{},
		layouts:     map[string]*metadata.LayoutDefinition{},
		entities:    map[string]*metadata.EntityDefinition{},
		fields:      map[string][]metadata.FieldDefinition{},
		navRows:     map[string]int{},
		physFields:  map[string]doctor.PhysicalFieldRow{},
		nnRels:      map[string]doctor.NNRelationshipRow{},
		tables:      map[string]*fakeTableShape{},
		dupUnique:   map[string]bool{},
		zombies:     map[string]doctor.ZombieWorkflowRow{},
		wfDisabled:  map[string]bool{},
		deadTargets: map[string]doctor.DeadRelationTargetRow{},
	}
}

func sortedRelRows(m map[string]doctor.RelationshipRow, limit int) []doctor.RelationshipRow {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]doctor.RelationshipRow, 0, len(keys))
	for _, k := range keys {
		if len(out) >= limit {
			break
		}
		out = append(out, m[k])
	}
	return out
}

// --- doctorStore ---

func (w *fakeDoctorWorld) ListPhantomRelationships(_ context.Context, _ string, limit int) ([]doctor.RelationshipRow, error) {
	return sortedRelRows(w.phantoms, limit), nil
}
func (w *fakeDoctorWorld) IsPhantomRelationship(_ context.Context, _ string, relID string) (bool, error) {
	_, ok := w.phantoms[relID]
	return ok && !w.stale[relID], nil
}
func (w *fakeDoctorWorld) ListOrphanRelationRows(_ context.Context, _ string, limit int) ([]doctor.RelationshipRow, error) {
	return sortedRelRows(w.orphans, limit), nil
}
func (w *fakeDoctorWorld) IsOrphanRelationRow(_ context.Context, _ string, relID string) (bool, error) {
	_, ok := w.orphans[relID]
	return ok && !w.stale[relID], nil
}
func (w *fakeDoctorWorld) ListLayoutRows(_ context.Context, _ string, limit int) ([]doctor.LayoutRow, error) {
	keys := make([]string, 0, len(w.layouts))
	for k := range w.layouts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]doctor.LayoutRow, 0, len(keys))
	for _, k := range keys {
		if len(out) >= limit {
			break
		}
		l := w.layouts[k]
		out = append(out, doctor.LayoutRow{ID: l.ID, EntityName: l.EntityName, ViewType: l.ViewType, LayoutJSON: l.LayoutJSON})
	}
	return out, nil
}
func (w *fakeDoctorWorld) ListOrphanNavRefs(_ context.Context, _ string, limit int) ([]doctor.NavRef, error) {
	keys := make([]string, 0, len(w.navRows))
	for k := range w.navRows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]doctor.NavRef, 0, len(keys))
	for _, k := range keys {
		if len(out) >= limit {
			break
		}
		itemType, refID, _ := cutNavKey(k)
		out = append(out, doctor.NavRef{ItemType: itemType, RefID: refID, Rows: w.navRows[k]})
	}
	return out, nil
}
func (w *fakeDoctorWorld) CountOrphanNavRows(_ context.Context, _ string, itemType, refID string) (int, error) {
	return w.navRows[itemType+":"+refID], nil
}
func (w *fakeDoctorWorld) PurgeNavRef(_ context.Context, _ string, itemType, refID string) error {
	delete(w.navRows, itemType+":"+refID)
	return nil
}

func cutNavKey(k string) (string, string, bool) {
	for i := 0; i < len(k); i++ {
		if k[i] == ':' {
			return k[:i], k[i+1:], true
		}
	}
	return k, "", false
}

// --- doctorStore: DOCTOR-SCHEMA detect surface (the fake information_schema) ---

func (w *fakeDoctorWorld) ListPhysicalFieldRows(_ context.Context, _ string, limit int) ([]doctor.PhysicalFieldRow, error) {
	keys := make([]string, 0, len(w.physFields))
	for k := range w.physFields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]doctor.PhysicalFieldRow, 0, len(keys))
	for _, k := range keys {
		if len(out) >= limit {
			break
		}
		out = append(out, w.physFields[k])
	}
	return out, nil
}

func (w *fakeDoctorWorld) GetPhysicalFieldRow(_ context.Context, _ string, fieldID string) (*doctor.PhysicalFieldRow, error) {
	row, ok := w.physFields[fieldID]
	if !ok {
		return nil, nil
	}
	cp := row
	return &cp, nil
}

func (w *fakeDoctorWorld) ListActiveEntities(_ context.Context, _ string, limit int) ([]doctor.EntityRef, error) {
	names := make([]string, 0, len(w.entities))
	for n := range w.entities {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]doctor.EntityRef, 0, len(names))
	for _, n := range names {
		if len(out) >= limit {
			break
		}
		out = append(out, doctor.EntityRef{ID: w.entities[n].ID, Name: n})
	}
	return out, nil
}

func (w *fakeDoctorWorld) ListActiveNNRelationships(_ context.Context, _ string, limit int) ([]doctor.NNRelationshipRow, error) {
	keys := make([]string, 0, len(w.nnRels))
	for k := range w.nnRels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]doctor.NNRelationshipRow, 0, len(keys))
	for _, k := range keys {
		if len(out) >= limit {
			break
		}
		out = append(out, w.nnRels[k])
	}
	return out, nil
}

func (w *fakeDoctorWorld) GetActiveNNRelationship(_ context.Context, _ string, relID string) (*doctor.NNRelationshipRow, error) {
	row, ok := w.nnRels[relID]
	if !ok {
		return nil, nil
	}
	cp := row
	return &cp, nil
}

func (w *fakeDoctorWorld) ListExistingTables(_ context.Context, tables []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, t := range tables {
		if _, ok := w.tables[t]; ok {
			out[t] = true
		}
	}
	return out, nil
}

func (w *fakeDoctorWorld) ListTableColumns(_ context.Context, tables []string) ([]doctor.TableColumn, error) {
	var out []doctor.TableColumn
	for _, t := range tables {
		shape, ok := w.tables[t]
		if !ok {
			continue
		}
		cols := make([]string, 0, len(shape.columns))
		for c := range shape.columns {
			cols = append(cols, c)
		}
		sort.Strings(cols)
		for _, c := range cols {
			out = append(out, doctor.TableColumn{TableName: t, ColumnName: c, GenerationExpression: shape.columns[c]})
		}
	}
	return out, nil
}

func (w *fakeDoctorWorld) ListTableIndexes(_ context.Context, tables []string) ([]doctor.IndexColumn, error) {
	var out []doctor.IndexColumn
	for _, t := range tables {
		shape, ok := w.tables[t]
		if !ok {
			continue
		}
		names := make([]string, 0, len(shape.indexes))
		for ix := range shape.indexes {
			names = append(names, ix)
		}
		sort.Strings(names)
		for _, ix := range names {
			for i, col := range shape.indexes[ix] {
				out = append(out, doctor.IndexColumn{TableName: t, IndexName: ix, SeqInIndex: i + 1, ColumnName: col})
			}
		}
	}
	return out, nil
}

// --- doctorStore: DOCTOR-OPS surface (C4 zombies + report-only A3/B5/C8) ---

// ListPhysicalFieldRowsByEntityNames mirrors the global (all-tenant) A3 ownership query —
// the fake is single-tenant, so it filters physFields by entity name.
func (w *fakeDoctorWorld) ListPhysicalFieldRowsByEntityNames(_ context.Context, names []string, limit int) ([]doctor.PhysicalFieldRow, error) {
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}
	keys := make([]string, 0, len(w.physFields))
	for k := range w.physFields {
		if nameSet[w.physFields[k].EntityName] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]doctor.PhysicalFieldRow, 0, len(keys))
	for _, k := range keys {
		if len(out) >= limit {
			break
		}
		out = append(out, w.physFields[k])
	}
	return out, nil
}

func (w *fakeDoctorWorld) ListZombieWorkflows(_ context.Context, _ string, limit int) ([]doctor.ZombieWorkflowRow, error) {
	keys := make([]string, 0, len(w.zombies))
	for k := range w.zombies {
		if !w.wfDisabled[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]doctor.ZombieWorkflowRow, 0, len(keys))
	for _, k := range keys {
		if len(out) >= limit {
			break
		}
		out = append(out, w.zombies[k])
	}
	return out, nil
}

func (w *fakeDoctorWorld) GetZombieWorkflow(_ context.Context, _ string, wfID string) (*doctor.ZombieWorkflowRow, error) {
	row, ok := w.zombies[wfID]
	if !ok || w.wfDisabled[wfID] || w.stale[wfID] {
		return nil, nil
	}
	cp := row
	return &cp, nil
}

func (w *fakeDoctorWorld) DisableWorkflow(_ context.Context, _ string, wfID string) (int64, error) {
	if _, ok := w.zombies[wfID]; !ok || w.wfDisabled[wfID] {
		return 0, nil
	}
	w.wfDisabled[wfID] = true
	return 1, nil
}

func (w *fakeDoctorWorld) ListOrphanRelTables(_ context.Context, limit int) ([]doctor.OrphanRelTable, error) {
	out := w.orphanRelTbl
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (w *fakeDoctorWorld) ListDeadRelationTargets(_ context.Context, _ string, limit int) ([]doctor.DeadRelationTargetRow, error) {
	keys := make([]string, 0, len(w.deadTargets))
	for k := range w.deadTargets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]doctor.DeadRelationTargetRow, 0, len(keys))
	for _, k := range keys {
		if len(out) >= limit {
			break
		}
		out = append(out, w.deadTargets[k])
	}
	return out, nil
}

// --- doctorSchemaRepo (the fix executors — mutate the fake physical state) ---

func (w *fakeDoctorWorld) mustTable(entity string) *fakeTableShape {
	t, ok := w.tables["obj_"+entity]
	if !ok {
		t = newFakeTableShape()
		w.tables["obj_"+entity] = t
	}
	return t
}

func (w *fakeDoctorWorld) DeletePivotByField(_ context.Context, _, _, _ string) error { return nil }

// RM-28 A2 (D1 pivot reconcile) — not exercised by these DOCTOR-CORE/-SCHEMA/-OPS fakes (D1 is
// covered end-to-end by the real-MySQL integration test); no-op/empty stubs satisfy the
// interface.
func (w *fakeDoctorWorld) BackfillUniqueField(_ context.Context, _, _, _ string) (*dynrepo.BackfillResult, error) {
	return &dynrepo.BackfillResult{}, nil
}
func (w *fakeDoctorWorld) CountMissingPivotRows(_ context.Context, _, _, _ string) (int, error) {
	return 0, nil
}
func (w *fakeDoctorWorld) ListOrphanPivotIDs(_ context.Context, _, _, _ string, _ int) ([]string, error) {
	return nil, nil
}
func (w *fakeDoctorWorld) GetPivotRow(_ context.Context, _, _ string) (*dynrepo.PivotRow, error) {
	return nil, nil
}
func (w *fakeDoctorWorld) PivotRecordLive(_ context.Context, _, _, _, _, _ string) (bool, error) {
	return false, nil
}
func (w *fakeDoctorWorld) DeletePivotRow(_ context.Context, _, _ string) error { return nil }

// ListCurrencyCodeMismatches mirrors the SQL predicate: a non-empty code (after trim) other than
// want, ordered by id, LIMIT-bounded.
func (w *fakeDoctorWorld) ListCurrencyCodeMismatches(_ context.Context, entity, _, field, want string, limit int) ([]dynrepo.CurrencyCodeMismatch, error) {
	w.gotWantCodes = append(w.gotWantCodes, want)
	byID := w.currencyCodes[entity+"."+field]
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := []dynrepo.CurrencyCodeMismatch{}
	for _, id := range ids {
		code := strings.TrimSpace(byID[id])
		if code == "" || code == want {
			continue
		}
		if len(out) >= limit {
			break
		}
		out = append(out, dynrepo.CurrencyCodeMismatch{ID: id, Code: byID[id]})
	}
	return out, nil
}

// AddUniqueGencolNonUnique (RM-28 Slice B) materializes the SAME gencol AddUniqueColumn used to
// (fakeGuardedExpr — still CASE-guarded, unaffected), but a NON-unique idx_ index — it can never
// return ErrDuplicateUnique (the dupUnique fixture hook stays unused for this method on purpose:
// there is no code path left that could feed it a "unique" kind).
func (w *fakeDoctorWorld) AddUniqueGencolNonUnique(_ context.Context, entity, field string) error {
	t := w.mustTable(entity)
	t.columns[field] = fakeGuardedExpr
	t.indexes["idx_obj_"+entity+"_"+field] = []string{"tenant_id", field}
	return nil
}

func (w *fakeDoctorWorld) AddRelationColumn(_ context.Context, entity, field string) error {
	t := w.mustTable(entity)
	t.columns[field+"_id"] = fakeUnguardedExpr
	t.indexes["idx_obj_"+entity+"_"+field+"_id"] = []string{"tenant_id", field + "_id"}
	return nil
}

func (w *fakeDoctorWorld) AddOneToOneRelationColumn(_ context.Context, entity, field string) error {
	if w.dupUnique[entity+"."+field] {
		return fmt.Errorf("dynrepo.AddOneToOneRelationColumn: %w", dynrepo.ErrDuplicateUnique)
	}
	t := w.mustTable(entity)
	t.columns[field+"_id"] = fakeGuardedExpr
	t.indexes["uq_obj_"+entity+"_"+field+"_id"] = []string{"tenant_id", field + "_id"}
	return nil
}

func (w *fakeDoctorWorld) AddPolymorphicColumns(_ context.Context, entity, field string) error {
	t := w.mustTable(entity)
	t.columns[field+"_type"] = fakeUnguardedExpr
	t.columns[field+"_id"] = fakeUnguardedExpr
	t.indexes["idx_obj_"+entity+"_"+field] = []string{"tenant_id", field + "_type", field + "_id"}
	return nil
}

func (w *fakeDoctorWorld) AddIndexedColumn(_ context.Context, entity, field, _ string) error {
	t := w.mustTable(entity)
	t.columns[field] = fakeUnguardedExpr
	t.indexes["idx_obj_"+entity+"_"+field] = []string{"tenant_id", field}
	return nil
}

func (w *fakeDoctorWorld) GuardUniqueColumn(_ context.Context, entity, field string) error {
	if w.dupUnique[entity+"."+field] {
		return fmt.Errorf("dynrepo.GuardUniqueColumn: %w", dynrepo.ErrDuplicateUnique)
	}
	w.mustTable(entity).columns[field] = fakeGuardedExpr
	return nil
}

func (w *fakeDoctorWorld) GuardOneToOneRelationColumn(_ context.Context, entity, field string) error {
	w.mustTable(entity).columns[field+"_id"] = fakeGuardedExpr
	return nil
}

func (w *fakeDoctorWorld) AddMandatoryColumn(_ context.Context, entity, column string) error {
	w.mustTable(entity).columns[column] = ""
	return nil
}

func (w *fakeDoctorWorld) EnsureMandatoryIndex(_ context.Context, entity, index string, _ bool) error {
	for _, ix := range ddl.MandatoryIndexes {
		if ix.Name == index {
			w.mustTable(entity).indexes[index] = append([]string{}, ix.Columns...)
			return nil
		}
	}
	return fmt.Errorf("fake: unknown mandatory index %q", index)
}

func (w *fakeDoctorWorld) CreateTable(_ context.Context, entity string, fields []ddl.FieldDef) error {
	if _, ok := w.tables["obj_"+entity]; ok {
		return nil // IF NOT EXISTS semantics
	}
	t := newFakeTableShape()
	t.addMandatoryShape()
	for _, f := range fields {
		switch {
		case f.IsUnique:
			// RM-28 Slice B: CreateTable's unique branch now emits a non-unique idx_ index.
			t.columns[f.Name] = fakeGuardedExpr
			t.indexes["idx_obj_"+entity+"_"+f.Name] = []string{"tenant_id", f.Name}
		case f.FieldType == "relation" && f.OneToOne:
			t.columns[f.Name+"_id"] = fakeGuardedExpr
			t.indexes["uq_obj_"+entity+"_"+f.Name+"_id"] = []string{"tenant_id", f.Name + "_id"}
		case f.FieldType == "relation":
			t.columns[f.Name+"_id"] = fakeUnguardedExpr
			t.indexes["idx_obj_"+entity+"_"+f.Name+"_id"] = []string{"tenant_id", f.Name + "_id"}
		case f.Indexed:
			t.columns[f.Name] = fakeUnguardedExpr
			t.indexes["idx_obj_"+entity+"_"+f.Name] = []string{"tenant_id", f.Name}
		}
	}
	w.tables["obj_"+entity] = t
	return nil
}

func (w *fakeDoctorWorld) CreateJoinTable(_ context.Context, entityA, entityB string) error {
	name, _, _, err := ddl.JoinParts(entityA, entityB)
	if err != nil {
		return err
	}
	if _, ok := w.tables[name]; !ok {
		w.tables[name] = newFakeTableShape()
	}
	return nil
}

// --- doctorRelRepo (the SoftDeleteRelationshipByID repair helper) ---

func (w *fakeDoctorWorld) SoftDeleteRelationshipByID(_ context.Context, _ string, relID string) error {
	if _, ok := w.phantoms[relID]; ok {
		delete(w.phantoms, relID)
		return nil
	}
	if _, ok := w.orphans[relID]; ok {
		delete(w.orphans, relID)
		return nil
	}
	return metadata.ErrRelationshipNotFound
}

// --- doctorLayoutRepo ---

func (w *fakeDoctorWorld) GetByID(_ context.Context, _ string, id string) (*metadata.LayoutDefinition, error) {
	l, ok := w.layouts[id]
	if !ok {
		return nil, metadata.ErrLayoutNotFound
	}
	cp := *l
	return &cp, nil
}
func (w *fakeDoctorWorld) UpdateByID(_ context.Context, _ string, id, layoutJSON string) error {
	l, ok := w.layouts[id]
	if !ok {
		return metadata.ErrLayoutNotFound
	}
	l.LayoutJSON = layoutJSON
	return nil
}
func (w *fakeDoctorWorld) HardDelete(_ context.Context, _ string, id string) error {
	if _, ok := w.layouts[id]; !ok {
		return metadata.ErrLayoutNotFound
	}
	delete(w.layouts, id)
	return nil
}

// --- LayoutMetaReader ---

func (w *fakeDoctorWorld) GetEntityByName(_ context.Context, _ string, name string) (*metadata.EntityDefinition, error) {
	e, ok := w.entities[name]
	if !ok {
		return nil, metadata.ErrEntityNotFound
	}
	cp := *e
	return &cp, nil
}
func (w *fakeDoctorWorld) ListFields(_ context.Context, _ string, entityID string) ([]metadata.FieldDefinition, error) {
	return w.fields[entityID], nil
}

// fakeDoctorAuditor captures audit events synchronously.
type fakeDoctorAuditor struct{ events []audit.Event }

func (f *fakeDoctorAuditor) Record(_ context.Context, evt audit.Event) {
	f.events = append(f.events, evt)
}

func newDoctorFixture() (*fakeDoctorWorld, *fakeDoctorAuditor, *DoctorService) {
	w := newFakeDoctorWorld()
	au := &fakeDoctorAuditor{}
	svc := NewDoctorService(w, w, w, w, w, au, nil)
	return w, au, svc
}

// scanByID indexes a scan result by check id.
func scanByID(t *testing.T, svc *DoctorService, tenantID string) map[string]doctor.CheckResult {
	t.Helper()
	results, err := svc.Scan(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	out := make(map[string]doctor.CheckResult, len(results))
	for _, r := range results {
		out[r.CheckID] = r
	}
	return out
}

func TestDoctor_ScanListsAllChecksEvenWhenClean(t *testing.T) {
	_, _, svc := newDoctorFixture()
	res := scanByID(t, svc, "tenant-a")
	// Contract per check id: A6/B4/C4 are needs-confirm (destructive-recreate / behavior
	// flip); A3/B5/C8 are report-only (no fixer at all); everything else is safe.
	wantSafe := map[string]bool{
		DoctorCheckMissingGenColumn:      true,
		DoctorCheckUnguardedUnique:       true,
		DoctorCheckOrphanUniqueIndex:     false,
		DoctorCheckMandatoryDDL:          true,
		DoctorCheckMissingObjTable:       false,
		DoctorCheckPhantomRelationships:  true,
		DoctorCheckOrphanRelationRows:    true,
		DoctorCheckMissingJoinTable:      false,
		DoctorCheckUnregisteredRelTables: false,
		DoctorCheckLayoutDeadFields:      true,
		DoctorCheckOrphanNavItems:        true,
		DoctorCheckZombieWorkflows:       false,
		DoctorCheckDeadRelationTarget:    false,
		DoctorCheckPivotReconcile:        true,
		DoctorCheckCurrencyCodeMismatch:  false,
	}
	wantReportOnly := map[string]bool{
		DoctorCheckOrphanUniqueIndex:     true,
		DoctorCheckUnregisteredRelTables: true,
		DoctorCheckDeadRelationTarget:    true,
		DoctorCheckCurrencyCodeMismatch:  true,
	}
	if len(res) != len(wantSafe) {
		t.Fatalf("scan trả %d check; want %d (registry đủ 15 check)", len(res), len(wantSafe))
	}
	for id, safe := range wantSafe {
		r, ok := res[id]
		if !ok {
			t.Fatalf("scan thiếu check %s — FE cần checklist đầy đủ kể cả khi sạch", id)
		}
		if r.Count != 0 || r.Items == nil || r.AutoFixSafe != safe {
			t.Fatalf("check %s = %+v; want count 0, items [], autoFixSafe %v", id, r, safe)
		}
		if r.ReportOnly != wantReportOnly[id] {
			t.Fatalf("check %s reportOnly = %v; want %v (wire contract cho FE)", id, r.ReportOnly, wantReportOnly[id])
		}
	}
}

func TestDoctor_B1_PhantomRelationships_FixThenClean(t *testing.T) {
	w, au, svc := newDoctorFixture()
	w.phantoms["rel-1"] = doctor.RelationshipRow{ID: "rel-1", Name: "ghost_rel", ParentEntityName: "gone", ChildEntityName: "accounts", RelationshipType: "N-N"}
	w.phantoms["rel-2"] = doctor.RelationshipRow{ID: "rel-2", Name: "ghost_rel2", ParentEntityName: "accounts", ChildEntityName: "gone2", RelationshipType: "1-N"}

	if got := scanByID(t, svc, "t-a")[DoctorCheckPhantomRelationships]; got.Count != 2 {
		t.Fatalf("detect = %+v; want 2 phantom", got)
	}
	results, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckPhantomRelationships}, false)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if results[0].Fixed != 2 || results[0].Failed != 0 {
		t.Fatalf("fix = %+v; want 2 fixed", results[0])
	}
	if got := scanByID(t, svc, "t-a")[DoctorCheckPhantomRelationships]; got.Count != 0 {
		t.Fatalf("re-scan = %+v; want clean (idempotent re-run)", got)
	}
	// Idempotency: a second fix run finds nothing to do.
	results, err = svc.Fix(context.Background(), "t-a", []string{DoctorCheckPhantomRelationships}, false)
	if err != nil || results[0].Fixed != 0 {
		t.Fatalf("second fix = %+v (err %v); want 0 fixed", results[0], err)
	}
	// One audit row per fixed finding, marked __doctor__ with the check id in the diff.
	if len(au.events) != 2 {
		t.Fatalf("audit events = %d; want 2 (one per fix)", len(au.events))
	}
	evt := au.events[0]
	if evt.EntityName != audit.DoctorMeta || evt.TenantID != "t-a" ||
		evt.Diff == nil || evt.Diff.After["checkId"] != DoctorCheckPhantomRelationships {
		t.Fatalf("audit event = %+v; want __doctor__ với checkId trong diff", evt)
	}
}

func TestDoctor_B1_StaleFindingSkipped(t *testing.T) {
	w, au, svc := newDoctorFixture()
	w.phantoms["rel-1"] = doctor.RelationshipRow{ID: "rel-1", Name: "ghost", ParentEntityName: "x", ChildEntityName: "y", RelationshipType: "N-N"}
	w.stale["rel-1"] = true // list still reports it; the per-item re-detect says clean

	results, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckPhantomRelationships}, false)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if results[0].Skipped != 1 || results[0].Fixed != 0 {
		t.Fatalf("fix = %+v; want 1 skipped (re-detect-first contract)", results[0])
	}
	if len(au.events) != 0 {
		t.Fatalf("skipped item không được ghi audit — events = %d", len(au.events))
	}
}

func TestDoctor_B2_OrphanRelationRows_FixThenClean(t *testing.T) {
	w, _, svc := newDoctorFixture()
	w.orphans["rel-9"] = doctor.RelationshipRow{ID: "rel-9", Name: "contacts_account", ParentEntityName: "accounts", ChildEntityName: "contacts", RelationshipType: "1-N", ChildFKColumn: "account_id"}

	if got := scanByID(t, svc, "t-a")[DoctorCheckOrphanRelationRows]; got.Count != 1 {
		t.Fatalf("detect = %+v; want 1 orphan", got)
	}
	results, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckOrphanRelationRows}, false)
	if err != nil || results[0].Fixed != 1 {
		t.Fatalf("fix = %+v (err %v); want 1 fixed", results[0], err)
	}
	if got := scanByID(t, svc, "t-a")[DoctorCheckOrphanRelationRows]; got.Count != 0 {
		t.Fatalf("re-scan = %+v; want clean", got)
	}
}

func TestDoctor_C1_LayoutDeadFields(t *testing.T) {
	seed := func() (*fakeDoctorWorld, *DoctorService) {
		w, _, svc := newDoctorFixture()
		w.entities["accounts"] = &metadata.EntityDefinition{ID: "ent-1", Name: "accounts"}
		w.fields["ent-1"] = []metadata.FieldDefinition{{ID: "f1", EntityID: "ent-1", Name: "name", FieldType: "text"}}
		return w, svc
	}

	t.Run("layout còn field chết → persist bản đã prune", func(t *testing.T) {
		w, svc := seed()
		w.layouts["lay-1"] = &metadata.LayoutDefinition{ID: "lay-1", EntityName: "accounts", ViewType: "list",
			LayoutJSON: `{"columns":[{"field":"name"},{"field":"ghost"}]}`}

		if got := scanByID(t, svc, "t-a")[DoctorCheckLayoutDeadFields]; got.Count != 1 {
			t.Fatalf("detect = %+v; want 1 dirty layout", got)
		}
		results, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckLayoutDeadFields}, false)
		if err != nil || results[0].Fixed != 1 {
			t.Fatalf("fix = %+v (err %v); want 1 fixed", results[0], err)
		}
		// Persisted JSON must be EXACTLY the serve-time pruned shape (ghost dropped, name kept).
		var stored map[string]any
		if err := json.Unmarshal([]byte(w.layouts["lay-1"].LayoutJSON), &stored); err != nil {
			t.Fatalf("persisted layout_json không parse được: %v", err)
		}
		cols, _ := stored["columns"].([]any)
		if len(cols) != 1 {
			t.Fatalf("persisted columns = %v; want chỉ còn 'name'", stored["columns"])
		}
		if got := scanByID(t, svc, "t-a")[DoctorCheckLayoutDeadFields]; got.Count != 0 {
			t.Fatalf("re-scan = %+v; want clean", got)
		}
	})

	t.Run("layout của entity tombstoned → hard-delete row", func(t *testing.T) {
		w, svc := seed()
		w.layouts["lay-2"] = &metadata.LayoutDefinition{ID: "lay-2", EntityName: "gone_entity", ViewType: "edit",
			LayoutJSON: `{"sections":[{"title":"a","fields":["x"]}]}`}

		if got := scanByID(t, svc, "t-a")[DoctorCheckLayoutDeadFields]; got.Count != 1 {
			t.Fatalf("detect = %+v; want 1 finding", got)
		}
		results, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckLayoutDeadFields}, false)
		if err != nil || results[0].Fixed != 1 {
			t.Fatalf("fix = %+v (err %v); want 1 fixed", results[0], err)
		}
		if _, ok := w.layouts["lay-2"]; ok {
			t.Fatalf("layout của entity chết phải bị xóa hẳn")
		}
	})

	t.Run("layout_json hỏng → skip, không phải finding, không lỗi scan", func(t *testing.T) {
		w, svc := seed()
		w.layouts["lay-3"] = &metadata.LayoutDefinition{ID: "lay-3", EntityName: "accounts", ViewType: "list",
			LayoutJSON: `{not json`}
		if got := scanByID(t, svc, "t-a")[DoctorCheckLayoutDeadFields]; got.Count != 0 {
			t.Fatalf("detect = %+v; corrupt JSON không thuộc lớp C1", got)
		}
	})

	t.Run("layout sạch → không finding", func(t *testing.T) {
		w, svc := seed()
		w.layouts["lay-4"] = &metadata.LayoutDefinition{ID: "lay-4", EntityName: "accounts", ViewType: "list",
			LayoutJSON: `{"columns":[{"field":"name"}]}`}
		if got := scanByID(t, svc, "t-a")[DoctorCheckLayoutDeadFields]; got.Count != 0 {
			t.Fatalf("detect = %+v; layout sạch không được flag", got)
		}
	})
}

func TestDoctor_C2_OrphanNavItems_FixThenClean(t *testing.T) {
	w, _, svc := newDoctorFixture()
	w.navRows["entity:dead-ent-id"] = 2

	got := scanByID(t, svc, "t-a")[DoctorCheckOrphanNavItems]
	if got.Count != 1 || got.Items[0].Key != "entity:dead-ent-id" {
		t.Fatalf("detect = %+v; want 1 group key entity:dead-ent-id", got)
	}
	results, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckOrphanNavItems}, false)
	if err != nil || results[0].Fixed != 1 {
		t.Fatalf("fix = %+v (err %v); want 1 fixed", results[0], err)
	}
	if len(w.navRows) != 0 {
		t.Fatalf("nav rows còn %v; want purged", w.navRows)
	}
	if got := scanByID(t, svc, "t-a")[DoctorCheckOrphanNavItems]; got.Count != 0 {
		t.Fatalf("re-scan = %+v; want clean", got)
	}
}

func TestDoctor_Fix_UnknownAndEmptyCheckIDs(t *testing.T) {
	_, _, svc := newDoctorFixture()
	for name, ids := range map[string][]string{
		"checkId lạ":    {"X9_NOPE"},
		"checkIds rỗng": {},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Fix(context.Background(), "t-a", ids, false)
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != "DOCTOR_CHECK_UNKNOWN" {
				t.Fatalf("err = %v; want ValidationError DOCTOR_CHECK_UNKNOWN", err)
			}
		})
	}
}

func TestDoctor_FixBatchInvalidatesMetaCache(t *testing.T) {
	w, _, _ := newDoctorFixture()
	w.phantoms["rel-1"] = doctor.RelationshipRow{ID: "rel-1", Name: "ghost", ParentEntityName: "x", ChildEntityName: "y", RelationshipType: "N-N"}

	// countingCatalog is the shared fake from metadata_cache_test.go.
	catalog := &countingCatalog{}
	cache := NewMetaCache(catalog, time.Hour) // long TTL: only invalidation can force a reload
	svc := NewDoctorService(w, w, w, w, w, nil, nil).WithMetaCache(cache)

	// Warm the cache.
	if _, err := cache.GetEntityByName(context.Background(), "t-a", "accounts"); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if n := atomic.LoadInt64(&catalog.entityCalls); n != 1 {
		t.Fatalf("loads = %d; want 1 after warm", n)
	}
	if _, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckPhantomRelationships}, false); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	// The fix batch fixed ≥1 item → cache must have been blanket-invalidated.
	if _, err := cache.GetEntityByName(context.Background(), "t-a", "accounts"); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if n := atomic.LoadInt64(&catalog.entityCalls); n != 2 {
		t.Fatalf("loads = %d; want 2 — fix batch phải invalidate MetaCache", n)
	}
}

// ---- DOCTOR-SCHEMA unit tests (A1/A2a/A5/A6/B4 over the fake information_schema) -------------

// seedSchemaEntity plants an active entity + a fresh fake table carrying the full mandatory
// shape, returning the entity id.
func seedSchemaEntity(w *fakeDoctorWorld, name string) string {
	id := "ent-" + name
	w.entities[name] = &metadata.EntityDefinition{ID: id, Name: name}
	tbl := newFakeTableShape()
	tbl.addMandatoryShape()
	w.tables["obj_"+name] = tbl
	return id
}

func TestDoctor_A1_MissingGenColumn_FixThenClean(t *testing.T) {
	w, _, svc := newDoctorFixture()
	seedSchemaEntity(w, "accounts")
	opts11 := `{"relatedEntity":"accounts","cardinality":"1-1"}`
	_ = opts11
	w.physFields["f-email"] = doctor.PhysicalFieldRow{FieldID: "f-email", EntityName: "accounts", FieldName: "email", FieldType: "text", IsUnique: 1}
	w.physFields["f-parent"] = doctor.PhysicalFieldRow{FieldID: "f-parent", EntityName: "accounts", FieldName: "parent", FieldType: "relation", Options: `{"relatedEntity":"accounts"}`}
	w.physFields["f-partner"] = doctor.PhysicalFieldRow{FieldID: "f-partner", EntityName: "accounts", FieldName: "partner", FieldType: "relation", Options: opts11}
	w.physFields["f-score"] = doctor.PhysicalFieldRow{FieldID: "f-score", EntityName: "accounts", FieldName: "score", FieldType: "number", Options: `{"indexed":true}`}

	got := scanByID(t, svc, "t-a")[DoctorCheckMissingGenColumn]
	if got.Count != 4 {
		t.Fatalf("detect = %+v; want 4 missing columns (unique + 1-N + 1-1 + indexed)", got)
	}
	// SQL preview: every finding embeds the WOULD-RUN ALTER (spec §2.3 proposedSql-in-detail).
	for _, it := range got.Items {
		if !strings.Contains(it.Detail, "— SQL: ALTER TABLE `obj_accounts`") {
			t.Fatalf("finding %q thiếu SQL preview trong detail: %s", it.Key, it.Detail)
		}
	}
	results, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckMissingGenColumn}, false)
	if err != nil || results[0].Fixed != 4 || results[0].Failed != 0 {
		t.Fatalf("fix = %+v (err %v); want 4 fixed", results[0], err)
	}
	tbl := w.tables["obj_accounts"]
	for _, col := range []string{"email", "parent_id", "partner_id", "score"} {
		if _, ok := tbl.columns[col]; !ok {
			t.Fatalf("cột %q chưa được materialize sau fix; columns = %v", col, tbl.columns)
		}
	}
	if got := scanByID(t, svc, "t-a")[DoctorCheckMissingGenColumn]; got.Count != 0 {
		t.Fatalf("re-scan = %+v; want clean (idempotent)", got)
	}
}

func TestDoctor_A1_TableGoneIsNotA1sClass(t *testing.T) {
	w, _, svc := newDoctorFixture()
	w.entities["accounts"] = &metadata.EntityDefinition{ID: "ent-1", Name: "accounts"}
	// NO table — A6's class; A1 must stay silent (disjoint classes).
	w.physFields["f-email"] = doctor.PhysicalFieldRow{FieldID: "f-email", EntityName: "accounts", FieldName: "email", FieldType: "text", IsUnique: 1}
	res := scanByID(t, svc, "t-a")
	if res[DoctorCheckMissingGenColumn].Count != 0 {
		t.Fatalf("A1 = %+v; bảng biến mất là lớp A6, A1 không được flag", res[DoctorCheckMissingGenColumn])
	}
	if res[DoctorCheckMissingObjTable].Count != 1 {
		t.Fatalf("A6 = %+v; want 1 finding", res[DoctorCheckMissingObjTable])
	}
}

// TestDoctor_A1_CrossTenantDuplicateIsPublicFailedItem covers the ONE remaining A1 kind that can
// still 1062 on a shared table post RM-28 Slice B: a 1-1 relation's cardinality index
// (AddOneToOneRelationColumn) is a SEPARATE physical UNIQUE index, NOT pivot-backed, NOT
// retired. A plain `is_unique` field can no longer reach this path — its A1 fix
// (AddUniqueGencolNonUnique) materializes a non-unique idx_ index that cannot 1062.
func TestDoctor_A1_CrossTenantDuplicateIsPublicFailedItem(t *testing.T) {
	w, _, svc := newDoctorFixture()
	seedSchemaEntity(w, "accounts")
	w.physFields["f-partner"] = doctor.PhysicalFieldRow{FieldID: "f-partner", EntityName: "accounts", FieldName: "partner", FieldType: "relation", Options: `{"relatedEntity":"accounts","cardinality":"1-1"}`}
	w.dupUnique["accounts.partner"] = true // shared table already violates 1-1 exclusivity → 1062

	results, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckMissingGenColumn}, false)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	r := results[0]
	if r.Failed != 1 || r.Fixed != 0 || len(r.Errors) != 1 {
		t.Fatalf("fix = %+v; want 1 failed item (KHÔNG retry)", r)
	}
	// W-1 sanitize giữ verdict public (stable code) nhưng không lộ internals wrap/driver.
	if !strings.Contains(r.Errors[0], "FIELD_RELATION_CARDINALITY_CONFLICT") {
		t.Fatalf("error = %q; want verdict FIELD_RELATION_CARDINALITY_CONFLICT xuyên qua sanitizer", r.Errors[0])
	}
	if strings.Contains(r.Errors[0], "dynrepo.") || strings.Contains(r.Errors[0], "[public]") {
		t.Fatalf("error = %q; internals/marker không được lộ ra client", r.Errors[0])
	}
}

func TestDoctor_A2a_UnguardedColumn_FixThenClean(t *testing.T) {
	w, _, svc := newDoctorFixture()
	seedSchemaEntity(w, "accounts")
	tbl := w.tables["obj_accounts"]
	// sku: unique column present but UNGUARDED (the W5-E1 regression class).
	w.physFields["f-sku"] = doctor.PhysicalFieldRow{FieldID: "f-sku", EntityName: "accounts", FieldName: "sku", FieldType: "text", IsUnique: 1}
	tbl.columns["sku"] = fakeUnguardedExpr
	// email: unique column present and guarded → clean.
	w.physFields["f-email"] = doctor.PhysicalFieldRow{FieldID: "f-email", EntityName: "accounts", FieldName: "email", FieldType: "text", IsUnique: 1}
	tbl.columns["email"] = fakeGuardedExpr
	// plain_col: a NON-generated column under a unique field name → NOT A2a's class
	// (MODIFY-to-generated là destructive, mirror đúng predicate 000015).
	w.physFields["f-plain"] = doctor.PhysicalFieldRow{FieldID: "f-plain", EntityName: "accounts", FieldName: "plain_col", FieldType: "text", IsUnique: 1}
	tbl.columns["plain_col"] = ""

	got := scanByID(t, svc, "t-a")[DoctorCheckUnguardedUnique]
	if got.Count != 1 || got.Items[0].Key != "f-sku" {
		t.Fatalf("detect = %+v; want đúng 1 finding (sku)", got)
	}
	if !strings.Contains(got.Items[0].Detail, "MODIFY COLUMN") {
		t.Fatalf("detail thiếu SQL preview MODIFY: %s", got.Items[0].Detail)
	}
	results, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckUnguardedUnique}, false)
	if err != nil || results[0].Fixed != 1 {
		t.Fatalf("fix = %+v (err %v); want 1 fixed", results[0], err)
	}
	if !strings.Contains(tbl.columns["sku"], "deleted_at") {
		t.Fatalf("sku expr = %q; want guarded sau fix", tbl.columns["sku"])
	}
	if got := scanByID(t, svc, "t-a")[DoctorCheckUnguardedUnique]; got.Count != 0 {
		t.Fatalf("re-scan = %+v; want clean", got)
	}
}

func TestDoctor_A5_MandatoryDrift_FixThenClean(t *testing.T) {
	w, _, svc := newDoctorFixture()
	seedSchemaEntity(w, "accounts")
	tbl := w.tables["obj_accounts"]
	delete(tbl.columns, "version")                         // 000013 residue
	delete(tbl.indexes, "idx_updated_at")                  // missing composite
	tbl.indexes["idx_created_at"] = []string{"created_at"} // RM-47: right name, WRONG shape

	got := scanByID(t, svc, "t-a")[DoctorCheckMandatoryDDL]
	if got.Count != 3 {
		t.Fatalf("detect = %+v; want 3 findings (version + idx_updated_at + idx_created_at reshape)", got)
	}
	keys := map[string]bool{}
	for _, it := range got.Items {
		keys[it.Key] = true
		if !strings.Contains(it.Detail, "— SQL: ALTER TABLE `obj_accounts`") {
			t.Fatalf("finding %q thiếu SQL preview: %s", it.Key, it.Detail)
		}
	}
	for _, k := range []string{"accounts:column:version", "accounts:index:idx_updated_at", "accounts:index:idx_created_at"} {
		if !keys[k] {
			t.Fatalf("thiếu finding key %q trong %v", k, keys)
		}
	}
	results, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckMandatoryDDL}, false)
	if err != nil || results[0].Fixed != 3 || results[0].Failed != 0 {
		t.Fatalf("fix = %+v (err %v); want 3 fixed", results[0], err)
	}
	if _, ok := tbl.columns["version"]; !ok {
		t.Fatalf("version column chưa quay lại sau fix")
	}
	wantIdx := []string{"tenant_id", "deleted_at", "created_at", "id"}
	if got := tbl.indexes["idx_created_at"]; len(got) != 4 || got[0] != wantIdx[0] || got[3] != wantIdx[3] {
		t.Fatalf("idx_created_at = %v; want composite %v (RM-47 paid)", got, wantIdx)
	}
	if got := scanByID(t, svc, "t-a")[DoctorCheckMandatoryDDL]; got.Count != 0 {
		t.Fatalf("re-scan = %+v; want clean", got)
	}
}

func TestDoctor_A6_ConfirmGateThenRecreate(t *testing.T) {
	w, _, svc := newDoctorFixture()
	entID := "ent-tickets"
	w.entities["tickets"] = &metadata.EntityDefinition{ID: entID, Name: "tickets"}
	w.fields[entID] = []metadata.FieldDefinition{
		{ID: "f1", EntityID: entID, Name: "code", FieldType: "text", IsUnique: true},
		{ID: "f2", EntityID: entID, Name: "subject", FieldType: "text"},
	}
	// NO w.tables entry — the obj_tickets table was DROPped by hand.

	got := scanByID(t, svc, "t-a")[DoctorCheckMissingObjTable]
	if got.Count != 1 || got.Items[0].Key != "tickets" || got.AutoFixSafe {
		t.Fatalf("detect = %+v; want 1 finding key=tickets, autoFixSafe=false", got)
	}
	if !strings.Contains(got.Items[0].Detail, "KHÔNG quay lại") || !strings.Contains(got.Items[0].Detail, "CREATE TABLE IF NOT EXISTS `obj_tickets`") {
		t.Fatalf("detail phải nêu hệ quả mất data + SQL preview: %s", got.Items[0].Detail)
	}

	// Needs-confirm contract: refuse the WHOLE request without confirm=true.
	_, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckMissingObjTable}, false)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "DOCTOR_CONFIRM_REQUIRED" {
		t.Fatalf("err = %v; want ValidationError DOCTOR_CONFIRM_REQUIRED", err)
	}
	if _, ok := w.tables["obj_tickets"]; ok {
		t.Fatalf("bảng bị tạo dù request bị reject — mutation trước validation")
	}

	results, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckMissingObjTable}, true)
	if err != nil || results[0].Fixed != 1 {
		t.Fatalf("fix confirm=true = %+v (err %v); want 1 fixed", results[0], err)
	}
	tbl := w.tables["obj_tickets"]
	if tbl == nil {
		t.Fatalf("obj_tickets chưa được recreate")
	}
	if _, ok := tbl.columns["code"]; !ok {
		t.Fatalf("unique field 'code' phải có generated column sau recreate; cols = %v", tbl.columns)
	}
	if got := scanByID(t, svc, "t-a")[DoctorCheckMissingObjTable]; got.Count != 0 {
		t.Fatalf("re-scan = %+v; want clean", got)
	}
}

func TestDoctor_B4_ConfirmGateThenRecreate(t *testing.T) {
	w, _, svc := newDoctorFixture()
	canonical, _, _, err := ddl.JoinParts("accounts", "tags")
	if err != nil {
		t.Fatalf("JoinParts: %v", err)
	}
	w.nnRels["rel-1"] = doctor.NNRelationshipRow{ID: "rel-1", Name: "accounts_tags",
		ParentEntityName: "accounts", ChildEntityName: "tags", JoinTableName: canonical}
	// Join table absent → drift.

	got := scanByID(t, svc, "t-a")[DoctorCheckMissingJoinTable]
	if got.Count != 1 || got.Items[0].Key != "rel-1" || got.AutoFixSafe {
		t.Fatalf("detect = %+v; want 1 finding, autoFixSafe=false", got)
	}
	if !strings.Contains(got.Items[0].Detail, "CREATE TABLE IF NOT EXISTS `"+canonical+"`") {
		t.Fatalf("detail thiếu SQL preview: %s", got.Items[0].Detail)
	}
	_, err = svc.Fix(context.Background(), "t-a", []string{DoctorCheckMissingJoinTable}, false)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "DOCTOR_CONFIRM_REQUIRED" {
		t.Fatalf("err = %v; want DOCTOR_CONFIRM_REQUIRED", err)
	}
	results, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckMissingJoinTable}, true)
	if err != nil || results[0].Fixed != 1 {
		t.Fatalf("fix confirm=true = %+v (err %v); want 1 fixed", results[0], err)
	}
	if _, ok := w.tables[canonical]; !ok {
		t.Fatalf("join table %q chưa được recreate", canonical)
	}
	if got := scanByID(t, svc, "t-a")[DoctorCheckMissingJoinTable]; got.Count != 0 {
		t.Fatalf("re-scan = %+v; want clean", got)
	}
}

func TestDoctor_B4_NonCanonicalJoinNameRefusedPublicly(t *testing.T) {
	w, _, svc := newDoctorFixture()
	w.nnRels["rel-odd"] = doctor.NNRelationshipRow{ID: "rel-odd", Name: "odd",
		ParentEntityName: "accounts", ChildEntityName: "tags", JoinTableName: "rel_hand_edited"}

	got := scanByID(t, svc, "t-a")[DoctorCheckMissingJoinTable]
	if got.Count != 1 || !strings.Contains(got.Items[0].Detail, "lệch tên chuẩn") {
		t.Fatalf("detect = %+v; want finding nêu rõ tên lệch chuẩn", got)
	}
	results, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckMissingJoinTable}, true)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if results[0].Failed != 1 || !strings.Contains(results[0].Errors[0], "không khớp tên chuẩn") {
		t.Fatalf("fix = %+v; want 1 failed item với verdict public", results[0])
	}
	if _, ok := w.tables["rel_hand_edited"]; ok {
		t.Fatalf("fix KHÔNG được tạo bảng theo tên sửa tay")
	}
}
