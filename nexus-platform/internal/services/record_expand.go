package services

import (
	"context"
	"fmt"
	"strings"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/dynrepo"
)

// LabelFetcher resolves a batch of record IDs in a target entity to their label values.
// Implemented by *dynrepo.Repository; abstracted here so the service is unit-testable.
type LabelFetcher interface {
	BatchFetchLabels(ctx context.Context, targetEntity, tenantID, labelField string, ids []string) ([]dynrepo.LabelResult, error)
}

// FieldValueFetcher resolves a batch of record IDs in a target entity to the value of ONE
// field, preserving its native JSON type. Implemented by *dynrepo.Repository (BatchFetchField);
// abstracted here so applyLookups is unit-testable. Backs ENG-COMPUTED #2 (read-time lookup).
type FieldValueFetcher interface {
	BatchFetchField(ctx context.Context, targetEntity, tenantID, sourceField string, ids []string) ([]dynrepo.FieldValueResult, error)
}

// userLabelFetcher resolves a batch of user ids to their display label (full name) + email — the
// `user`-field analogue of LabelFetcher, which targets an obj_* table; this targets the FIXED
// `users` system table instead (USER-FIELD slice). applyExpand type-asserts its EXISTING fetcher
// param against this interface (no second wiring call in cmd/server/main.go needed): production's
// *dynrepo.Repository satisfies both LabelFetcher and userLabelFetcher.
type userLabelFetcher interface {
	BatchFetchUserLabels(ctx context.Context, tenantID string, ids []string) ([]dynrepo.UserLabelResult, error)
}

// expandedValue is the response shape for an expanded relation field: {id, label}.
type expandedValue struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// expandedIDOnly is the response shape for an expanded relation whose labelField the caller may
// NOT view under field-level ACL (EXP-01): the FK id — which the caller already sees in the record
// it read — is echoed as {id}, but the target's label is withheld (never fetched, never leaked).
type expandedIDOnly struct {
	ID string `json:"id"`
}

// expandedUserValue is the response shape for an expanded `user` field: {id, label, email}.
// `label` mirrors expandedValue's key (the display name) so FE code that already renders a
// generic {id,label} shape for relation expands gets the person's name for free; `email` is
// additive. There is no field-level-ACL gate here (EXP-01's gate is keyed on the TARGET
// entity's sys_fields — `users` is a fixed system table, never a sys_entity, so it has no
// field-ACL surface at this layer) and no entity-level authz gate either (module ACL is
// per-sys_entity too) — every caller who can see a record may see who it is assigned to.
type expandedUserValue struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Email string `json:"email"`
}

