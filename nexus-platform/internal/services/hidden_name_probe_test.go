package services

// Queue #317 (hidden-field-name-probe-oracle, api-conventions §4.4b): a non-admin must not be able
// to tell "this field exists but is hidden from me" from "there is no such field". Sending the
// EXACT name of a hidden field used to answer 403 FIELD_FORBIDDEN naming it, while a made-up name
// answered 422 RECORD_UNKNOWN_FIELD / LIST_UNKNOWN_FILTER / SORT_NOT_ALLOWED — a free existence
// oracle for anyone who can guess "salary" or "cost_price".
//
// Every door is driven twice through the REAL FieldACLService — once with the hidden field's name,
// once with a name that does not exist — and the two refusals must be the same error once the
// probed name itself is masked: same code, same message template, same details, same field.

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

const (
	probeHidden  = "secret"    // exists, hidden from the role
	probeUnknown = "zzz_nope"  // does not exist
	probeVisible = "notes"     // exists, visible
	probeRoleLoc = "locked_ro" // exists, visible, read-only
)

// probeShape renders an error with the probed name masked, so two refusals compare equal exactly
// when a caller could not tell them apart.
func probeShape(err error, probed string) string {
	if err == nil {
		return "<nil>"
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		return "non-validation: " + strings.ReplaceAll(err.Error(), probed, "<K>")
	}
	b, _ := json.Marshal(map[string]any{"code": ve.Code, "msg": ve.Msg, "field": ve.Field, "details": ve.Details})
	return strings.ReplaceAll(string(b), probed, "<K>")
}

func probeCatalog() *rjCatalog {
	return &rjCatalog{
		entities: map[string]*metadata.EntityDefinition{"deals": ent("deals")},
		fields: map[string][]metadata.FieldDefinition{"deals": {
			{Name: "name", FieldType: "text"},
			{Name: probeVisible, FieldType: "text"},
			{Name: probeHidden, FieldType: "text", IsUnique: true}, // unique ⇒ sortable, so the sort door is reachable
			{Name: probeRoleLoc, FieldType: "text"},
		}},
	}
}

func probeService(store RecordStore) *RecordService {
	return NewRecordService(probeCatalog(), store).WithFieldACL(NewFieldACLService(&fakeFieldACLLoader{
		rows: []auth.FieldACL{hide(probeHidden), lock(probeRoleLoc)},
	}))
}

func probeRole() RecordScope {
	return RecordScope{UserID: "u1", RoleID: flsRole, AccessLevel: "all"}
}

// probeDoors lists every surface a caller can name a field on. run(key) performs the call with
// that name and returns the refusal.
func probeDoors(t *testing.T) map[string]func(key string) error {
	t.Helper()
	ctx := context.Background()
	store := func() *fakeComputedStore {
		rec := dynrepo.Record{ID: "d1", Data: map[string]any{"name": "Deal"}}
		return &fakeComputedStore{record: &rec, records: []dynrepo.Record{rec}}
	}
	return map[string]func(string) error{
		"create": func(key string) error {
			_, err := probeService(store()).Create(ctx, probeRole(), "t1", "u1", "deals", map[string]any{key: "x"})
			return err
		},
		"create with null": func(key string) error {
			_, err := probeService(store()).Create(ctx, probeRole(), "t1", "u1", "deals", map[string]any{key: nil})
			return err
		},
		"update": func(key string) error {
			return probeService(store()).Update(ctx, probeRole(), "t1", "deals", "d1", map[string]any{key: "x"}, nil)
		},
		"mass edit row": func(key string) error {
			// MassEdit is Update per row; the guard lives in Update, so this is the same door the
			// bulk-edit endpoint takes.
			return probeService(store()).Update(ctx, probeRole(), "t1", "deals", "d1", map[string]any{key: "x", "name": "n"}, nil)
		},
		"list filter": func(key string) error {
			_, err := probeService(store()).List(ctx, probeRole(), "t1", "deals", ListQuery{Page: 1, Limit: 10,
				Filters: []FilterInput{{Field: key, Values: []string{"x"}}}})
			return err
		},
		"list filter with an operator the field type rejects": func(key string) error {
			_, err := probeService(store()).List(ctx, probeRole(), "t1", "deals", ListQuery{Page: 1, Limit: 10,
				Filters: []FilterInput{{Field: key, Operator: dynrepo.FilterGt, Values: []string{"x"}}}})
			return err
		},
		"list sort": func(key string) error {
			_, err := probeService(store()).List(ctx, probeRole(), "t1", "deals", ListQuery{Page: 1, Limit: 10, Sort: key})
			return err
		},
		"list cursor sort": func(key string) error {
			_, err := probeService(store()).List(ctx, probeRole(), "t1", "deals", ListQuery{Limit: 10, Paginate: "cursor", Sort: key})
			return err
		},
		"aggregate filter": func(key string) error {
			_, err := probeService(store()).Aggregate(ctx, probeRole(), "t1", "deals", AggregateQuery{Metric: "count",
				Filters: []FilterInput{{Field: key, Values: []string{"x"}}}})
			return err
		},
		"export filter": func(key string) error {
			var buf bytes.Buffer
			return probeService(store()).ExportCSV(ctx, probeRole(), "t1", "deals", ExportCSVOptions{
				Filters: []FilterInput{{Field: key, Values: []string{"x"}}}}, &buf)
		},
	}
}

