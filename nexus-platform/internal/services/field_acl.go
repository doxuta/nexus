package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/formula"
	"nexus/platform/internal/metadata"
)

// fieldACLLoader is the subset of the field-ACL repository the service needs: loading a role's
// per-field grants for one entity, pinned to the request's tenant. Declared consumer-side so the
// service is unit-testable with an in-memory fake and so services does not import a concrete DB
// type. The implementation (auth.FieldACLRepository.LoadFieldACLForEntity) pins the query to
// tenant_id via a JOIN to acl_roles, so a role id from another tenant returns zero rows →
// default-allow (no restriction), and a forged tenant can never read another tenant's grants.
type fieldACLLoader interface {
	LoadFieldACLForEntity(ctx context.Context, tenantID, roleID, entity string) ([]auth.FieldACL, error)
}

// FieldACLService enforces field-level security (RM-42 / A6) on the dynamic record surface. It
// sits ABOVE the A4 module ACL and A5 row scope: those decide whether the role may touch the
// entity at all and which rows; this decides which FIELDS within an allowed record the role may
// SEE (read strip) and WRITE (write guard).
//
// Policy (DEFAULT-ALLOW, restrictive overlay):
//   - A field with NO sys_field_acl row for the role is fully visible+editable.
//   - A row with can_view=0 → the field is STRIPPED from every read response (get/list/
//     aggregate/export) AND cannot be written (you cannot edit what you cannot see).
//   - A row with can_edit=0 (but can_view=1) → the field is visible but a write payload that
//     includes it is rejected 403 FIELD_FORBIDDEN.
//
// is_admin BYPASSES this entirely (the caller passes isAdmin from the verified claim). The
// service holds no per-request state and is safe for concurrent use.
//
// FAIL-CLOSED: a loader error makes the read strip hide ALL business fields (over-hide, never
// over-expose) and makes the write guard reject the whole write — an infrastructure fault must
// never widen field access. The caller treats both as fail-safe.
type FieldACLService struct {
	loader fieldACLLoader
	// catalog (optional) lets loadRestrictions see which fields are formulas, so a formula whose
	// operand the role may not view is hidden with it (queue #316). nil = no derivation.
	catalog EntityCatalog
}

// NewFieldACLService wires the field-ACL enforcer over a field-grant loader.
func NewFieldACLService(loader fieldACLLoader) *FieldACLService {
	return &FieldACLService{loader: loader}
}

// WithCatalog wires the entity/field catalog used to derive hidden formula fields (queue #316).
func (s *FieldACLService) WithCatalog(c EntityCatalog) *FieldACLService {
	s.catalog = c
	return s
}

// loadRestrictions returns two sets for (tenant, role, entity): fields the role may NOT view
// (hidden) and fields the role may NOT edit (locked). A can_view=0 row implies the field is also
// locked (can't edit what you can't see), so a hidden field is always in BOTH sets regardless of
// its stored can_edit. Default-allow: a field absent from sys_field_acl is in NEITHER set.
func (s *FieldACLService) loadRestrictions(ctx context.Context, tenantID, roleID, entity string) (hidden, locked map[string]struct{}, err error) {
	rows, lerr := s.loader.LoadFieldACLForEntity(ctx, tenantID, roleID, entity)
	if lerr != nil {
		return nil, nil, fmt.Errorf("services.FieldACLService.loadRestrictions: %w", lerr)
	}
	hidden = make(map[string]struct{})
	locked = make(map[string]struct{})
	for _, r := range rows {
		if !r.CanView {
			hidden[r.FieldName] = struct{}{}
			locked[r.FieldName] = struct{}{} // can't edit what you can't see
			continue
		}
		if !r.CanEdit {
			locked[r.FieldName] = struct{}{}
		}
	}
	if len(hidden) > 0 && s.catalog != nil {
		derived, derr := s.hiddenFormulaFields(ctx, tenantID, entity, hidden)
		if derr != nil {
			return nil, nil, fmt.Errorf("services.FieldACLService.loadRestrictions: %w", derr)
		}
		for _, f := range derived {
			hidden[f] = struct{}{}
			locked[f] = struct{}{}
		}
	}
	return hidden, locked, nil
}

