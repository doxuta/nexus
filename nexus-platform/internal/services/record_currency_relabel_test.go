package services

// Queue #63 — a legacy record whose stored currency code differs from the field's configured code
// must NOT be silently relabelled by a save.
//
// Rows written before the Q7 write guard (queue #7) can hold {amount:100, currencyCode:"USD"} in a
// VND-configured field. The edit form never shows the stored code: reverseValue keeps only the
// amount, and buildPayload stamps the CONFIGURED code on the way out — and the form resends every
// field on update. So "open an old deal, fix a typo in its name, save" turned USD 100 into VND 100:
// a different amount of money under the same number, with nobody told. The Q7 guard cannot see it
// (the payload's code equals the configured code), so the rule needs the stored value.

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// relabelStore reports what the record holds today and captures what the update wrote.
type relabelStore struct {
	fakeRecordStore
	current  map[string]any // nil ⇒ GetRecord fails (the best-effort read degraded)
	updated  bool
	gotWrite map[string]any
}

func (r *relabelStore) GetRecord(_ context.Context, _, _, _ string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	if r.current == nil {
		return nil, dynrepo.ErrRecordNotFound
	}
	return &dynrepo.Record{ID: "rec-1", Data: r.current}, nil
}

func (r *relabelStore) UpdateRecord(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, data map[string]any, _ *int, _ []dynrepo.UniqueFieldSpec) error {
	r.updated = true
	r.gotWrite = data
	return nil
}

var vndOpts = `{"currencyCode":"VND"}`

func newRelabelService(store *relabelStore) *RecordService {
	cat := &fakeRecordCatalog{
		entity: courseEntityFixture(),
		fields: append(courseFieldDefs(), metadata.FieldDefinition{
			Name: "price", FieldType: "currency", Options: &vndOpts,
		}),
	}
	return NewRecordService(cat, store)
}

func money(amount float64, code string) map[string]any {
	return map[string]any{"amount": amount, "currencyCode": code}
}

func TestUpdate_CurrencyLegacyMismatch(t *testing.T) {
	admin := RecordScope{IsAdmin: true, AccessLevel: "all"}
	tests := []struct {
		name    string
		current map[string]any // stored record; nil = the read failed
		payload map[string]any
		// wantPrice: the currency value the store must receive; nil = the key must be ABSENT
		// (left untouched by JSON_MERGE_PATCH, i.e. the stored value survives verbatim).
		wantPrice map[string]any
		wantTitle string
	}{
		{
			// The exact LayoutForm payload: every field resent, the configured code stamped over
			// the stored USD, amount untouched, only the name edited. Before the fix the store
			// received VND 100 — the silent relabel.
			name:      "form resend over a legacy USD value keeps the stored value",
			current:   map[string]any{"title": "cũ", "price": money(100, "USD")},
			payload:   map[string]any{"title": "mới", "price": money(100, "VND")},
			wantPrice: nil,
			wantTitle: "mới",
		},
		{
			// A different amount is the caller's own edit: the user typed a new number into a
			// field labelled VND, so it is written with the configured code.
			name:      "a changed amount over a legacy value is a real edit",
			current:   map[string]any{"title": "cũ", "price": money(100, "USD")},
			payload:   map[string]any{"title": "cũ", "price": money(2500000, "VND")},
			wantPrice: money(2500000, "VND"),
			wantTitle: "cũ",
		},
		{
			// A clean record is untouched by the rule: the resend is written as before.
			name:      "stored code matches the configured code: normal path",
			current:   map[string]any{"title": "cũ", "price": money(100, "VND")},
			payload:   map[string]any{"title": "mới", "price": money(100, "VND")},
			wantPrice: money(100, "VND"),
			wantTitle: "mới",
		},
		{
			// No stored code to compare against (never set, or a bare legacy number): nothing is
			// being relabelled, so the rule does not fire.
			name:      "stored value without a code: rule does not fire",
			current:   map[string]any{"title": "cũ", "price": 100.0},
			payload:   map[string]any{"title": "mới", "price": money(100, "VND")},
			wantPrice: money(100, "VND"),
			wantTitle: "mới",
		},
		{
			// The best-effort current read failed: no stored value to compare against, so the
			// payload is written as sent — the behaviour that existed before this rule (the same
			// degrade the relation and field-ACL unchanged-value rules use).
			name:      "current read failed: payload written as sent",
			current:   nil,
			payload:   map[string]any{"title": "mới", "price": money(100, "VND")},
			wantPrice: money(100, "VND"),
			wantTitle: "mới",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &relabelStore{current: tc.current}
			svc := newRelabelService(store)
			if err := svc.Update(context.Background(), admin, "tenant-1", "courses", "rec-1", tc.payload, nil); err != nil {
				t.Fatalf("Update: %v", err)
			}
			if !store.updated {
				t.Fatal("the update never reached the store")
			}
			if got := store.gotWrite["title"]; got != tc.wantTitle {
				t.Errorf("title written = %v; want %q (the rest of the save must go through)", got, tc.wantTitle)
			}
			got, present := store.gotWrite["price"]
			if tc.wantPrice == nil {
				if present {
					t.Fatalf("price written = %v; want the key ABSENT so the stored USD 100 survives — a same-amount resend with the configured code is a relabel", got)
				}
				return
			}
			m, ok := got.(map[string]any)
			if !ok || m["amount"] != tc.wantPrice["amount"] || m["currencyCode"] != tc.wantPrice["currencyCode"] {
				t.Fatalf("price written = %v (present=%v); want %v", got, present, tc.wantPrice)
			}
		})
	}
}

// The payload must not be mutated in place when the relabel is dropped — callers (mass edit,
// inbound hooks) reuse the map they passed in.
func TestUpdate_CurrencyLegacyMismatch_DoesNotMutateCallerPayload(t *testing.T) {
	store := &relabelStore{current: map[string]any{"price": money(100, "USD")}}
	svc := newRelabelService(store)
	payload := map[string]any{"price": money(100, "VND"), "title": "x"}
	if err := svc.Update(context.Background(), RecordScope{IsAdmin: true, AccessLevel: "all"}, "tenant-1", "courses", "rec-1", payload, nil); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, ok := payload["price"]; !ok {
		t.Error("the caller's payload lost its price key")
	}
}