func TestHiddenFieldName_IsIndistinguishableFromUnknown(t *testing.T) {
	for name, run := range probeDoors(t) {
		t.Run(name, func(t *testing.T) {
			hidden, unknown := run(probeHidden), run(probeUnknown)
			if unknown == nil {
				t.Fatalf("door %q accepted an unknown name %q — the probe is not exercising a refusal", name, probeUnknown)
			}
			if h, u := probeShape(hidden, probeHidden), probeShape(unknown, probeUnknown); h != u {
				t.Fatalf("a hidden field is distinguishable from a missing one:\n hidden : %s\n unknown: %s", h, u)
			}
		})
	}
}

// The oracle fix must not widen what a caller may do: a visible read-only field is still refused
// with the explicit 403 (the caller can see it exists), and an admin still gets the plain answer.
func TestHiddenFieldName_VisibleLockedStaysForbidden_AdminUnaffected(t *testing.T) {
	ctx := context.Background()
	rec := dynrepo.Record{ID: "d1", Data: map[string]any{"name": "Deal"}}
	store := &fakeComputedStore{record: &rec, records: []dynrepo.Record{rec}}

	_, err := probeService(store).Create(ctx, probeRole(), "t1", "u1", "deals", map[string]any{probeRoleLoc: "x"})
	if got := codeOf(err); got != "FIELD_FORBIDDEN" {
		t.Fatalf("visible read-only field: code = %q, want FIELD_FORBIDDEN (err=%v)", got, err)
	}

	admin := RecordScope{UserID: "u1", RoleID: flsRole, AccessLevel: "all", IsAdmin: true}
	if _, err := probeService(store).Create(ctx, admin, "t1", "u1", "deals", map[string]any{probeHidden: "x"}); err != nil {
		t.Fatalf("admin writing the field must succeed: %v", err)
	}
	_, err = probeService(store).List(ctx, admin, "t1", "deals", ListQuery{Page: 1, Limit: 10,
		Filters: []FilterInput{{Field: probeHidden, Values: []string{"x"}}}})
	if err != nil {
		t.Fatalf("admin filtering by the field must succeed: %v", err)
	}
}