// applyExpand resolves the requested expand fields in a slice of records (RM-07).
// Design rules:
//   - Depth = 1 only: expanded values are {id, label} terminals, never further expanded.
//   - One query per unique target entity (not per record) — batch IN, no N+1.
//   - Unknown expand field names (not declared in specs or not a relation/user field) are
//     silently ignored (forward-compat friendly; documented in api-conventions.md).
//   - labelField defaults to "name" when not specified in the field's options.
//   - Tenant-scoped: BatchFetchLabels filters by tenant_id so cross-tenant ids never resolve.
//   - Field-level ACL (EXP-01/A6): a labelField the caller may NOT view on the TARGET entity
//     resolves to {id} only (the label withheld, never fetched). The target's hidden-field set is
//     loaded ONCE per target entity per call (hoisted). A nil fieldACL disables the gate (unit
//     tests / pre-ACL wiring), mirroring the nil-authz behaviour of the entity-level gate.
//   - USER-FIELD: a `user` field expands to {id, label, email} (expandedUserValue) via a SEPARATE
//     fetch (users is a fixed system table, not obj_*) — see the userFieldNames block below.
//     Resolved only when `fetcher` also implements userLabelFetcher (production's
//     *dynrepo.Repository does); otherwise silently left as the raw id (forward-compat, mirrors
//     an unknown expand field).
//
// records is modified in-place: for each expanded field, the raw id string in record.Data
// is replaced with a map{"id":..., "label":...} (or {"id":...} when the labelField is field-ACL
// denied, or left as-is if the id did not resolve).
func applyExpand(
	ctx context.Context,
	fetcher LabelFetcher,
	authz entityAuthorizer,
	fieldACL fieldACLEnforcer,
	scope RecordScope,
	tenantID string,
	records []dynrepo.Record,
	specs []FieldSpec,
	expandFields []string,
) error {
	if len(expandFields) == 0 || len(records) == 0 {
		return nil
	}

	// Build a map of relation field specs keyed by name for fast lookup, and (USER-FIELD) the
	// set of declared `user` field names — a `user` field needs no Options (fixed target), so a
	// name-set is enough (unlike relSpecs, which carries relatedEntity/labelField).
	relSpecs := make(map[string]FieldSpec)
	userFieldSet := make(map[string]struct{})
	for _, s := range specs {
		switch s.FieldType {
		case "relation":
			relSpecs[s.Name] = s
		case "user":
			userFieldSet[s.Name] = struct{}{}
		}
	}

	// Filter expandFields to those that are declared relation fields. Unknown fields are
	// ignored (silently, per the documented design choice in api-conventions.md).
	type expandTarget struct {
		fieldName    string
		targetEntity string
		labelField   string
		labelDenied  bool // EXP-01: field-ACL denies the labelField on the target → resolve {id} only
	}
	var targets []expandTarget
	var userFieldNames []string // USER-FIELD: requested `user` fields, resolved separately below
	seen := make(map[string]struct{})
	// Per-call cache of the caller's hidden-field set on each TARGET entity, so N expand fields
	// into the same entity cost ONE restriction load, not N (mirrors applyLookups' targetHidden).
	targetHidden := make(map[string]map[string]struct{})
	for _, ef := range expandFields {
		clean, err := ddl.SanitizeIdentifier(ef)
		if err != nil {
			// Not a valid identifier → ignore.
			continue
		}
		if _, already := seen[clean]; already {
			continue
		}
		seen[clean] = struct{}{}
		if _, isUserField := userFieldSet[clean]; isUserField {
			userFieldNames = append(userFieldNames, clean)
			continue
		}
		spec, ok := relSpecs[clean]
		if !ok {
			// Not a declared relation (or user) field → silently skip.
			continue
		}
		// Extract target entity from options.relatedEntity.
		target, ok := spec.Options["relatedEntity"].(string)
		if !ok || strings.TrimSpace(target) == "" {
			continue
		}
		target = strings.TrimSpace(target)

		// W5-A2: only expand a relation whose TARGET entity the caller may VIEW. Without this,
		// ?expand resolves a label (e.g. a person's name) for ANY tenant record regardless of the
		// caller's ACL on the target entity — a cross-entity / cross-team label harvest (List is a
		// bulk harvester). Admins bypass; a nil authorizer (unit tests / expand-without-ACL wiring)
		// skips the check to preserve prior behaviour — production wires WithACLAuthorizer.
		// Entity-level (v1): row-level narrowing of labels is deliberately out of scope so a
		// legitimately-referenced record's name still resolves; the worst hole (an entity the caller
		// cannot view at all) is closed here.
		if authz != nil && !scope.IsAdmin {
			allowed, _, err := authz.Authorize(ctx, tenantID, scope.RoleID, target, "view")
			if err != nil {
				return fmt.Errorf("services.applyExpand: authorize target %q: %w", target, err)
			}
			if !allowed {
				continue // caller cannot view the target entity → leave the raw id, do not leak a label
			}
		}
		// labelField defaults to "name" when not specified.
		lf := "name"
		if lfRaw, ok := spec.Options["labelField"].(string); ok && strings.TrimSpace(lfRaw) != "" {
			lf = strings.TrimSpace(lfRaw)
		}
		// EXP-01 (RM-42/A6): field-level ACL gate on the labelField of the TARGET entity — closes
		// the pre-existing debt where a caller allowed to VIEW the entity but denied can_view on
		// its labelField still received {id,label}. This is a SEPARATE gate from the entity-level
		// authz above: entity-view can pass while the specific labelField is denied. Mirrors
		// applyLookups' sourceDenied gate — the caller's hidden-field set on the target is loaded
		// ONCE per target entity per call (hoisted cache; N expand fields into the same entity, and
		// every record in the page, cost ONE restriction load, not N). A denied labelField →
		// labelDenied: the expand resolves to {id} only, never fetched (so the withheld label is
		// never pulled into process memory). Fail-closed: a restriction-load error refuses the
		// whole page (mirrors stripReadFields). A nil fieldACL disables the gate (pre-ACL wiring).
		labelDenied := false
		if fieldACL != nil && !scope.IsAdmin {
			th, cached := targetHidden[target]
			if !cached {
				var herr error
				th, herr = fieldACL.HiddenFields(ctx, tenantID, scope.RoleID, target, scope.IsAdmin)
				if herr != nil {
					return fmt.Errorf("services.applyExpand: field-acl on target %q: %w", target, herr)
				}
				targetHidden[target] = th
			}
			_, labelDenied = th[lf]
		}
		targets = append(targets, expandTarget{
			fieldName:    clean,
			targetEntity: strings.TrimSpace(target),
			labelField:   lf,
			labelDenied:  labelDenied,
		})
	}

	// For each expand target, collect all distinct non-empty ids across all records,
	// fetch labels in one batch query, then patch each record's data.
	for _, t := range targets {
		// EXP-01: labelField denied by field-ACL → resolve each non-empty FK to {id} only, WITHOUT
		// fetching. Fetching-then-discarding would still copy the withheld label into process
		// memory (one bug from the response). The id is the caller's own stored FK (already
		// visible in the record it read), so echoing {id} leaks nothing; the label never appears.
		if t.labelDenied {
			for i := range records {
				if val, ok := records[i].Data[t.fieldName]; ok {
					if idStr, ok := val.(string); ok && strings.TrimSpace(idStr) != "" {
						records[i].Data[t.fieldName] = expandedIDOnly{ID: idStr}
					}
				}
			}
			continue
		}
		// Collect unique IDs for this relation field across all records.
		idSet := make(map[string]struct{})
		for _, rec := range records {
			if val, ok := rec.Data[t.fieldName]; ok {
				if idStr, ok := val.(string); ok && strings.TrimSpace(idStr) != "" {
					idSet[idStr] = struct{}{}
				}
			}
		}
		if len(idSet) == 0 {
			continue
		}
		ids := make([]string, 0, len(idSet))
		for id := range idSet {
			ids = append(ids, id)
		}

		// One query for all ids in this entity (batch IN — no N+1).
		results, err := fetcher.BatchFetchLabels(ctx, t.targetEntity, tenantID, t.labelField, ids)
		if err != nil {
			return fmt.Errorf("services.applyExpand: fetch labels for %q: %w", t.targetEntity, err)
		}

		// Build an id→label lookup map from the results.
		labelMap := make(map[string]string, len(results))
		for _, r := range results {
			labelMap[r.ID] = r.Label
		}

		// Patch each record: replace the raw id string with {id, label}.
		for i := range records {
			if val, ok := records[i].Data[t.fieldName]; ok {
				if idStr, ok := val.(string); ok && strings.TrimSpace(idStr) != "" {
					label, found := labelMap[idStr]
					if found {
						records[i].Data[t.fieldName] = expandedValue{ID: idStr, Label: label}
					}
					// If not found (deleted/cross-tenant target), leave the raw id string
					// untouched so the caller still sees the stored FK value.
				}
			}
		}
	}

	// USER-FIELD: resolve every requested `user` field via a SEPARATE fetch against the fixed
	// `users` system table (not obj_*), so it needs its own type-asserted fetcher rather than
	// reusing BatchFetchLabels (which is entity-table-shaped: targetEntity + labelField). A
	// fetcher that does not implement userLabelFetcher (unit-test fakes that predate this slice)
	// silently disarms this block only — the relation expand above is unaffected.
	if len(userFieldNames) > 0 {
		if uf, ok := fetcher.(userLabelFetcher); ok {
			for _, fieldName := range userFieldNames {
				idSet := make(map[string]struct{})
				for _, rec := range records {
					if val, ok := rec.Data[fieldName]; ok {
						if idStr, ok := val.(string); ok && strings.TrimSpace(idStr) != "" {
							idSet[idStr] = struct{}{}
						}
					}
				}
				if len(idSet) == 0 {
					continue
				}
				ids := make([]string, 0, len(idSet))
				for id := range idSet {
					ids = append(ids, id)
				}
				results, err := uf.BatchFetchUserLabels(ctx, tenantID, ids)
				if err != nil {
					return fmt.Errorf("services.applyExpand: fetch user labels for %q: %w", fieldName, err)
				}
				byID := make(map[string]dynrepo.UserLabelResult, len(results))
				for _, r := range results {
					byID[r.ID] = r
				}
				for i := range records {
					if val, ok := records[i].Data[fieldName]; ok {
						if idStr, ok := val.(string); ok && strings.TrimSpace(idStr) != "" {
							if u, found := byID[idStr]; found {
								records[i].Data[fieldName] = expandedUserValue{ID: idStr, Label: u.Name, Email: u.Email}
							}
							// Not found (deleted/cross-tenant user) → leave the raw id untouched,
							// mirroring the relation branch above.
						}
					}
				}
			}
		}
	}
	return nil
}

