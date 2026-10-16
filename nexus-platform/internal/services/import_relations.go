package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/text/unicode/norm"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// Relation cells of a CSV import (specs/061 FR-012..FR-015): a customer's file names the target
// record the way people do — "Phú Gia Riverside", "Công ty TNHH Lạnh Việt 1", a project code — not
// by UUID. Each mapped relation field matches its cells against the target's ids first, then
// against ONE field of the target (the relation's label field by default, or the one the builder
// chose in "Khớp theo"), in one batched query per field, within the tenant.

// relationValueMatcher finds live target records by a field value (dynrepo.BatchIDsByFieldValue).
// It is read off the wired RelationBatchChecker — production's *dynrepo.Repository is both — so the
// lookup cannot be wired without the id check it extends.
type relationValueMatcher interface {
	BatchIDsByFieldValue(ctx context.Context, entity, tenantID, fieldName string, values []string, scope dynrepo.RowScope) (map[string][]string, error)
}

// matchableTypes are the target field types a cell may name: stored as a plain string.
var matchableTypes = map[string]bool{"text": true, "email": true, "auto_number": true}

// relationMatch is how one mapped relation field's cells find their target record.
type relationMatch struct {
	target      string // sanitized target entity; "" = the relation names no valid target
	targetLabel string // the target's display name, for messages
	field       string // target field the cells may name; "" = ids only
	fieldLabel  string
	denied      bool // the caller may not look the target up by value (FR-015): ids only
	// scope narrows the value lookup to the target records the caller may VIEW (FR-015): a name
	// is guessable, so it must not reach a record their own reads would not return.
	scope dynrepo.RowScope
	// idScope / noView: the id probe of a NON-admin import runs under the caller's VIEW grant on the
	// target (#331), the same predicate GetRecord and RecordService.relationScopeFor apply — an id
	// the importer may not see reads as absent. idScoped false (admin, or no authorizer: unit
	// tests) keeps the tenant-wide system probe. noView = no VIEW grant: every id reads as absent.
	idScope  dynrepo.RowScope
	idScoped bool
	noView   bool
}

// WithACLAuthorizer injects the entity-ACL authorizer that gates relation lookup by value on the
// caller's VIEW grant on the target entity (FR-015) — the same gate ?expand uses (W5-A2). nil (unit
// tests) leaves the lookup ungated, like RecordService.WithACLAuthorizer.
func (s *ImportService) WithACLAuthorizer(a entityAuthorizer) *ImportService {
	s.authz = a
	return s
}

// defaultRelationMatchField picks the target field a relation's cells name when the builder did not
// choose one (FR-013): the relation's options.labelField (what the record screens show for it), else
// the table's derived default — metadata.ResolveLabelField, the same pick ?expand and record titles
// use (#479). Matchable types only.
func defaultRelationMatchField(rel FieldSpec, target []FieldSpec) string {
	byName := make(map[string]FieldSpec, len(target))
	defs := make([]metadata.FieldDefinition, 0, len(target))
	for i, f := range target {
		byName[f.Name] = f
		defs = append(defs, metadata.FieldDefinition{Name: f.Name, FieldType: f.FieldType, IsRequired: f.IsRequired, FieldOrder: i})
	}
	if lf, _ := rel.Options["labelField"].(string); strings.TrimSpace(lf) != "" {
		if f, ok := byName[strings.TrimSpace(lf)]; ok && matchableTypes[f.FieldType] {
			return f.Name
		}
	}
	name, _ := metadata.ResolveLabelField(nil, defs)
	return name
}

