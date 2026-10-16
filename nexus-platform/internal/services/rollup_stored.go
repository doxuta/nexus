package services

// specs/083-rollup-stored-value: a roll-up that keeps its value in the parent record's `data`.
//
// A plain roll-up is computed when a record is READ, so nothing in the table holds the number:
// it cannot be filtered, SUMmed in a report, or read by a rule. With options.stored = true the
// value is written into obj_<parent>.data[<field>] — in the same shape as a number or currency
// field — and refreshed whenever a child row is created, edited, deleted or restored. The read
// path then serves the stored value untouched (applyRollups skips it).
//
// The stored value is the TENANT-WIDE total (the refresh runs as the system, not as the user who
// saved the child), because one number is shared by everyone who can see the parent. Field-level
// ACL on the roll-up field itself still applies to readers, as for any field.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// isStoredRollup reports whether sp is a roll-up that keeps its value in `data`.
func isStoredRollup(sp FieldSpec) bool {
	return sp.FieldType == "rollup" && boolOpt(sp.Options, "stored")
}

// storedRollupValueType is the plain field type a stored roll-up is stored as: "currency"
// ({amount,currencyCode}) when it totals money, else "number". Stamped into options.valueType by
// validateRollupCrossEntity, which can see the child's aggField; filters and aggregates read it so
// a stored roll-up behaves like the number/currency field it holds.
func storedRollupValueType(sp FieldSpec) string {
	if vt, _ := sp.Options["valueType"].(string); vt == "currency" {
		return "currency"
	}
	return "number"
}

// storedValue converts a resolved roll-up into what is written to `data`: count and plain
// metrics as a JSON number, money as {amount,currencyCode}, no matching child as nil (which
// JSON_MERGE_PATCH turns into an absent key — "no value", like the read-time form).
func (r RollupResult) storedValue() (any, error) {
	if r.Metric == "count" {
		if r.IntValue == nil {
			return float64(0), nil
		}
		return float64(*r.IntValue), nil
	}
	if r.DecimalValue == nil {
		return nil, nil
	}
	n, err := strconv.ParseFloat(*r.DecimalValue, 64)
	if err != nil {
		return nil, fmt.Errorf("services.storedValue: parse %q: %w", *r.DecimalValue, err)
	}
	if r.IsCurrency {
		return map[string]any{"amount": n, "currencyCode": r.CurrencyCode}, nil
	}
	return n, nil
}

// seedStoredCounts gives a NEW record its starting value for every stored count roll-up (0), so a
// parent with no children yet filters like one whose children were all removed (`si_so[lt]=30`
// must match an empty class). CREATE-only: never call it on update, where it would overwrite.
// Other metrics have no value until a child exists (absent key), as on the read-time form.
func seedStoredCounts(specs []FieldSpec, data map[string]any) {
	for _, sp := range specs {
		if !isStoredRollup(sp) || sp.Options["metric"] != "count" {
			continue
		}
		if _, has := data[sp.Name]; !has {
			data[sp.Name] = float64(0)
		}
	}
}

// storedRollupFinder lists the stored roll-ups over one child entity. Satisfied by
// *metadata.Repository; a metadata fake without it simply skips the refresh (like the other
// optional seams).
type storedRollupFinder interface {
	ListStoredRollupsOfChild(ctx context.Context, tenantID, childEntity string) ([]metadata.StoredRollupField, error)
}

// computedValueWriter is the version-guarded, non-edit write the refresh uses (the same primitive
// the stored-formula sweep uses): it leaves version/updated_at/modified_by alone.
type computedValueWriter interface {
	SetComputedValues(ctx context.Context, entity, tenantID, id string, version int, values map[string]any) (bool, error)
}

var _ computedValueWriter = (*dynrepo.Repository)(nil)
var _ storedRollupFinder = (*metadata.Repository)(nil)

// storedRollupAttempts bounds the re-reads of one parent that keeps changing under the refresh.
const storedRollupAttempts = 3