// applyLookups resolves every `lookup` field declared on the entity (ENG-COMPUTED #2): each
// lookup field mirrors `options.sourceField` from the related record referenced by the sibling
// `options.via` relation field. Unlike ?expand, lookup resolution is ALWAYS ON (read-time,
// always-fresh per the spec) — it does not depend on a query param, and it runs regardless of
// whether the caller also requested ?expand on the same `via` field.
//
// IMPORTANT ORDERING: this must run BEFORE applyExpand. applyExpand rewrites
// record.Data[via] from the raw FK id string into an {id,label} expandedValue struct; if lookup
// ran after that mutation it would read a struct instead of a string and resolve nothing. Both
// callers (Get/List in record_orchestration.go) invoke applyLookups first, then applyExpand.
//
// Design rules (mirrors applyExpand):
//   - Depth = 1 only, via a single sibling to-one relation field (never chained).
//   - One BatchFetchField query per unique (targetEntity, sourceField) pair — batch IN, no N+1.
//   - Tenant-scoped: BatchFetchField filters by tenant_id so a cross-tenant FK never resolves.
//   - ACL check on the SOURCE entity (options.via's relatedEntity) mirrors applyExpand's W5-A2
//     gate: a caller who cannot view the source entity gets null, never a leaked value.
//   - Field-level ACL (RM-42/A6), fail-closed on BOTH sides: a lookup field in `hidden` (the
//     caller's can_view=0 set on the CURRENT entity, resolved by the caller exactly like
//     stripReadFields) is never resolved nor written — computed writes run AFTER the read
//     strip, so skipping here is what keeps the strip authoritative. And a sourceField the
//     role may not view on the TARGET entity resolves to null (fieldACL restriction set
//     loaded ONCE per target entity per call — hoisted, no per-row load) so a lookup can
//     never side-door the target's field-level ACL (List would be a bulk harvester).
//   - Null via-FK (absent/empty), a soft-deleted source row, a cross-tenant FK, or an
//     ACL-denied source entity all resolve to null — never an error, never a leak.
func applyLookups(
	ctx context.Context,
	fetcher FieldValueFetcher,
	authz entityAuthorizer,
	fieldACL fieldACLEnforcer,
	scope RecordScope,
	tenantID string,
	records []dynrepo.Record,
	specs []FieldSpec,
	hidden map[string]struct{},
	denied map[string]struct{}, // optional out-set: lookups whose source the caller may not view
) error {
	if fetcher == nil || len(records) == 0 {
		return nil
	}

	// Build a map of sibling relation field specs (by name) so a lookup's `via` can be
	// resolved to its relatedEntity without a second pass over specs per lookup field.
	relSpecs := make(map[string]FieldSpec)
	var lookupSpecs []FieldSpec
	for _, s := range specs {
		switch s.FieldType {
		case "relation":
			relSpecs[s.Name] = s
		case "lookup":
			if _, isHidden := hidden[s.Name]; isHidden {
				// can_view=0 on the lookup field itself: the read strip already removed any
				// stored key; never resolve, never re-add (the original A6 bypass).
				continue
			}
			lookupSpecs = append(lookupSpecs, s)
		}
	}
	if len(lookupSpecs) == 0 {
		return nil
	}

	type lookupTarget struct {
		fieldName    string // the lookup field's own name (write destination)
		viaField     string // sibling relation field name (read source of the FK id)
		sourceField  string // field name on the related entity to mirror
		targetEntity string // relatedEntity of the via relation field
		sourceDenied bool   // field-ACL: role may not view sourceField on the target → null, no fetch
	}
	var targets []lookupTarget
	// Per-call cache of the caller's hidden-field set on each TARGET entity, so N lookup fields
	// into the same entity (and every record in the page) cost ONE restriction load, not N.
	targetHidden := make(map[string]map[string]struct{})
	for _, ls := range lookupSpecs {
		via, _ := ls.Options["via"].(string)
		via = strings.TrimSpace(via)
		sourceField, _ := ls.Options["sourceField"].(string)
		sourceField = strings.TrimSpace(sourceField)
		if via == "" || sourceField == "" {
			continue // malformed config (should have been rejected at config time) — skip defensively
		}
		relSpec, ok := relSpecs[via]
		if !ok {
			continue // via no longer names a relation field (e.g. sibling deleted) — skip defensively
		}
		target, ok := relSpec.Options["relatedEntity"].(string)
		if !ok || strings.TrimSpace(target) == "" {
			continue
		}
		target = strings.TrimSpace(target)

		// ACL gate on the SOURCE entity (mirrors applyExpand W5-A2): a caller who cannot view
		// the related entity must never receive a mirrored value from it.
		if authz != nil && !scope.IsAdmin {
			allowed, _, err := authz.Authorize(ctx, tenantID, scope.RoleID, target, "view")
			if err != nil {
				return fmt.Errorf("services.applyLookups: authorize source %q: %w", target, err)
			}
			if !allowed {
				markDenied(denied, ls.Name)
				continue // caller cannot view the source entity → leave the lookup field null
			}
		}

		// Field-level ACL gate on the mirrored sourceField of the TARGET entity (RM-42/A6):
		// without this, a lookup field is a side-door around the target's can_view=0 — e.g.
		// contacts.salary denied, but deals.contactSalary mirrors it anyway. Denied → the
		// lookup stays declared but resolves to null (like an unresolvable FK). Fail-closed:
		// a restriction-load error refuses the whole page (mirrors stripReadFields).
		sourceDenied := false
		if fieldACL != nil && !scope.IsAdmin {
			th, cached := targetHidden[target]
			if !cached {
				var herr error
				th, herr = fieldACL.HiddenFields(ctx, tenantID, scope.RoleID, target, scope.IsAdmin)
				if herr != nil {
					return fmt.Errorf("services.applyLookups: field-acl on source %q: %w", target, herr)
				}
				targetHidden[target] = th
			}
			_, sourceDenied = th[sourceField]
		}

		targets = append(targets, lookupTarget{
			fieldName:    ls.Name,
			viaField:     via,
			sourceField:  sourceField,
			targetEntity: target,
			sourceDenied: sourceDenied,
		})
	}
	if len(targets) == 0 {
		return nil
	}

	for _, t := range targets {
		if t.sourceDenied {
			// Role may not view the mirrored field on the target entity: every row resolves to
			// null and the value is NEVER fetched (fetching-then-discarding would still copy the
			// denied value into process memory and one bug away from the response).
			markDenied(denied, t.fieldName)
			for i := range records {
				records[i].Data[t.fieldName] = nil
			}
			continue
		}
		// Collect unique via-FK ids across all records (read the RAW id string — this must run
		// before applyExpand mutates record.Data[viaField] into an expandedValue struct).
		idSet := make(map[string]struct{})
		for _, rec := range records {
			if val, ok := rec.Data[t.viaField]; ok {
				if idStr, ok := val.(string); ok && strings.TrimSpace(idStr) != "" {
					idSet[idStr] = struct{}{}
				}
			}
		}

		// valueMap stays empty (never queried) when EVERY record's via-FK is null/empty across
		// the whole batch — an empty IN-list is never a valid query (mirrors buildPredicates'
		// empty-IN-list guard elsewhere), so BatchFetchField would have nothing to resolve
		// anyway. Bug fix: this must NOT `continue` past the per-record fill-in loop below — a
		// lookup field's presence in the response must never depend on whether some OTHER
		// record in the same page happened to have a resolved FK (batch-dependent
		// inconsistency: page A with one resolved FK sets the key to null on every row; page B
		// with zero resolved FKs across the whole page previously left the key absent
		// entirely). The field is ALWAYS present (as null or the resolved value) on every
		// record that declares it, regardless of the rest of the batch.
		var valueMap map[string]any
		if len(idSet) > 0 {
			ids := make([]string, 0, len(idSet))
			for id := range idSet {
				ids = append(ids, id)
			}

			// One query per (targetEntity, sourceField) pair for every record needing this
			// lookup — batch IN, no N+1.
			results, err := fetcher.BatchFetchField(ctx, t.targetEntity, tenantID, t.sourceField, ids)
			if err != nil {
				return fmt.Errorf("services.applyLookups: fetch field %q from %q: %w", t.sourceField, t.targetEntity, err)
			}
			valueMap = make(map[string]any, len(results))
			for _, r := range results {
				valueMap[r.ID] = r.Value
			}
		}

		for i := range records {
			via, ok := records[i].Data[t.viaField]
			if !ok {
				records[i].Data[t.fieldName] = nil
				continue
			}
			idStr, ok := via.(string)
			if !ok || strings.TrimSpace(idStr) == "" {
				// Null/empty via-FK → the lookup field is null (no fetch attempted for this row).
				records[i].Data[t.fieldName] = nil
				continue
			}
			// Not found → soft-deleted source, cross-tenant FK, or race-deleted between the FK
			// write and this read. Resolves to null, never an error, never a leak.
			records[i].Data[t.fieldName] = valueMap[idStr]
		}
	}
	return nil
}
