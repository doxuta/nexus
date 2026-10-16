package services

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// specs/061 FR-012..FR-015 (#268) — a CSV relation column names its target the way the customer's
// file does: "Phú Gia Riverside", "Công ty TNHH Lạnh Việt 1", or a code — never a UUID.

// multiCatalog serves several entities; ListFields is keyed by entity id (= name here).
type multiCatalog struct {
	fields map[string][]metadata.FieldDefinition
	opts   map[string]string // entity -> sys_entities.options JSON (#272), optional
}

func (c *multiCatalog) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	if _, ok := c.fields[name]; !ok {
		return nil, metadata.ErrEntityNotFound
	}
	e := &metadata.EntityDefinition{ID: name, Name: name, Label: strings.ToUpper(name[:1]) + name[1:]}
	if o, ok := c.opts[name]; ok {
		e.Options = &o
	}
	return e, nil
}

func (c *multiCatalog) ListFields(_ context.Context, _, entityID string) ([]metadata.FieldDefinition, error) {
	return c.fields[entityID], nil
}

// matchStore is an ImportStore + RelationBatchChecker + value matcher over in-memory records.
type matchStore struct {
	fakeImportStore
	ids     map[string]map[string]bool                // entity → live ids
	values  map[string]map[string]map[string][]string // entity → field → lowered value → ids
	owners  map[string]string                         // id → owner; "" = owned by nobody the caller is
	matches int                                       // BatchIDsByFieldValue calls
	scopes  []dynrepo.RowScope                        // the row scope each value lookup ran under
	// idScopes are the row scopes the id probe ran under (#331); idProbes counts the tenant-wide
	// BatchRecordExists calls, which a non-admin caller must never reach.
	idScopes []dynrepo.RowScope
	idProbes int
	// existsErr / matchErr fail the id check / the value lookup (a DB error).
	existsErr, matchErr error
}

func (m *matchStore) BatchRecordExists(_ context.Context, entity, _ string, ids []string) (map[string]bool, error) {
	m.idProbes++
	if m.existsErr != nil {
		return nil, m.existsErr
	}
	out := map[string]bool{}
	for _, id := range ids {
		if m.ids[entity][id] {
			out[id] = true
		}
	}
	return out, nil
}

// BatchRecordExistsInScope is the id probe a non-admin import runs under (#331): it narrows like the
// value lookup — all/admin sees every record, AccessNone nothing, anything else only the owner's.
func (m *matchStore) BatchRecordExistsInScope(_ context.Context, entity, _ string, ids []string, scope dynrepo.RowScope) (map[string]bool, error) {
	m.idScopes = append(m.idScopes, scope)
	if m.existsErr != nil {
		return nil, m.existsErr
	}
	out := map[string]bool{}
	for _, id := range ids {
		if !m.ids[entity][id] || scope.Level == dynrepo.AccessNone {
			continue
		}
		if scope.IsAdmin || scope.Level == dynrepo.AccessAll || m.owners[id] == scope.UserID {
			out[id] = true
		}
	}
	return out, nil
}