// hiddenFormulaFields returns the formula fields of entity whose VALUE would disclose a hidden
// field (queue #316): a stored formula is computed from its operands, so serving
// `weighted_value = amount * probability / 100` while `amount` is hidden lets the caller divide the
// hidden number back out. It is the ONE predicate behind every surface — loadRestrictions feeds the
// read strip, metadata, filter/sort guard, report, export and ⌘K from the same set.
//
// Runs to a fixed point so a formula over a derived-hidden formula is hidden too (the save-time
// validator refuses formula-over-formula today, but a row saved before that rule still evaluates).
// A formula whose expression cannot be parsed is hidden: "cannot tell what it reads" must not be
// read as "reads nothing". Only called when something is hidden, so the common (unrestricted) role
// never pays for the catalog read; the catalog is the cached MetaCache in production.
func (s *FieldACLService) hiddenFormulaFields(ctx context.Context, tenantID, entity string, hidden map[string]struct{}) ([]string, error) {
	e, err := s.catalog.GetEntityByName(ctx, tenantID, entity)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return nil, nil // no such entity → no formula fields to derive
		}
		return nil, fmt.Errorf("get entity: %w", err)
	}
	defs, err := s.catalog.ListFields(ctx, tenantID, e.ID)
	if err != nil {
		return nil, fmt.Errorf("list fields: %w", err)
	}
	type formulaDef struct {
		name string
		refs []string
		bad  bool
	}
	var formulas []formulaDef
	for _, d := range defs {
		if d.FieldType != "formula" {
			continue
		}
		f := formulaDef{name: d.Name}
		var opts struct {
			Formula string `json:"formula"`
		}
		if d.Options != nil {
			if uerr := json.Unmarshal([]byte(*d.Options), &opts); uerr != nil {
				f.bad = true
			}
		}
		if !f.bad {
			refs, ierr := formula.Identifiers(opts.Formula)
			f.refs, f.bad = refs, ierr != nil
		}
		formulas = append(formulas, f)
	}
	var derived []string
	seen := make(map[string]struct{}, len(hidden))
	for k := range hidden {
		seen[k] = struct{}{}
	}
	for changed := true; changed; {
		changed = false
		for _, f := range formulas {
			if _, done := seen[f.name]; done {
				continue
			}
			hit := f.bad
			for _, r := range f.refs {
				if _, h := seen[r]; h {
					hit = true
					break
				}
			}
			if hit {
				seen[f.name] = struct{}{}
				derived = append(derived, f.name)
				changed = true
			}
		}
	}
	return derived, nil
}

// FilterReadFields strips, IN PLACE, every key of data the role may not view (RM-42 read path).
// It is applied to each record returned by get/list/aggregate-row/export AFTER the row passed
// tenant + A5 row scope. An admin caller (isAdmin) sees everything → no-op. A nil/empty data is a
// no-op.
//
// FAIL-CLOSED: when the field-grant load fails, every BUSINESS field key is removed (the system
// columns id/version/createdAt/updatedAt live on the Record struct, not in data, so they are
// unaffected) — an infra fault hides data rather than leaking a field the role might be denied.
func (s *FieldACLService) FilterReadFields(ctx context.Context, tenantID, roleID, entity string, isAdmin bool, data map[string]any) error {
	if isAdmin || len(data) == 0 {
		return nil
	}
	hidden, _, err := s.loadRestrictions(ctx, tenantID, roleID, entity)
	if err != nil {
		// Fail closed: drop all business fields rather than risk exposing a denied field.
		for k := range data {
			delete(data, k)
		}
		return err
	}
	for k := range hidden {
		delete(data, k)
	}
	return nil
}

