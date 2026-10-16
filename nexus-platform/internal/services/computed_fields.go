package services

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/formula"
)

// formulaDefaultCurrency is the currency code a currency-resultType formula without
// options.currencyCode is written with — shared with rollupAggCurrency so a rollup over such a
// formula reports the same code its children store.
const formulaDefaultCurrency = "USD"

// applyComputedFields evaluates every formula field in specs against the provided data map
// and writes the computed result back into data. It must be called AFTER the record's
// non-formula fields have been validated (so data contains the final typed values) and
// BEFORE the record is persisted (InsertRecord / UpdateRecord).
//
// Calling conventions by write path:
//
//   - Create: pass the full cleaned payload from ValidateRecord. data contains every
//     non-formula field the client submitted (plus defaults).
//
//   - Update (partial PUT): pass the MERGED image (before-image ⊕ clean patch) so that
//     a PUT that only changes one field still recomputes using the other fields' stored
//     values. The caller (RecordService.Update) must construct the merged map first.
//
//   - Import (processRows): pass the cleaned full-row payload from ValidateRecord, same
//     as Create.
//
// Resolution rules:
//   - number field → value as float64; missing/null → 0.
//   - currency field → reads the ".amount" sub-value; missing/null → 0.
//   - formula results:
//     resultType "currency" → writes {amount: result, currencyCode: ...}
//     resultType "number" (default) → writes result as float64
//     resultType "text" → writes the chosen text as a string (specs/053)
//   - Division by zero → writes nil (null) for the formula field (never blocks the write).
//   - Any other error (bad expression, unexpected) → skip silently; never block the write.
//
// This function never returns an error — formula failures are always silent and non-blocking.
func applyComputedFields(specs []FieldSpec, data map[string]any) {
	// Build a resolver that reads the current data map, interpreting field types correctly.
	resolve := func(name string) float64 {
		// Find the field spec to determine its type.
		for _, sp := range specs {
			if sp.Name != name {
				continue
			}
			v, _ := data[name]
			switch sp.FieldType {
			case "currency":
				// Currency is stored as {amount: float64, currencyCode: "XXX"}.
				obj, ok := v.(map[string]any)
				if !ok {
					return 0
				}
				amt, _ := obj["amount"].(float64)
				return amt
			case "number", "rating":
				n, _ := v.(float64)
				return n
			default:
				// Non-numeric field referenced in formula — treated as 0.
				// (Config-time validation should have rejected this, but be safe.)
				return 0
			}
		}
		// Field not found in specs → treat as 0.
		return 0
	}

	for _, sp := range specs {
		if sp.FieldType != "formula" {
			continue
		}
		if expr, _ := sp.Options["formula"].(string); expr == "" {
			continue
		}
		// queue #284: a formula over a lookup/rollup has no operands in the stored record —
		// applyDerivedFormulas computes it at read; storing a 0 here would be a wrong number.
		if len(derivedFormula(sp, specs)) > 0 {
			continue
		}
		data[sp.Name] = evalFormulaSpec(sp, resolve, textValues(data))
	}
}

// evalFormulaSpec evaluates one formula field to the value stored/served for it, by resultType
// (default "number"): text → the chosen string; currency → {amount, currencyCode}; number → float.
// Division by zero or a bad expression gives nil — never blocks a write or a read.
func evalFormulaSpec(sp FieldSpec, resolve func(string) float64, txt func(string) string) any {
	expr, _ := sp.Options["formula"].(string)
	resultType, _ := sp.Options["resultType"].(string)
	if resultType == "text" {
		text, divByZero, err := formula.EvaluateTextWith(expr, resolve, txt)
		if err != nil || divByZero {
			return nil
		}
		return text
	}
	result, divByZero, err := formula.EvaluateWith(expr, resolve, txt)
	if err != nil || divByZero {
		return nil
	}
	if resultType == "currency" {
		return map[string]any{"amount": result, "currencyCode": formulaCurrencyCode(sp.Options)}
	}
	return result
}

// textValues reads a picklist/text field's stored string for a formula's text comparison
// (queue #235); anything that is not a string reads as "".
func textValues(data map[string]any) func(string) string {
	return func(name string) string {
		s, _ := data[name].(string)
		return s
	}
}

// formulaCurrencyCode is the ISO code a currency-result formula stores: options.currencyCode, or
// formulaDefaultCurrency when unset. The aggregate engine reports the same code
// (resolveMetricField), so a report summing the formula can never label its total with a currency
// the rows do not carry.
func formulaCurrencyCode(opts map[string]any) string {
	if c, _ := opts["currencyCode"].(string); c != "" {
		return c
	}
	return formulaDefaultCurrency
}