// BatchIDsByFieldValue narrows like dynrepo's row filter for the levels these tests use: all/admin
// sees every record, anything else only the caller's own (owner) records.
func (m *matchStore) BatchIDsByFieldValue(_ context.Context, entity, _, field string, values []string, scope dynrepo.RowScope) (map[string][]string, error) {
	m.matches++
	m.scopes = append(m.scopes, scope)
	if m.matchErr != nil {
		return nil, m.matchErr
	}
	out := map[string][]string{}
	for _, v := range values {
		var ids []string // a value repeated in the file still names each record once, as in SQL
		for _, id := range m.values[entity][field][v] {
			if scope.IsAdmin || scope.Level == dynrepo.AccessAll || m.owners[id] == scope.UserID {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			out[v] = ids
		}
	}
	return out, nil
}

var _ relationValueMatcher = (*matchStore)(nil)

func relDealsCatalog() *multiCatalog {
	return &multiCatalog{fields: map[string][]metadata.FieldDefinition{
		"deals": {
			{Name: "name", FieldType: "text", IsRequired: true},
			{Name: "project", Label: "Dự án", FieldType: "relation", Options: strPtr(`{"relatedEntity":"projects"}`)},
			{Name: "customer", Label: "Khách hàng", FieldType: "relation", Options: strPtr(`{"relatedEntity":"customers"}`)},
		},
		"projects": {
			{Name: "ghi_chu", FieldType: "text"},
			{Name: "ten_du_an", Label: "Tên dự án", FieldType: "text", IsRequired: true},
			{Name: "ma_du_an", Label: "Mã dự án", FieldType: "text", IsUnique: true},
			{Name: "gia", FieldType: "currency"},
		},
		"customers": {
			{Name: "name", Label: "Tên", FieldType: "text"},
			{Name: "email", FieldType: "email"},
		},
	}}
}

func dealsStore() *matchStore {
	return &matchStore{
		ids: map[string]map[string]bool{
			"projects":  {"p1": true, "p2": true, "p3": true},
			"customers": {"c1": true},
		},
		values: map[string]map[string]map[string][]string{
			"projects": {
				"ten_du_an": {"phú gia riverside": {"p1"}, "sky garden": {"p2", "p3"}},
				"ma_du_an":  {"pgr": {"p1"}},
			},
			"customers": {"name": {"công ty tnhh lạnh việt 1": {"c1"}}},
		},
	}
}

func importDeals(t *testing.T, svc *ImportService, csv string, in ImportInput) *ImportResult {
	t.Helper()
	in.TenantID, in.UserID, in.EntityName = "t1", "u1", "deals"
	if in.Mapping == nil {
		in.Mapping = map[string]string{"Tên": "name", "Dự án": "project", "Khách hàng": "customer"}
	}
	in.Reader = strings.NewReader(csv)
	res, err := svc.Import(context.Background(), in)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	return res
}

func TestImport_RelationByName(t *testing.T) {
	store := dealsStore()
	svc := NewImportService(relDealsCatalog(), store, nil).WithRelationChecker(store)
	res := importDeals(t, svc, "Tên,Dự án,Khách hàng\n"+
		"Căn 1,Phú Gia Riverside,Công ty TNHH Lạnh Việt 1\n"+
		"Căn 2, phú gia RIVERSIDE ,c1\n"+
		"Căn 3,Sky Garden,\n"+
		"Căn 4,Không Có,Ai Đó\n", ImportInput{IsAdmin: true})

	if res.Inserted != 2 || res.Failed != 2 {
		t.Fatalf("result = %+v; want 2 inserted, 2 failed", res)
	}
	rows := store.insertedRows
	if rows[0]["project"] != "p1" || rows[0]["customer"] != "c1" || rows[1]["project"] != "p1" || rows[1]["customer"] != "c1" {
		t.Fatalf("stored = %#v; want names replaced by ids", rows)
	}
	byKey := map[string]ImportRowError{}
	for _, e := range res.Errors {
		byKey[e.Field+"@"+string(rune('0'+e.Row))] = e
	}
	amb := byKey["project@3"]
	if amb.Code != "IMPORT_RELATION_AMBIGUOUS" || !strings.Contains(amb.Message, "Sky Garden") || !strings.Contains(amb.Message, "2") {
		t.Fatalf("row 3 = %+v; want IMPORT_RELATION_AMBIGUOUS naming the value and the count", amb)
	}
	miss := byKey["project@4"]
	if miss.Code != "RECORD_RELATION_INVALID" || !strings.Contains(miss.Message, "Không Có") {
		t.Fatalf("row 4 project = %+v; want RECORD_RELATION_INVALID naming the value", miss)
	}
	// Every bad relation cell of a row is reported, not only the first.
	if e := byKey["customer@4"]; e.Code != "RECORD_RELATION_INVALID" || !strings.Contains(e.Message, "Ai Đó") {
		t.Fatalf("row 4 customer = %+v; want RECORD_RELATION_INVALID too", e)
	}
	// One value lookup per relation field — not one per row.
	if store.matches != 2 {
		t.Fatalf("BatchIDsByFieldValue calls = %d; want 2 (one per relation field)", store.matches)
	}
}

// #360: a name typed decomposed (NFD: base letter + combining mark) names the same record as the
// NFC text stored — the cell is normalised before it is looked up.
func TestImport_RelationNameNFD(t *testing.T) {
	store := dealsStore()
	svc := NewImportService(relDealsCatalog(), store, nil).WithRelationChecker(store)
	nfd := norm.NFD.String("Phú Gia Riverside")
	if nfd == "Phú Gia Riverside" {
		t.Fatal("fixture is not decomposed")
	}
	res := importDeals(t, svc, "Tên,Dự án\nCăn 1,"+nfd+"\n", ImportInput{
		IsAdmin: true, Mapping: map[string]string{"Tên": "name", "Dự án": "project"},
	})
	if res.Inserted != 1 || store.insertedRows[0]["project"] != "p1" {
		t.Fatalf("result = %+v errors = %+v; want NFD name matched to p1", res, res.Errors)
	}
}

// #272: the table's chosen label field is what an import cell names when the maker picked no column.
func TestImport_RelationByTableLabelField(t *testing.T) {
	store := dealsStore()
	cat := relDealsCatalog()
	cat.opts = map[string]string{"projects": `{"labelField":"ma_du_an"}`}
	svc := NewImportService(cat, store, nil).WithRelationChecker(store)
	res := importDeals(t, svc, "Tên,Dự án\nCăn 1,PGR\n", ImportInput{
		IsAdmin: true, Mapping: map[string]string{"Tên": "name", "Dự án": "project"},
	})
	if res.Inserted != 1 || store.insertedRows[0]["project"] != "p1" {
		t.Fatalf("result = %+v rows = %#v; want the code PGR (table label field) resolved to p1", res, store.insertedRows)
	}
}

func TestImport_RelationMatchBy(t *testing.T) {
	t.Run("a chosen unique field", func(t *testing.T) {
		store := dealsStore()
		svc := NewImportService(relDealsCatalog(), store, nil).WithRelationChecker(store)
		res := importDeals(t, svc, "Tên,Dự án\nCăn 1,pgr\nCăn 2,Phú Gia Riverside\n", ImportInput{
			IsAdmin: true, Mapping: map[string]string{"Tên": "name", "Dự án": "project"},
			MatchBy: map[string]string{"project": "ma_du_an"},
		})
		if res.Inserted != 1 || store.insertedRows[0]["project"] != "p1" {
			t.Fatalf("result = %+v rows = %v; want only the code row in", res, store.insertedRows)
		}
		if len(res.Errors) != 1 || res.Errors[0].Row != 2 || !strings.Contains(res.Errors[0].Message, "Mã dự án") {
			t.Fatalf("errors = %+v; want row 2 refused naming the chosen field", res.Errors)
		}
	})
	for name, matchBy := range map[string]map[string]string{
		"unknown target field":  {"project": "khong_co"},
		"not matchable type":    {"project": "gia"},
		"not a relation":        {"name": "ten_du_an"},
		"relation not mapped":   {"customer": "name"},
		"relation to the field": {"project": "ghi_chu') OR 1=1"},
	} {
		t.Run(name, func(t *testing.T) {
			store := dealsStore()
			svc := NewImportService(relDealsCatalog(), store, nil).WithRelationChecker(store)
			_, err := svc.Import(context.Background(), ImportInput{
				TenantID: "t1", UserID: "u1", EntityName: "deals", IsAdmin: true,
				Mapping: map[string]string{"Tên": "name", "Dự án": "project"}, MatchBy: matchBy,
				Reader: strings.NewReader("Tên,Dự án\nCăn 1,p1\n"),
			})
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != "IMPORT_MATCH_FIELD_INVALID" {
				t.Fatalf("err = %v; want IMPORT_MATCH_FIELD_INVALID", err)
			}
			if store.insertCalled {
				t.Fatal("a refused mapping wrote rows")
			}
		})
	}
}

// denyAuthz refuses "view" on one entity.
type denyAuthz struct{ entity string }

func (d denyAuthz) Authorize(_ context.Context, _, _, entity, _ string) (bool, string, error) {
	return entity != d.entity, "all", nil
}

// hidingACL hides one field of one entity from every non-admin.
type hidingACL struct {
	fakeFieldACL
	entity, field string
}

func (h *hidingACL) HiddenFields(_ context.Context, _, _, entity string, isAdmin bool) (map[string]struct{}, error) {
	if isAdmin || entity != h.entity {
		return nil, nil
	}
	return map[string]struct{}{h.field: {}}, nil
}

// FR-015: a caller who may not view the target (or its match field) matches by id only — the
// name of a record they cannot see never resolves, and a hidden field reads as a missing one.
func TestImport_RelationByName_RespectsACL(t *testing.T) {
	csv := "Tên,Dự án\nCăn 1,Phú Gia Riverside\nCăn 2,p2\n"
	mapping := map[string]string{"Tên": "name", "Dự án": "project"}
	check := func(t *testing.T, svc *ImportService, store *matchStore) {
		t.Helper()
		res := importDeals(t, svc, csv, ImportInput{RoleID: "r1", Mapping: mapping})
		if res.Inserted != 1 || store.insertedRows[0]["project"] != "p2" {
			t.Fatalf("result = %+v; want only the id row in", res)
		}
		if len(res.Errors) != 1 || res.Errors[0].Code != "RECORD_RELATION_INVALID" || res.Errors[0].Row != 1 {
			t.Fatalf("errors = %+v; want row 1 refused", res.Errors)
		}
		if store.matches != 0 {
			t.Fatalf("value lookup ran %d times for a caller who may not look", store.matches)
		}
	}
	t.Run("match field hidden", func(t *testing.T) {
		store := dealsStore()
		svc := NewImportService(relDealsCatalog(), store, nil).WithRelationChecker(store).
			WithFieldACL(&hidingACL{entity: "projects", field: "ten_du_an"})
		check(t, svc, store)
	})
	// No VIEW grant on the target: a raw id is no way in either (#331) — it reads as absent, like
	// the name does, and the tenant-wide probe is never reached.
	t.Run("target not viewable: ids refused too", func(t *testing.T) {
		store := dealsStore()
		svc := NewImportService(relDealsCatalog(), store, nil).WithRelationChecker(store).WithACLAuthorizer(denyAuthz{"projects"})
		res := importDeals(t, svc, csv, ImportInput{RoleID: "r1", Mapping: mapping})
		if res.Inserted != 0 || len(res.Errors) != 2 || store.matches != 0 || store.idProbes != 0 {
			t.Fatalf("result = %+v matches=%d tenantWideProbes=%d; want both rows refused, no lookup", res, store.matches, store.idProbes)
		}
		for _, e := range res.Errors {
			if e.Code != "RECORD_RELATION_INVALID" {
				t.Fatalf("errors = %+v; want RECORD_RELATION_INVALID", res.Errors)
			}
		}
	})
	// A view grant narrowed to the caller's own records narrows the lookup too: a colleague's
	// record named by a guessable key reads exactly like a missing one (review MEDIUM).
	t.Run("view owner: only the caller's own records match", func(t *testing.T) {
		store := dealsStore()
		store.owners = map[string]string{"p1": "u1", "p2": "u2", "p3": "u2"}
		svc := NewImportService(relDealsCatalog(), store, nil).WithRelationChecker(store).WithACLAuthorizer(&levelAuthz{level: "owner"})
		res := importDeals(t, svc, "Tên,Dự án\nCăn 1,Phú Gia Riverside\nCăn 2,Sky Garden\n", ImportInput{RoleID: "r1", Mapping: mapping})
		if res.Inserted != 1 || store.insertedRows[0]["project"] != "p1" {
			t.Fatalf("result = %+v rows = %v; want only the caller's own project matched", res, store.insertedRows)
		}
		if len(res.Errors) != 1 || res.Errors[0].Row != 2 || res.Errors[0].Code != "RECORD_RELATION_INVALID" ||
			!strings.Contains(res.Errors[0].Message, "không có") {
			t.Fatalf("errors = %+v; want row 2 refused as not found (never ambiguous: the count is a colleague's)", res.Errors)
		}
		want := dynrepo.RowScope{UserID: "u1", Level: dynrepo.AccessOwner}
		if len(store.scopes) != 1 || store.scopes[0].UserID != want.UserID || store.scopes[0].Level != want.Level || store.scopes[0].IsAdmin {
			t.Fatalf("lookup scopes = %+v; want %+v", store.scopes, want)
		}
	})
	t.Run("hidden field named in matchBy reads as unknown", func(t *testing.T) {
		store := dealsStore()
		svc := NewImportService(relDealsCatalog(), store, nil).WithRelationChecker(store).
			WithFieldACL(&hidingACL{entity: "projects", field: "ma_du_an"})
		_, errHidden := svc.Import(context.Background(), ImportInput{TenantID: "t1", UserID: "u1", RoleID: "r1",
			EntityName: "deals", Mapping: mapping, MatchBy: map[string]string{"project": "ma_du_an"}, Reader: strings.NewReader(csv)})
		_, errMissing := svc.Import(context.Background(), ImportInput{TenantID: "t1", UserID: "u1", RoleID: "r1",
			EntityName: "deals", Mapping: mapping, MatchBy: map[string]string{"project": "khong_co"}, Reader: strings.NewReader(csv)})
		var a, b *ValidationError
		if !errors.As(errHidden, &a) || !errors.As(errMissing, &b) || a.Code != b.Code || a.Msg != strings.ReplaceAll(b.Msg, "khong_co", "ma_du_an") {
			t.Fatalf("hidden = %v, missing = %v; want the same refusal", errHidden, errMissing)
		}
	})
}

func TestDefaultRelationMatchField(t *testing.T) {
	text := func(name string, required bool) FieldSpec {
		return FieldSpec{Name: name, FieldType: "text", IsRequired: required}
	}
	tests := []struct {
		name   string
		rel    FieldSpec
		target []FieldSpec
		want   string
	}{
		{"labelField of the relation", FieldSpec{Options: map[string]any{"labelField": "title"}},
			[]FieldSpec{text("name", true), text("title", false)}, "title"},
		{"labelField that no longer exists", FieldSpec{Options: map[string]any{"labelField": "gone"}},
			[]FieldSpec{text("name", false)}, "name"},
		{"name", FieldSpec{}, []FieldSpec{text("code", true), text("name", false)}, "name"},
		{"first required text", FieldSpec{}, []FieldSpec{text("ghi_chu", false), text("ho_ten", true)}, "ho_ten"},
		{"first text", FieldSpec{}, []FieldSpec{{Name: "n", FieldType: "number"}, text("mo_ta", false)}, "mo_ta"},
		{"none", FieldSpec{}, []FieldSpec{{Name: "n", FieldType: "number"}}, ""},
		// #479: the same unlabelled-table default as ?expand / record titles (shared resolver).
		{"title like the record title", FieldSpec{}, []FieldSpec{text("code", true), text("title", false)}, "title"},
		{"email when nothing else", FieldSpec{}, []FieldSpec{{Name: "n", FieldType: "number"}, {Name: "mail", FieldType: "email"}}, "mail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := defaultRelationMatchField(tt.rel, tt.target); got != tt.want {
				t.Fatalf("defaultRelationMatchField = %q; want %q", got, tt.want)
			}
		})
	}
}