// FilterReadFieldsBatch strips, IN PLACE, the not-viewable fields from EVERY record's data in
// one shot, loading the (tenant, role, entity) restriction set ONCE and reusing it across all
// rows (the O(1)-grant-lookup form of FilterReadFields for list/export pages). Admin bypasses.
// FAIL-CLOSED: a load error drops all business fields from every record AND returns the error.
func (s *FieldACLService) FilterReadFieldsBatch(ctx context.Context, tenantID, roleID, entity string, isAdmin bool, datas []map[string]any) error {
	if isAdmin || len(datas) == 0 {
		return nil
	}
	hidden, _, err := s.loadRestrictions(ctx, tenantID, roleID, entity)
	if err != nil {
		// Fail closed: drop all business fields from every row.
		for _, data := range datas {
			for k := range data {
				delete(data, k)
			}
		}
		return err
	}
	if len(hidden) == 0 {
		return nil
	}
	for _, data := range datas {
		for k := range hidden {
			delete(data, k)
		}
	}
	return nil
}

// CheckWriteFields rejects a write (create/update) whose payload includes any field the role may
// not edit (RM-42 write path) → ValidationError code FIELD_FORBIDDEN, which the handler maps to
// HTTP 403. A locked field is one with can_edit=0 OR can_view=0 (the latter is also un-editable).
// An admin caller bypasses (no-op). An empty payload is a no-op.
//
// A key naming a HIDDEN field (can_view=0) is NOT answered FIELD_FORBIDDEN (queue #317, §4.4b — that
// confirms the field exists); it is refused as RECORD_UNKNOWN_FIELD, the answer a name the entity
// never declared gets, and only after every visible-but-locked key had its chance to refuse. See
// hiddenKeyRefusal / finishHiddenKeyRefusal for how the write doors keep multi-key payloads identical.
//
// FAIL-CLOSED: a field-grant load failure rejects the whole write (FIELD_FORBIDDEN with a generic
// message) rather than letting a locked field through on a transient infra fault.
// CheckWriteFieldsAgainst is CheckWriteFields with the record's CURRENT values, so a locked field
// whose submitted value is IDENTICAL to what is stored is not treated as an edit.
//
// Without this, read-only field-ACL made a record unsavable rather than partially editable. The
// form sends every field on update (buildPayload's "update" mode is what lets a user CLEAR a
// value), so a single read-only field put that field in every payload, and every save came back
// 403 FIELD_FORBIDDEN — including saves that touched nothing but the fields the user was allowed
// to change. Marking one field read-only silently took the whole record away.
//
// Rejecting a no-op is not enforcement: nothing is being written. What must still be refused, and
// is, is a payload that carries a DIFFERENT value for a locked field.
//
// current == nil falls back to the strict behaviour (any mention is a refusal), which is right for
// create — there is nothing to be unchanged from.
//
// It MUTATES payload in ONE case: a HIDDEN field (can_view=0) sent as nil is deleted from it. The
// read path strips hidden fields, so the caller never saw the stored value — the edit form seeds
// the field empty and buildPayload's update mode resends every empty optional field as null. That
// null cannot express an intent to clear something the caller could not see; it is the absence of
// a value. Counting it as an edit (nil ≠ the stored 9000 → 403) made every record holding a value
// in a hidden field unsavable — hiding one field took the whole record away, the same bug the
// unchanged-value rule above fixed for read-only fields. Skipping the key is not enough: payload
// flows on into ValidatePartialRecord and the UPDATE, where an explicit null REMOVES the stored
// key (JSON_MERGE_PATCH), so the key itself must go. This holds even when current is nil (the
// record read failed): dropping a nil for a hidden field never writes anything, so fail-closed
// loses nothing.
//
// A hidden field carrying a NON-nil value is ALWAYS refused — even one equal to the stored value.
// The unchanged-value rule does not apply to hidden fields: current is the UNSTRIPPED row and the
// read path never served the field, so an equal value is a correct guess, not a resend, and
// passing it (200) while refusing every other value (403) would make this guard an equality
// oracle on the one field the role may not see (the same reasoning as AssertFieldsVisible). The
// unchanged-value rule is for VISIBLE-but-locked fields only; such a field sent as nil against a
// stored value is still refused — the caller saw that value, so nil there IS a change.
func (s *FieldACLService) CheckWriteFieldsAgainst(ctx context.Context, tenantID, roleID, entity string, isAdmin bool, payload, current map[string]any) error {
	if isAdmin || len(payload) == 0 {
		return nil
	}
	hidden, locked, err := s.loadRestrictions(ctx, tenantID, roleID, entity)
	if err != nil {
		return invalid("FIELD_FORBIDDEN", "không thể xác minh quyền sửa field; thao tác bị từ chối")
	}
	var hiddenHit []string
	for k, v := range payload {
		if _, isLocked := locked[k]; !isLocked {
			continue
		}
		if _, isHidden := hidden[k]; isHidden {
			if v == nil {
				// Never seen by the caller → nil is "nothing", not "clear". Drop it so the UPDATE
				// cannot NULL the column the caller is not even allowed to see.
				delete(payload, k)
				continue
			}
			// No unchanged-value exemption for a hidden field: "equal to stored" is a guess the
			// caller could never have read back, and honouring it would be an equality oracle.
			hiddenHit = append(hiddenHit, k)
			continue
		}
		if current != nil && looseSameValue(current[k], v) {
			continue // unchanged — the write would be a no-op
		}
		return invalidField("FIELD_FORBIDDEN", k, "bạn không có quyền sửa field %q", k)
	}
	return hiddenKeyRefusal(hiddenHit, hidden)
}