// systemRollupScope is the caller identity a refresh computes under: the whole tenant's children.
var systemRollupScope = RecordScope{IsAdmin: true}

// refreshStoredRollups recomputes, for the parents named by the given child row images, every
// stored roll-up that totals childEntity. Call it AFTER a child write committed, with the image(s)
// that carry the parent id: one for create/delete/restore, before AND after for an edit (the child
// may have moved to another parent — both totals change).
//
// Best effort by design: the child write has already committed, so a failure here is logged and
// swallowed (a failed save would lose the user's data over a derived number). Admins can repair a
// drifted total with RecomputeStoredRollup ("Tính lại" in Studio). Runs detached from the request's
// cancellation so a closed tab cannot leave a total half-updated.
//
// ponytail: one aggregate query per (parent, roll-up) and a plan per roll-up per call — fine for
// interactive writes; the import path hands it one call per batch.
func (s *RecordService) refreshStoredRollups(ctx context.Context, tenantID, childEntity string, images ...map[string]any) {
	finder, ok := s.rollupMeta.(storedRollupFinder)
	if !ok || len(images) == 0 {
		return
	}
	writer, ok := s.store.(computedValueWriter)
	if !ok {
		return
	}
	ctx = context.WithoutCancel(ctx)
	refs, err := finder.ListStoredRollupsOfChild(ctx, tenantID, childEntity)
	if err != nil {
		s.log().Warn("stored rollup: list roll-ups of child failed",
			zap.String("tenant_id", tenantID), zap.String("entity_name", childEntity), zap.Error(err))
		return
	}
	for _, ref := range refs {
		spec, perr := storedRollupSpec(ref)
		if perr != nil {
			s.log().Warn("stored rollup: bad options", zap.String("tenant_id", tenantID),
				zap.String("entity_name", ref.ParentEntity), zap.String("field", ref.Name), zap.Error(perr))
			continue
		}
		via, _ := spec.Options["viaRelation"].(string)
		parents := distinctParentIDs(via, images)
		if len(parents) == 0 {
			continue
		}
		plan, perr := s.prepareRollup(ctx, systemRollupScope, tenantID, spec)
		if perr != nil {
			s.log().Warn("stored rollup: prepare failed", zap.String("tenant_id", tenantID),
				zap.String("entity_name", ref.ParentEntity), zap.String("field", ref.Name), zap.Error(perr))
			continue
		}
		for _, pid := range parents {
			if _, werr := s.writeStoredRollup(ctx, writer, tenantID, ref.ParentEntity, plan, pid); werr != nil {
				s.log().Warn("stored rollup: refresh failed", zap.String("tenant_id", tenantID),
					zap.String("entity_name", ref.ParentEntity), zap.String("record_id", pid),
					zap.String("field", ref.Name), zap.Error(werr))
			}
		}
	}
}

// RefreshStoredRollups is refreshStoredRollups for callers outside the record service (the
// workflow mutator's after-write hook).
func (s *RecordService) RefreshStoredRollups(ctx context.Context, tenantID, childEntity string, images ...map[string]any) {
	s.refreshStoredRollups(ctx, tenantID, childEntity, images...)
}

func storedRollupSpec(ref metadata.StoredRollupField) (FieldSpec, error) {
	opts, err := parseOptionsJSON(ref.Options)
	if err != nil {
		return FieldSpec{}, fmt.Errorf("services.storedRollupSpec: %w", err)
	}
	return FieldSpec{Name: ref.Name, Label: ref.Label, FieldType: "rollup", Options: opts}, nil
}