// planRelationMatches decides, once per import, how each relation field's values find their
// target: which field of the target a MAPPED column's cells name, and whether the caller may look
// it up (FR-012, FR-013, FR-015). A relation the file does not map still gets its target — its
// only value is the field's default, an id, checked like any id cell (FR-014). A matchBy entry that
// names no mapped relation, or a target field that does not exist, is not matchable, or is hidden
// from the caller, refuses the whole file with ONE message shape — a hidden field reads exactly
// like a missing one.
func (s *ImportService) planRelationMatches(ctx context.Context, in ImportInput, specs []FieldSpec, cm columnMapping) (map[string]relationMatch, error) {
	mapped := make(map[string]bool)
	for _, sp := range cm {
		if sp.FieldType == "relation" {
			mapped[sp.Name] = true
		}
	}
	for field := range in.MatchBy {
		if !mapped[field] {
			return nil, invalidField("IMPORT_MATCH_FIELD_INVALID", field,
				"matchBy: %q không phải trường liên kết đang được ánh xạ", field)
		}
	}
	lookup := &RecordService{catalog: s.catalog}
	plan := make(map[string]relationMatch)
	for _, rel := range specs {
		if rel.FieldType != "relation" {
			continue
		}
		name := rel.Name
		raw, ok := relatedEntityName(rel.Options)
		target, err := ddl.SanitizeIdentifier(raw)
		if !ok || err != nil {
			plan[name] = relationMatch{} // every non-empty cell is refused below
			continue
		}
		m := relationMatch{target: target, targetLabel: target, scope: dynrepo.AdminScope()}
		e, targetSpecs, err := lookup.resolve(ctx, in.TenantID, target)
		switch {
		case errors.Is(err, ErrEntityNotFound):
			// the id check refuses every cell; nothing to match by value
		case err != nil:
			return nil, fmt.Errorf("import.planRelationMatches: %w", err)
		default:
			if e.Label != "" {
				m.targetLabel = e.Label
			}
		}
		if !in.IsAdmin { // every relation, mapped or not: its ids are probed under the caller's VIEW grant
			if m.noView, m.idScope, m.idScoped, err = s.viewScope(ctx, in, target); err != nil {
				return nil, err
			}
		}
		if !mapped[name] {
			plan[name] = m // no column: the default is an id, matched by id only
			continue
		}
		chosen := strings.TrimSpace(in.MatchBy[name])
		m.field = chosen
		if chosen == "" {
			m.field = defaultRelationMatchField(rel, targetSpecs)
			// #272: the table's own label field (builder's choice) is what its records are called
			// everywhere else, so it is also what an import cell names — ahead of the guesses.
			if e != nil {
				defs := make([]metadata.FieldDefinition, 0, len(targetSpecs))
				for _, f := range targetSpecs {
					defs = append(defs, metadata.FieldDefinition{Name: f.Name, FieldType: f.FieldType})
				}
				if lf, explicit := metadata.ResolveLabelField(e.Options, defs); explicit {
					m.field = lf
				}
			}
		}
		if m.field != "" && !in.IsAdmin {
			if m.denied, m.scope, err = s.matchDenied(ctx, in, target, m.field); err != nil {
				return nil, err
			}
		}
		fieldSpec, found := specByName(targetSpecs, m.field)
		if chosen != "" && (!found || !matchableTypes[fieldSpec.FieldType] || m.denied) {
			return nil, invalidField("IMPORT_MATCH_FIELD_INVALID", name,
				"matchBy: %s không có trường %q để khớp (chữ, email hoặc mã tự tăng)", m.targetLabel, chosen)
		}
		m.fieldLabel = fieldSpec.Label
		if m.fieldLabel == "" {
			m.fieldLabel = m.field
		}
		plan[name] = m
	}
	return plan, nil
}

func specByName(specs []FieldSpec, name string) (FieldSpec, bool) {
	for _, f := range specs {
		if f.Name == name {
			return f, true
		}
	}
	return FieldSpec{}, false
}

// viewScope is the non-admin caller's VIEW grant on the target entity as a row scope — the one
// predicate GetRecord applies (owner/team + manual shares). noView = no grant at all (every record
// reads as absent). scoped false = no authorizer wired (unit tests): ungated, tenant-wide probe.
// An ACL read error refuses the import (fail closed).
func (s *ImportService) viewScope(ctx context.Context, in ImportInput, target string) (noView bool, scope dynrepo.RowScope, scoped bool, err error) {
	if s.authz == nil {
		return false, dynrepo.AdminScope(), false, nil
	}
	allowed, level, err := s.authz.Authorize(ctx, in.TenantID, in.RoleID, target, "view")
	if err != nil {
		return false, scope, false, fmt.Errorf("import.viewScope: authorize %q: %w", target, err)
	}
	if !allowed {
		return true, dynrepo.RowScope{UserID: in.UserID, Level: dynrepo.AccessNone}, true, nil
	}
	return false, dynrepo.RowScope{UserID: in.UserID, Level: dynrepo.AccessLevel(level), IncludeShares: true}, true, nil
}

// matchDenied reports whether a non-admin caller may NOT find target records by field — no VIEW
// grant on the target, or the field is hidden from their role (FR-015) — and otherwise the row
// scope of that VIEW grant, which the lookup runs under (the same scope Link probes a target with).
// An ACL read error refuses the import (fail closed). nil authz (unit tests) = ungated.
func (s *ImportService) matchDenied(ctx context.Context, in ImportInput, target, field string) (bool, dynrepo.RowScope, error) {
	noView, vs, scoped, err := s.viewScope(ctx, in, target)
	if err != nil {
		return false, vs, fmt.Errorf("import.matchDenied: %w", err)
	}
	scope := dynrepo.AdminScope()
	if scoped {
		scope = dynrepo.RowScope{UserID: in.UserID, Level: vs.Level}
		if noView {
			return true, scope, nil
		}
	}
	if s.fieldACL != nil {
		hidden, err := s.fieldACL.HiddenFields(ctx, in.TenantID, in.RoleID, target, in.IsAdmin)
		if err != nil {
			return false, scope, fmt.Errorf("import.matchDenied: field-acl on %q: %w", target, err)
		}
		if _, isHidden := hidden[field]; isHidden {
			return true, scope, nil
		}
	}
	return false, scope, nil
}