// hiddenKeyRefusal is the answer to a payload naming fields the caller may not VIEW (queue #317,
// api-conventions §4.4b): exactly what a name that does not exist gets, RECORD_UNKNOWN_FIELD with no
// details — never FIELD_FORBIDDEN, which confirmed the field exists. It is returned only AFTER every
// visible-but-locked key had its chance to refuse, so {hidden, locked} and {missing, locked} read
// the same too. nil when hit is empty. The returned error carries the hidden set so the write door
// can finish the refusal through its own validator (see ValidationError.hiddenKeys).
func hiddenKeyRefusal(hit []string, hidden map[string]struct{}) error {
	if len(hit) == 0 {
		return nil
	}
	sort.Strings(hit)
	ve := unknownFieldError(hit[0])
	ve.hiddenKeys = hidden
	return ve
}

// unknownFieldError is THE refusal for a payload key the entity does not declare — one constructor
// for ValidateRecord, ValidatePartialRecord and the hidden-key guard, so the two can never drift
// into a different shape (a drift is the §4.4b oracle).
func unknownFieldError(key string) *ValidationError {
	return invalid("RECORD_UNKNOWN_FIELD", "field %q không tồn tại trong metadata", key).(*ValidationError)
}

// finishHiddenKeyRefusal turns a write-guard refusal that came from a HIDDEN key into the exact
// error the door's own validator gives an undeclared key. validate re-runs that validator with the
// hidden names treated as undeclared; payloads naming several bad keys then name the same one an
// unknown-only payload would (the strict key loops range over a map, so the key reported is
// randomized — a fixed answer for hidden names would be a statistical oracle). Any other error, or
// a validator that unexpectedly passes, returns err untouched: still RECORD_UNKNOWN_FIELD, still safe.
func finishHiddenKeyRefusal(err error, validate func(hidden map[string]struct{}) error) error {
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.hiddenKeys == nil {
		return err
	}
	if verr := validate(ve.hiddenKeys); verr != nil {
		return verr
	}
	return err
}