// A whole-entity export must not list a hidden field as a column either — it used to appear headed
// by its bare name over empty cells, which names a field nothing else serves this role. A formula
// derived from the hidden field goes with it (queue #316). Admin keeps every column.
func TestExportCSV_WholeEntityOmitsHiddenColumns(t *testing.T) {
	cat := &rjCatalog{
		entities: map[string]*metadata.EntityDefinition{"deals": ent("deals")},
		fields: map[string][]metadata.FieldDefinition{"deals": {
			{Name: "name", FieldType: "text"},
			{Name: "amount", FieldType: "number"},
			{Name: "probability", FieldType: "number"},
			{Name: "weighted", FieldType: "formula", Options: strPtr(`{"formula":"amount * probability / 100","resultType":"number"}`)},
		}},
	}
	header := func(scope RecordScope) []string {
		svc := NewRecordService(cat, &exportStore{rjStore: &rjStore{}}).
			WithFieldACL(NewFieldACLService(&fakeFieldACLLoader{rows: []auth.FieldACL{hide("amount")}}).WithCatalog(cat))
		var buf bytes.Buffer
		if err := svc.ExportCSV(context.Background(), scope, "t1", "deals", ExportCSVOptions{}, &buf); err != nil {
			t.Fatalf("ExportCSV: %v", err)
		}
		rows, err := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
		if err != nil || len(rows) == 0 {
			t.Fatalf("parse CSV: %v (out=%q)", err, buf.String())
		}
		return rows[0]
	}
	got := strings.Join(header(probeRole()), ",")
	for _, leaked := range []string{"amount", "weighted"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("role header names hidden/derived field %q: %s", leaked, got)
		}
	}
	if !strings.Contains(got, "probability") {
		t.Fatalf("visible columns must stay: %s", got)
	}
	admin := strings.Join(header(RecordScope{UserID: "u1", RoleID: flsRole, AccessLevel: "all", IsAdmin: true}), ",")
	if !strings.Contains(admin, "amount") || !strings.Contains(admin, "weighted") {
		t.Fatalf("admin must keep every column: %s", admin)
	}
}

// Update keeps its documented rule: a null for a hidden field is dropped (the role never saw the
// value, so null is "nothing", not "clear") — not refused and not written.
func TestHiddenFieldName_UpdateNullIsStillDropped(t *testing.T) {
	rec := dynrepo.Record{ID: "d1", Data: map[string]any{"name": "Deal", probeHidden: "stored"}}
	store := &fakeComputedStore{record: &rec}
	payload := map[string]any{probeHidden: nil, "name": "n2"}
	if err := probeService(store).Update(context.Background(), probeRole(), "t1", "deals", "d1", payload, nil); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, kept := payload[probeHidden]; kept {
		t.Fatalf("hidden null must be dropped from the payload: %v", payload)
	}
}

// CSV import: the column MAPPING names the field. A mapping to a hidden field must read exactly
// like a mapping to a field that does not exist.
func TestHiddenFieldName_ImportMapping(t *testing.T) {
	run := func(key string) error {
		svc, _ := newImportSvcForTest()
		svc.WithFieldACL(NewFieldACLService(&fakeFieldACLLoader{rows: []auth.FieldACL{hide("secret")}}))
		csv := "col_name,col_x\nAlice,v\n"
		_, err := svc.Import(context.Background(), ImportInput{
			TenantID: "t1", UserID: "u1", RoleID: flsRole, EntityName: "contacts",
			Mapping: map[string]string{"col_name": "name", "col_x": key},
			Reader:  strings.NewReader(csv), FileSize: int64(len(csv)),
		})
		return err
	}
	hidden, unknown := run("secret"), run(probeUnknown)
	if unknown == nil {
		t.Fatal("an unknown mapping target must be refused")
	}
	if h, u := probeShape(hidden, "secret"), probeShape(unknown, probeUnknown); h != u {
		t.Fatalf("import mapping distinguishes hidden from unknown:\n hidden : %s\n unknown: %s", h, u)
	}
}

// Bulk create runs the same guard per row; the row's code/error/details are what the caller reads.
func TestHiddenFieldName_BulkRow(t *testing.T) {
	rowShape := func(key string) string {
		svc := NewRecordService(probeCatalog(), &bulkTestStore{}).
			WithSequenceClaimer(&fakeSequenceClaimer{}).WithBulkInserter(&fakeBulkInserter{}).
			WithFieldACL(NewFieldACLService(&fakeFieldACLLoader{rows: []auth.FieldACL{hide(probeHidden)}}))
		res, err := svc.CreateBulk(context.Background(), probeRole(), "t1", "u1", "deals",
			[]map[string]any{{key: "x"}}, BulkModePartial)
		if err != nil {
			t.Fatalf("CreateBulk: %v", err)
		}
		if len(res.Results) != 1 || res.Results[0].Status != "failed" {
			t.Fatalf("want one failed row, got %+v", res.Results)
		}
		b, _ := json.Marshal(res.Results[0])
		return strings.ReplaceAll(string(b), key, "<K>")
	}
	if h, u := rowShape(probeHidden), rowShape(probeUnknown); h != u {
		t.Fatalf("bulk row distinguishes hidden from unknown:\n hidden : %s\n unknown: %s", h, u)
	}
}