// The value lookup is typed on the concrete repository the server wires.
var _ relationValueMatcher = (*dynrepo.Repository)(nil)

// A relation the file does not map but whose field has a default (an id) is still stored and still
// FK-checked: the default fills every row, and it must resolve like an id cell would (review HIGH).
func TestImport_UnmappedRelationDefault(t *testing.T) {
	for _, tt := range []struct {
		name, def    string
		wantInserted int
	}{
		{"default names a live record", "p1", 2},
		{"default names no record", "p9", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cat := relDealsCatalog()
			cat.fields["deals"][1].DefaultVal = tt.def
			store := dealsStore()
			svc := NewImportService(cat, store, nil).WithRelationChecker(store)
			res := importDeals(t, svc, "Tên\nCăn 1\nCăn 2\n", ImportInput{IsAdmin: true, Mapping: map[string]string{"Tên": "name"}})
			if res.Inserted != tt.wantInserted {
				t.Fatalf("result = %+v; want %d inserted", res, tt.wantInserted)
			}
			for _, r := range store.insertedRows {
				if r["project"] != tt.def {
					t.Fatalf("stored = %v; want project = default %q", r, tt.def)
				}
			}
			for _, e := range res.Errors {
				if e.Code != "RECORD_RELATION_INVALID" || e.Field != "project" || !strings.Contains(e.Message, tt.def) {
					t.Fatalf("errors = %+v; want RECORD_RELATION_INVALID naming the default", res.Errors)
				}
			}
			if store.matches != 0 {
				t.Fatalf("value lookup ran %d times for a column the file does not have", store.matches)
			}
		})
	}
}