// resolveRelations turns every relation cell of the surviving rows into the target record's id
// (FR-014): one BatchRecordExists per target entity (RM-11), then one value lookup per relation
// field for the cells that were not ids. A row keeps going only if every relation cell resolved to
// exactly one live record of the tenant; each cell that did not is reported on its own (not found:
// RECORD_RELATION_INVALID; several records: IMPORT_RELATION_AMBIGUOUS; lookup failure: IMPORT_ERROR).
func (s *ImportService) resolveRelations(ctx context.Context, tenantID string, specs []FieldSpec, rows []validRow, plan map[string]relationMatch) (kept []validRow, errs []ImportRowError) {
	type cell struct {
		row   int // index into rows
		field string
		value string // trimmed cell
	}
	var cells []cell
	idsByTarget := make(map[string][]string)
	for i, vr := range rows {
		for _, f := range specs {
			if f.FieldType != "relation" {
				continue
			}
			raw, _ := vr.payload[f.Name].(string)
			value := norm.NFC.String(strings.TrimSpace(raw)) // #360: an NFD-typed name matches the stored NFC one
			if value == "" {
				continue
			}
			cells = append(cells, cell{row: i, field: f.Name, value: value})
			if t := plan[f.Name].target; t != "" {
				idsByTarget[t] = append(idsByTarget[t], value)
			}
		}
	}
	if len(cells) == 0 {
		return rows, nil
	}

	// One id probe per target, under the importer's VIEW grant when they are not an admin (#331):
	// import writes through the store, not RecordService.Create, so it carries its own guard. A
	// hidden id answers exactly like a missing one.
	idScope := make(map[string]relationMatch, len(idsByTarget))
	for _, m := range plan {
		if m.target != "" {
			idScope[m.target] = m
		}
	}
	scoped, canScope := s.relChecker.(scopedRelationExister)
	failed := make(map[string]error) // target → id probe failure
	isID := make(map[string]map[string]bool, len(idsByTarget))
	for target, ids := range idsByTarget {
		var found map[string]bool
		var err error
		switch m := idScope[target]; {
		case m.noView:
			found = map[string]bool{} // no VIEW grant: nothing is visible, no probe needed
		case !m.idScoped:
			found, err = s.relChecker.BatchRecordExists(ctx, target, tenantID, ids)
		case !canScope: // fail closed rather than widen to the tenant
			err = errors.New("checker cannot probe in a row scope")
		default:
			found, err = scoped.BatchRecordExistsInScope(ctx, target, tenantID, ids, m.idScope)
		}
		if err != nil {
			failed[target] = err
			continue
		}
		isID[target] = found
	}

	// Cells that are not ids, per relation field, looked up by the planned field in one query.
	matcher, _ := s.relChecker.(relationValueMatcher)
	byValue := make(map[string]map[string][]string) // relation field → lowered value → ids
	lookupFailed := make(map[string]error)
	pending := make(map[string][]string)
	for _, c := range cells {
		m := plan[c.field]
		if m.target == "" || failed[m.target] != nil || isID[m.target][c.value] || m.field == "" || m.denied || matcher == nil {
			continue
		}
		pending[c.field] = append(pending[c.field], strings.ToLower(c.value))
	}
	for field, values := range pending {
		m := plan[field]
		found, err := matcher.BatchIDsByFieldValue(ctx, m.target, tenantID, m.field, values, m.scope)
		if err != nil {
			lookupFailed[field] = err
			continue
		}
		byValue[field] = found
	}

	bad := make(map[int]bool)
	for _, c := range cells {
		m := plan[c.field]
		fail := func(code, format string, args ...any) {
			bad[c.row] = true
			errs = append(errs, ImportRowError{Row: rows[c.row].rowNum, Field: c.field, Code: code, Message: fmt.Sprintf(format, args...)})
		}
		switch {
		case m.target == "":
			fail("RECORD_RELATION_INVALID", "field relation %q thiếu entity đích để kiểm tra FK", c.field)
		case failed[m.target] != nil:
			fail("IMPORT_ERROR", "không kiểm tra được liên kết %q: %s", c.field, failed[m.target].Error())
		case isID[m.target][c.value]:
			rows[c.row].payload[c.field] = c.value
		case lookupFailed[c.field] != nil:
			fail("IMPORT_ERROR", "không tra được %q trong %s: %s", c.value, m.targetLabel, lookupFailed[c.field].Error())
		default:
			ids := byValue[c.field][strings.ToLower(c.value)]
			switch {
			case len(ids) == 1:
				rows[c.row].payload[c.field] = ids[0]
			case len(ids) > 1:
				fail("IMPORT_RELATION_AMBIGUOUS", "%q khớp %d bản ghi của %s — ghi mã (id) của bản ghi, hoặc chọn \"Khớp theo\" một trường không trùng", c.value, len(ids), m.targetLabel)
			case m.field != "" && !m.denied && matcher != nil:
				fail("RECORD_RELATION_INVALID", "không có %s nào có %s là %q", m.targetLabel, m.fieldLabel, c.value)
			default:
				fail("RECORD_RELATION_INVALID", "không có %s nào mang mã %q — ở bảng này chỉ khớp được theo mã", m.targetLabel, c.value)
			}
		}
	}

	kept = make([]validRow, 0, len(rows))
	for i, vr := range rows {
		if !bad[i] {
			kept = append(kept, vr)
		}
	}
	return kept, errs
}