// distinctParentIDs collects the non-empty string values of the relation field `via` across the
// row images, in first-seen order.
func distinctParentIDs(via string, images []map[string]any) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, img := range images {
		id, _ := img[via].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// writeStoredRollup recomputes one parent's stored roll-up and writes it when it differs from what
// is stored. A parent that is gone (deleted, other tenant) is skipped. Returns whether a write
// happened.
func (s *RecordService) writeStoredRollup(ctx context.Context, writer computedValueWriter, tenantID, parentEntity string, plan rollupPlan, parentID string) (bool, error) {
	res, err := s.executeRollup(ctx, tenantID, plan, parentID)
	if err != nil {
		return false, err
	}
	val, err := res.storedValue()
	if err != nil {
		return false, err
	}
	for attempt := 0; attempt < storedRollupAttempts; attempt++ {
		rec, err := s.store.GetRecord(ctx, parentEntity, tenantID, parentID, dynrepo.AdminScope())
		if errors.Is(err, dynrepo.ErrRecordNotFound) || (err == nil && rec == nil) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("services.writeStoredRollup: read parent: %w", err)
		}
		if storedEqual(rec.Data[plan.spec.Name], val) {
			return false, nil
		}
		ok, err := writer.SetComputedValues(ctx, parentEntity, tenantID, parentID, rec.Version, map[string]any{plan.spec.Name: val})
		if err != nil {
			return false, fmt.Errorf("services.writeStoredRollup: write: %w", err)
		}
		if ok {
			return true, nil
		}
		// A user save moved the parent's version first: re-read and judge again.
	}
	return false, fmt.Errorf("services.writeStoredRollup: parent %s kept changing during the refresh", parentID)
}

// storedEqual compares a stored JSON value with a freshly computed one; nil equals an absent key.
func storedEqual(stored, next any) bool {
	if stored == nil && next == nil {
		return true
	}
	return reflect.DeepEqual(stored, next)
}

// RecomputeStoredRollup recomputes one stored roll-up on every live row of the parent entity — the
// "Tính lại" command, and the backfill that runs when the field is first added. Returns the rows
// whose stored value changed.
//
// ponytail: one aggregate query per parent row, synchronous, keyset pages of 100 — fine for the
// tens of thousands of rows a customer entity holds; a grouped aggregate (GROUP BY parent) and a
// background job with progress are the upgrade path.
func (s *RecordService) RecomputeStoredRollup(ctx context.Context, tenantID, entityName, fieldName string) (int, error) {
	ctx = context.WithoutCancel(ctx)
	e, specs, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return 0, fmt.Errorf("services.RecomputeStoredRollup: %w", err)
	}
	var spec *FieldSpec
	for i := range specs {
		if specs[i].Name == fieldName {
			spec = &specs[i]
		}
	}
	if spec == nil || !isStoredRollup(*spec) {
		return 0, invalid("FIELD_ROLLUP_NOT_STORED",
			"trường %q không phải trường tổng hợp có lưu giá trị", fieldName)
	}
	writer, ok := s.store.(computedValueWriter)
	if !ok {
		return 0, fmt.Errorf("services.RecomputeStoredRollup: store cannot write computed values")
	}
	plan, err := s.prepareRollup(ctx, systemRollupScope, tenantID, *spec)
	if err != nil {
		return 0, fmt.Errorf("services.RecomputeStoredRollup: %w", err)
	}
	written, after := 0, ""
	for {
		page, next, err := s.store.ListRecordsCursor(ctx, e.Name, tenantID, dynrepo.AdminScope(), dynrepo.CursorParams{
			Limit: formulaSweepPage, After: after, Sort: dynrepo.DefaultCursorSort, Order: "ASC",
		})
		if err != nil {
			return written, fmt.Errorf("services.RecomputeStoredRollup: list %s: %w", e.Name, err)
		}
		for _, rec := range page {
			changed, werr := s.writeStoredRollup(ctx, writer, tenantID, e.Name, plan, rec.ID)
			if werr != nil {
				return written, fmt.Errorf("services.RecomputeStoredRollup: %s %s: %w", e.Name, rec.ID, werr)
			}
			if changed {
				written++
			}
		}
		if next == "" {
			return written, nil
		}
		after = next
	}
}