// A relation check that fails refuses the row it could not check: never inserted with an unchecked
// FK, never with the target's NAME stored in the FK column (review MEDIUM — both were mutations no
// test caught). The row without a relation cell still goes in.
func TestImport_RelationLookupError_RefusesRow(t *testing.T) {
	for name, fail := range map[string]func(*matchStore){
		"id check fails":     func(m *matchStore) { m.existsErr = errors.New("db down") },
		"value lookup fails": func(m *matchStore) { m.matchErr = errors.New("db down") },
	} {
		t.Run(name, func(t *testing.T) {
			store := dealsStore()
			fail(store)
			svc := NewImportService(relDealsCatalog(), store, nil).WithRelationChecker(store)
			res := importDeals(t, svc, "Tên,Dự án\nCăn 1,Phú Gia Riverside\nCăn 2,\n",
				ImportInput{IsAdmin: true, Mapping: map[string]string{"Tên": "name", "Dự án": "project"}})
			if res.Inserted != 1 || len(store.insertedRows) != 1 || store.insertedRows[0]["name"] != "Căn 2" {
				t.Fatalf("result = %+v rows = %v; want only the row without a relation cell in", res, store.insertedRows)
			}
			if _, has := store.insertedRows[0]["project"]; has {
				t.Fatalf("stored = %v; want no project on the row that had none", store.insertedRows[0])
			}
			if len(res.Errors) != 1 || res.Errors[0].Row != 1 || res.Errors[0].Field != "project" || res.Errors[0].Code != "IMPORT_ERROR" {
				t.Fatalf("errors = %+v; want row 1 refused IMPORT_ERROR on project", res.Errors)
			}
		})
	}
}