// formulaRowStore is what the stored-formula sweep needs from the record store: a keyset page of a
// tenant's live rows, a re-read of one row, and a version-guarded write of derived values that is
// not an edit. *dynrepo.Repository satisfies it (pinned below); FieldService picks it up from its
// schema repo, so every AddField/EditField caller — Studio, AI Builder, pack install — gets it.
type formulaRowStore interface {
	ListRecordsCursor(ctx context.Context, entity, tenantID string, scope dynrepo.RowScope, p dynrepo.CursorParams) ([]dynrepo.Record, string, error)
	GetRecord(ctx context.Context, entity, tenantID, id string, scope dynrepo.RowScope) (*dynrepo.Record, error)
	SetComputedValues(ctx context.Context, entity, tenantID, id string, version int, values map[string]any) (bool, error)
}

var _ formulaRowStore = (*dynrepo.Repository)(nil)

// formulaSweepPage is one keyset page of the sweep (ListRecordsCursor caps a page at 100).
const formulaSweepPage = 100

// formulaSweepAttempts bounds the re-reads of one row that keeps changing under the sweep.
const formulaSweepAttempts = 3

// recomputeStoredFormulas re-evaluates every formula in specs on each live row of one tenant's
// entity and writes back the results that differ from what is stored. A formula is computed and
// STORED at write time (applyComputedFields), so without this a formula added to — or edited on —
// an entity that already holds rows leaves those rows empty or stale, and every report, widget and
// ⌘K total over it is silently wrong (review finding on specs/070 #264). Returns the rows written.
//
// The system sweep sees every row of the tenant (admin scope; the tenant_id predicate stays).
// Rows whose stored values already match are not written, so re-running it is cheap and idempotent.
//
// ponytail: synchronous, one keyset page at a time and one UPDATE per changed row — fine for the
// tens of thousands of rows a customer entity holds today; a background job with progress is the
// upgrade path if an entity outgrows the admin request. Soft-deleted rows keep their stored value
// until their next save.
func recomputeStoredFormulas(ctx context.Context, store formulaRowStore, tenantID, entity string, specs []FieldSpec) (int, error) {
	hasFormula := false
	for _, sp := range specs {
		hasFormula = hasFormula || sp.FieldType == "formula"
	}
	if !hasFormula {
		return 0, nil
	}
	system := dynrepo.RowScope{IsAdmin: true}
	written := 0
	after := ""
	for {
		page, next, err := store.ListRecordsCursor(ctx, entity, tenantID, system, dynrepo.CursorParams{
			Limit: formulaSweepPage, After: after, Sort: dynrepo.DefaultCursorSort, Order: "ASC",
		})
		if err != nil {
			return written, fmt.Errorf("services.recomputeStoredFormulas: list %s: %w", entity, err)
		}
		for _, rec := range page {
			ok, err := refreshRowFormulas(ctx, store, system, tenantID, entity, specs, rec)
			if err != nil {
				return written, fmt.Errorf("services.recomputeStoredFormulas: %s %s: %w", entity, rec.ID, err)
			}
			if ok {
				written++
			}
		}
		if next == "" {
			return written, nil
		}
		after = next
	}
}

// refreshRowFormulas writes one row's changed formula results, re-reading it when a concurrent
// save moved its version first (that save may carry inputs the stale read does not).
func refreshRowFormulas(ctx context.Context, store formulaRowStore, scope dynrepo.RowScope, tenantID, entity string, specs []FieldSpec, rec dynrepo.Record) (bool, error) {
	for attempt := 0; attempt < formulaSweepAttempts; attempt++ {
		diff := formulaDiff(specs, rec.Data)
		if len(diff) == 0 {
			return false, nil
		}
		ok, err := store.SetComputedValues(ctx, entity, tenantID, rec.ID, rec.Version, diff)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
		fresh, err := store.GetRecord(ctx, entity, tenantID, rec.ID, scope)
		if errors.Is(err, dynrepo.ErrRecordNotFound) {
			return false, nil // deleted meanwhile
		}
		if err != nil {
			return false, err
		}
		if fresh.Version == rec.Version {
			// Matched but nothing changed (a driver without clientFoundRows counts changed rows
			// only): the merge already left the row as close to the result as it can.
			return false, nil
		}
		rec = *fresh
	}
	return false, fmt.Errorf("row kept changing during the formula recompute")
}

// formulaDiff evaluates the formulas on a copy of data and returns the results that differ from
// the stored ones. A nil result (division by zero, bad input) equals an absent key: both read as
// "no value", and JSON_MERGE_PATCH removes the key when nil is written.
func formulaDiff(specs []FieldSpec, data map[string]any) map[string]any {
	img := cloneData(data)
	if img == nil {
		img = map[string]any{}
	}
	applyComputedFields(specs, img)
	diff := map[string]any{}
	for _, sp := range specs {
		if sp.FieldType != "formula" {
			continue
		}
		next := img[sp.Name]
		if next == nil && data[sp.Name] == nil {
			continue
		}
		if !reflect.DeepEqual(data[sp.Name], next) {
			diff[sp.Name] = next
		}
	}
	return diff
}