// looseSameValue reports whether two field values are the same for the purpose of "did this write
// change anything". JSON round-trips lose the distinction between 5 and 5.0 and re-order object
// keys, so a byte comparison would call an unchanged value changed and reintroduce the bug it is
// here to prevent. Comparing marshalled forms after a normalising round-trip is enough: both sides
// come from the same JSON pipeline.
func looseSameValue(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	ab, aerr := json.Marshal(a)
	bb, berr := json.Marshal(b)
	if aerr != nil || berr != nil {
		return false // cannot prove they are equal → treat as a change and refuse
	}
	if string(ab) == string(bb) {
		return true
	}
	var av, bv any
	if json.Unmarshal(ab, &av) != nil || json.Unmarshal(bb, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func (s *FieldACLService) CheckWriteFields(ctx context.Context, tenantID, roleID, entity string, isAdmin bool, payload map[string]any) error {
	if isAdmin || len(payload) == 0 {
		return nil
	}
	hidden, locked, err := s.loadRestrictions(ctx, tenantID, roleID, entity)
	if err != nil {
		return invalid("FIELD_FORBIDDEN", "không thể xác minh quyền sửa field; thao tác bị từ chối")
	}
	var hiddenHit []string
	for k := range payload {
		if _, isHidden := hidden[k]; isHidden {
			hiddenHit = append(hiddenHit, k) // answered like an unknown name, after the visible-locked keys
			continue
		}
		if _, isLocked := locked[k]; isLocked {
			return invalidField("FIELD_FORBIDDEN", k, "bạn không có quyền sửa field %q", k)
		}
	}
	return hiddenKeyRefusal(hiddenHit, hidden)
}

// AssertFieldVisible rejects (FIELD_FORBIDDEN → 403) when the role may not view fieldName on the
// entity. Used by paths that operate on a SINGLE named field a hidden field must not leak through
// — e.g. an aggregate (SUM/AVG/MIN/MAX/group-by) over a column the role cannot see, where the
// metric value itself would disclose the hidden data. An admin bypasses; an empty fieldName is a
// no-op (e.g. COUNT(*) names no field). FAIL-CLOSED on a load error (deny).
func (s *FieldACLService) AssertFieldVisible(ctx context.Context, tenantID, roleID, entity string, isAdmin bool, fieldName string) error {
	if isAdmin || fieldName == "" {
		return nil
	}
	hidden, _, err := s.loadRestrictions(ctx, tenantID, roleID, entity)
	if err != nil {
		return invalid("FIELD_FORBIDDEN", "không thể xác minh quyền xem field; thao tác bị từ chối")
	}
	if _, isHidden := hidden[fieldName]; isHidden {
		return invalidField("FIELD_FORBIDDEN", fieldName, "bạn không có quyền xem field %q", fieldName)
	}
	return nil
}

// AssertFieldsVisible rejects (FIELD_FORBIDDEN → 403) when the role may not view ANY of fieldNames
// on the entity, loading the restriction set ONCE. Used to block a non-admin from FILTERING or
// SORTING on a hidden field — which would otherwise be a probing oracle (a binary-search of
// eq/gt/lt filters reconstructs a hidden value from row counts even though the field is stripped
// from the response). Empty names in the slice are skipped. Admin bypasses; FAIL-CLOSED on error.
func (s *FieldACLService) AssertFieldsVisible(ctx context.Context, tenantID, roleID, entity string, isAdmin bool, fieldNames []string) error {
	if isAdmin || len(fieldNames) == 0 {
		return nil
	}
	hidden, _, err := s.loadRestrictions(ctx, tenantID, roleID, entity)
	if err != nil {
		return invalid("FIELD_FORBIDDEN", "không thể xác minh quyền xem field; thao tác bị từ chối")
	}
	if len(hidden) == 0 {
		return nil
	}
	for _, name := range fieldNames {
		if name == "" {
			continue
		}
		if _, isHidden := hidden[name]; isHidden {
			return invalidField("FIELD_FORBIDDEN", name, "bạn không có quyền lọc/sắp xếp theo field %q", name)
		}
	}
	return nil
}

// HiddenFields returns the set of field names the role may not VIEW on the entity. The caller uses
// it to drop hidden columns from a multi-field ?q search (so the search silently does not match on
// a hidden column) rather than rejecting the whole search. An admin gets an empty set (sees all).
// FAIL-CLOSED: a load error returns the error so the caller can refuse the search rather than
// search a field it could not confirm is visible.
func (s *FieldACLService) HiddenFields(ctx context.Context, tenantID, roleID, entity string, isAdmin bool) (map[string]struct{}, error) {
	if isAdmin {
		return map[string]struct{}{}, nil
	}
	hidden, _, err := s.loadRestrictions(ctx, tenantID, roleID, entity)
	if err != nil {
		return nil, err
	}
	return hidden, nil
}

// LockedFields returns the set of field names the role may not EDIT on the entity — the SAME
// `locked` set CheckWriteFields refuses with, exported so a read surface can SAY IT BEFORE the
// write (field-lock-signal: GET /metadata/fields/:entity marks each such field `readOnly` for the
// caller, and the form renders it as text instead of an input that will 403 on save). A hidden
// field (can_view=0) is in this set too — can't edit what you can't see — so a caller that serves
// field names MUST strip hidden first and only then mark locked; nothing here changes what is
// hidden. An admin gets an empty set (edits all). FAIL-CLOSED: a load error is returned so the
// caller refuses the whole answer rather than serving a list that claims "editable" unverified.
func (s *FieldACLService) LockedFields(ctx context.Context, tenantID, roleID, entity string, isAdmin bool) (map[string]struct{}, error) {
	if isAdmin {
		return map[string]struct{}{}, nil
	}
	_, locked, err := s.loadRestrictions(ctx, tenantID, roleID, entity)
	if err != nil {
		return nil, err
	}
	return locked, nil
}

// WriteBlockedFields splits the fields the role may not write on the entity in two, from ONE grant
// load: hidden (can_view=0 — the caller may not even know they exist) and readOnly (visible but
// locked: exactly the set GET /metadata/fields/:entity marks `readOnly` for this caller, the
// fields the form renders as text and never sends). The create path uses it to explain a required
// field the caller cannot fill (queue #62) and to keep a HIDDEN one unnamed (queue #143,
// api-conventions §4.4b). An admin gets two empty sets. FAIL-CLOSED: a load error is returned,
// never empty sets that would read as "nothing is locked, nothing is hidden".
func (s *FieldACLService) WriteBlockedFields(ctx context.Context, tenantID, roleID, entity string, isAdmin bool) (hidden, readOnly map[string]struct{}, err error) {
	if isAdmin {
		return map[string]struct{}{}, map[string]struct{}{}, nil
	}
	hidden, locked, err := s.loadRestrictions(ctx, tenantID, roleID, entity)
	if err != nil {
		return nil, nil, err
	}
	readOnly = make(map[string]struct{}, len(locked))
	for f := range locked {
		if _, isHidden := hidden[f]; !isHidden {
			readOnly[f] = struct{}{}
		}
	}
	return hidden, readOnly, nil
}

// requiredEngineCannotFill reports whether def has the shape a field-ACL lock turns into "this
// role can create nothing": required, written by clients (not server-assigned — formula/lookup/
// rollup/auto_number), and with no default the engine materializes at Create (defaultFills, the
// function ValidateRecord runs — a stored default the engine drops is no default). ONE predicate
// for every door to that state: the PutGrants guard (lock after required), the EditField guard
// (required after lock, queue #142) and Doctor C9 (rows that got there anyway, queue #143).
func requiredEngineCannotFill(def metadata.FieldDefinition) (bool, error) {
	if !def.IsRequired || ComputedReadonlyFieldType(def.FieldType) {
		return false, nil
	}
	specs, err := toFieldSpecs([]metadata.FieldDefinition{def})
	if err != nil {
		return false, fmt.Errorf("services.requiredEngineCannotFill: %w", err)
	}
	return !defaultFills(specs[0]), nil
}

// lockedRequiredRemedy is the admin's way out of "a required field this role may not write, that
// the engine does not fill" — the dead end the PutGrants guard refuses and the create path
// explains (queue #62). It names only exits an admin can take: turning off "bắt buộc" and leaving
// the field editable always work; a default only when studioCanSetDefault says so.
func lockedRequiredRemedy(fieldType string) string {
	if !studioCanSetDefault(fieldType) {
		return fmt.Sprintf("bỏ \"bắt buộc\" của field, hoặc để field sửa được cho vai này (Studio không đặt được giá trị mặc định cho kiểu %q)", fieldType)
	}
	return "đặt giá trị mặc định cho field, bỏ \"bắt buộc\" của field, hoặc để field sửa được cho vai này"
}

// studioCanSetDefault reports whether an admin can give a field of this type a default from Studio:
// the engine must accept one (fieldTypeRejectsDefault — the list ValidateFieldDefaultNotSupported
// enforces) AND the field builder must have an input for it. Since queue #320 the builder has a
// record picker for relation, so the engine's rule is the only rule. It drives both the remedy text
// and details.defaultSupported, so they never disagree. Mirror: nexus-web/lib/fieldOptions.ts
// defaultInputKind.
func studioCanSetDefault(fieldType string) bool {
	return !fieldTypeRejectsDefault(fieldType)
}