// FR-008: a header written "Dự án " (Excel keeps the space) maps like "Dự án" — the screen sends the
// trimmed header, an API client may send it as written; both find the column (review MEDIUM).
func TestImport_PaddedHeaderStillMapped(t *testing.T) {
	for name, mapping := range map[string]map[string]string{
		"trimmed key (the screen)": {"Tên": "name", "Dự án": "project"},
		"key as written":           {"Tên ": "name", "Dự án ": "project"},
	} {
		t.Run(name, func(t *testing.T) {
			store := dealsStore()
			svc := NewImportService(relDealsCatalog(), store, nil).WithRelationChecker(store)
			res := importDeals(t, svc, "Tên ,Dự án \nCăn 1,p1\n", ImportInput{IsAdmin: true, Mapping: mapping})
			if res.Inserted != 1 || store.insertedRows[0]["name"] != "Căn 1" || store.insertedRows[0]["project"] != "p1" {
				t.Fatalf("result = %+v rows = %v; want the padded columns mapped", res, store.insertedRows)
			}
		})
	}
}

// #331 — a relation cell written as an id is accepted only if the importer may VIEW that record,
// exactly like POST/PUT (RecordService.relationScopeFor). The import path writes through the store,
// not RecordService.Create, so it carries its own guard.
func TestImport_RelationByID_OnlyRecordsTheCallerMayView(t *testing.T) {
	mapping := map[string]string{"Tên": "name", "Dự án": "project"}
	owned := func() *matchStore {
		store := dealsStore()
		store.owners = map[string]string{"p1": "u1", "p2": "u2"}
		return store
	}
	t.Run("view owner: a colleague's id reads like a missing id", func(t *testing.T) {
		store := owned()
		svc := NewImportService(relDealsCatalog(), store, nil).WithRelationChecker(store).WithACLAuthorizer(&levelAuthz{level: "owner"})
		res := importDeals(t, svc, "Tên,Dự án\nCăn 1,p1\nCăn 2,p2\nCăn 3,khong-co\n", ImportInput{RoleID: "r1", Mapping: mapping})
		if res.Inserted != 1 || store.insertedRows[0]["project"] != "p1" {
			t.Fatalf("result = %+v rows = %v; want only the caller's own project accepted", res, store.insertedRows)
		}
		if len(res.Errors) != 2 || res.Errors[0].Row != 2 || res.Errors[1].Row != 3 ||
			res.Errors[0].Code != "RECORD_RELATION_INVALID" || res.Errors[1].Code != "RECORD_RELATION_INVALID" {
			t.Fatalf("errors = %+v; want rows 2 and 3 refused", res.Errors)
		}
		// hidden and missing answer with ONE shape: no oracle on which ids are real records
		hidden := strings.ReplaceAll(res.Errors[0].Message, "p2", "X")
		missing := strings.ReplaceAll(res.Errors[1].Message, "khong-co", "X")
		if hidden != missing {
			t.Fatalf("hidden id says %q, missing id says %q; want the same sentence", hidden, missing)
		}
		if store.idProbes != 0 || len(store.idScopes) == 0 {
			t.Fatalf("tenantWideProbes=%d scopedProbes=%d; want only the scoped probe", store.idProbes, len(store.idScopes))
		}
		got := store.idScopes[0]
		if got.UserID != "u1" || got.Level != dynrepo.AccessOwner || got.IsAdmin || !got.IncludeShares {
			t.Fatalf("id probe scope = %+v; want owner level for u1 with manual shares (GetRecord's predicate)", got)
		}
	})
	t.Run("view all: any id of the tenant", func(t *testing.T) {
		store := owned()
		svc := NewImportService(relDealsCatalog(), store, nil).WithRelationChecker(store).WithACLAuthorizer(&levelAuthz{level: "all"})
		res := importDeals(t, svc, "Tên,Dự án\nCăn 2,p2\n", ImportInput{RoleID: "r1", Mapping: mapping})
		if res.Inserted != 1 || len(res.Errors) != 0 {
			t.Fatalf("result = %+v; want the id accepted", res)
		}
	})
	t.Run("admin keeps the tenant-wide probe", func(t *testing.T) {
		store := owned()
		svc := NewImportService(relDealsCatalog(), store, nil).WithRelationChecker(store).WithACLAuthorizer(&levelAuthz{level: "owner"})
		res := importDeals(t, svc, "Tên,Dự án\nCăn 2,p2\n", ImportInput{RoleID: "r1", IsAdmin: true, Mapping: mapping})
		if res.Inserted != 1 || len(res.Errors) != 0 || len(store.idScopes) != 0 {
			t.Fatalf("result = %+v scopedProbes=%d; want admin unchanged", res, len(store.idScopes))
		}
	})
	t.Run("a checker that cannot probe in a scope fails closed for a non-admin", func(t *testing.T) {
		store := owned()
		svc := NewImportService(relDealsCatalog(), store, nil).WithRelationChecker(tenantWideOnly{store}).WithACLAuthorizer(&levelAuthz{level: "owner"})
		res := importDeals(t, svc, "Tên,Dự án\nCăn 2,p2\n", ImportInput{RoleID: "r1", Mapping: mapping})
		if res.Inserted != 0 || len(res.Errors) != 1 || res.Errors[0].Code != "IMPORT_ERROR" {
			t.Fatalf("result = %+v; want IMPORT_ERROR, never a widened probe", res)
		}
	})
}

// tenantWideOnly hides every probe but BatchRecordExists, like a decorator that dropped the scoped one.
type tenantWideOnly struct{ s *matchStore }

func (t tenantWideOnly) BatchRecordExists(ctx context.Context, entity, tenantID string, ids []string) (map[string]bool, error) {
	return t.s.BatchRecordExists(ctx, entity, tenantID, ids)
}
