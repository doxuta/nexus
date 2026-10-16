package services

// pii-sensitive-audit-redact (queue #19, ../specs/029-audit-log/spec.md §5): a field flagged
// `options.sensitive = true` must never have its VALUE copied verbatim into audit_log.diff.
//
// These tests sit at the EMIT SEAM — a capturing Auditor receives exactly the bytes the async
// writer would persist — because that is the write-side invariant the slice claims. The read
// side (admin list / record history) is deliberately not involved: `is_admin` bypasses ACL, so
// nothing an admin does could prove or disprove which bytes were written.
//
// The key must SURVIVE redaction on both sides. Blanking the input maps before ComputeDiff
// would make before == after, DeepEqual would call the field unchanged, and the key would
// vanish from the diff entirely — destroying the "this field was changed, by whom, when"
// signal the audit trail exists for. Every case below asserts presence AND masking.

import (
	"context"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// sensitiveFixture wires a "customers" entity with one ordinary field and one flagged
// sensitive — the shape an admin produces by ticking "Trường nhạy cảm" in Studio.
func sensitiveFixture(t *testing.T) *fakeRecordCatalog {
	t.Helper()
	return &fakeRecordCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-cus", TenantID: "tenant-1", Name: "customers", DataTable: "obj_customers"},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text"},
			{Name: "so_cccd", FieldType: "text", Options: jsonOptsPtr(t, map[string]any{"sensitive": true})},
		},
	}
}

// sensitiveStore serves a canned before-image so Update/Delete/Restore produce a two-sided or
// before-only diff without a database.
type sensitiveStore struct {
	fakeRecordStore
	record *dynrepo.Record
}

func (s *sensitiveStore) GetRecord(_ context.Context, _, _, _ string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	return s.record, nil
}

func (s *sensitiveStore) InsertRecord(_ context.Context, _, _, _ string, _ map[string]any, _ []dynrepo.UniqueFieldSpec) (string, error) {
	return "rec-1", nil
}

func sensitiveSvc(t *testing.T, store RecordStore) (*RecordService, *captureAuditor) {
	t.Helper()
	aud := &captureAuditor{}
	return NewRecordService(sensitiveFixture(t), store).WithAuditor(aud), aud
}

func soleEvent(t *testing.T, aud *captureAuditor) audit.Event {
	t.Helper()
	aud.mu.Lock()
	defer aud.mu.Unlock()
	if len(aud.events) != 1 {
		t.Fatalf("want exactly 1 audit event, got %d", len(aud.events))
	}
	return aud.events[0]
}

// wantMasked asserts the key is STILL THERE (the change stays attributable) and its value is
// the redaction marker, never the real one.
func wantMasked(t *testing.T, side map[string]any, key, realValue string) {
	t.Helper()
	got, ok := side[key]
	if !ok {
		t.Fatalf("key %q must survive redaction (a dropped key destroys the change signal); side = %v", key, side)
	}
	if got == realValue {
		t.Fatalf("key %q leaked its real value %q into the audit diff", key, realValue)
	}
	if got != auditRedactedMarker {
		t.Fatalf("key %q = %v, want %q", key, got, auditRedactedMarker)
	}
}

func TestAuditRedact_UpdateMasksSensitiveOnBothSides(t *testing.T) {
	store := &sensitiveStore{record: &dynrepo.Record{
		ID: "rec-1", Data: map[string]any{"name": "Trần Văn A", "so_cccd": "001099001122"},
	}}
	svc, aud := sensitiveSvc(t, store)

	if err := svc.Update(context.Background(), nonAdminScope(), "tenant-1", "customers", "rec-1",
		map[string]any{"name": "Trần Văn B", "so_cccd": "079203004455"}, nil); err != nil {
		t.Fatalf("Update: %v", err)
	}

	evt := soleEvent(t, aud)
	if evt.Action != audit.ActionUpdate || evt.Diff == nil {
		t.Fatalf("want an update event with a diff, got %+v", evt)
	}
	wantMasked(t, evt.Diff.Before, "so_cccd", "001099001122")
	wantMasked(t, evt.Diff.After, "so_cccd", "079203004455")
	if evt.Diff.After["name"] != "Trần Văn B" {
		t.Errorf("a non-sensitive field must pass through untouched; name = %v", evt.Diff.After["name"])
	}
}

func TestAuditRedact_CreateMasksSensitiveAfterValue(t *testing.T) {
	svc, aud := sensitiveSvc(t, &sensitiveStore{})

	if _, err := svc.Create(context.Background(), nonAdminScope(), "tenant-1", "u1", "customers",
		map[string]any{"name": "Trần Văn A", "so_cccd": "001099001122"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	evt := soleEvent(t, aud)
	wantMasked(t, evt.Diff.After, "so_cccd", "001099001122")
	if evt.Diff.After["name"] != "Trần Văn A" {
		t.Errorf("a non-sensitive field must pass through untouched; name = %v", evt.Diff.After["name"])
	}
}

func TestAuditRedact_DeleteMasksSensitiveBeforeValue(t *testing.T) {
	store := &sensitiveStore{record: &dynrepo.Record{
		ID: "rec-1", Data: map[string]any{"name": "Trần Văn A", "so_cccd": "001099001122"},
	}}
	svc, aud := sensitiveSvc(t, store)

	if err := svc.Delete(context.Background(), nonAdminScope(), "tenant-1", "customers", "rec-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	evt := soleEvent(t, aud)
	wantMasked(t, evt.Diff.Before, "so_cccd", "001099001122")
}

func TestAuditRedact_RestoreMasksSensitiveAfterValue(t *testing.T) {
	store := &sensitiveStore{record: &dynrepo.Record{
		ID: "rec-1", Data: map[string]any{"name": "Trần Văn A", "so_cccd": "001099001122"},
	}}
	svc, aud := sensitiveSvc(t, store)

	if err := svc.Restore(context.Background(), nonAdminScope(), "tenant-1", "customers", "rec-1"); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	evt := soleEvent(t, aud)
	wantMasked(t, evt.Diff.After, "so_cccd", "001099001122")
}

// The helper itself: a nil diff (nothing changed) stays nil, and a one-sided diff does not get
// an empty counterpart map invented for it — ComputeDiff nils the empty side and assigning into
// a nil map panics.
func TestRedactSensitiveDiff_NilAndOneSidedAreSafe(t *testing.T) {
	specs := []FieldSpec{{Name: "so_cccd", FieldType: "text", Options: map[string]any{"sensitive": true}}}

	if got := redactSensitiveDiff(specs, nil); got != nil {
		t.Errorf("nil diff must stay nil, got %+v", got)
	}

	oneSided := audit.ComputeDiff(nil, map[string]any{"so_cccd": "001099001122"})
	got := redactSensitiveDiff(specs, oneSided)
	if got.Before != nil {
		t.Errorf("an after-only diff must keep an absent before side, got %v", got.Before)
	}
	wantMasked(t, got.After, "so_cccd", "001099001122")

	// No sensitive field declared → the diff is returned untouched.
	plain := audit.ComputeDiff(nil, map[string]any{"so_cccd": "001099001122"})
	if out := redactSensitiveDiff([]FieldSpec{{Name: "so_cccd", FieldType: "text"}}, plain); out.After["so_cccd"] != "001099001122" {
		t.Errorf("an unflagged field must not be masked; got %v", out.After["so_cccd"])
	}
}

// ── Hardening found by the pre-commit review pass ───────────────────────────────────────────────

// A field can be flagged sensitive and LATER soft-deleted from the app. Its VALUES stay in the
// obj_* JSON blob (field delete is soft: the definition goes, the data stays), and delete/restore
// diff that raw blob rather than a validated payload. ListFields hides soft-deleted definitions,
// so the flag would be invisible exactly when the value is still there — a 500-record bulk delete
// would copy every real số CCCD into the append-only trail, which the CSV export serves with no
// read-side field filter at all. The redaction spec list must therefore see soft-deleted fields.
type softDeletedFieldCatalog struct {
	*fakeRecordCatalog
	deleted []metadata.FieldDefinition
}

func (c *softDeletedFieldCatalog) ListFieldsIncludingDeleted(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	return append(append([]metadata.FieldDefinition{}, c.fields...), c.deleted...), nil
}

func softDeletedSensitiveSvc(t *testing.T, store RecordStore) (*RecordService, *captureAuditor) {
	t.Helper()
	// so_cccd is flagged sensitive but NO LONGER in the live field list — only in the
	// including-deleted list, which is what a soft-deleted field looks like.
	live := &fakeRecordCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-cus", TenantID: "tenant-1", Name: "customers", DataTable: "obj_customers"},
		fields: []metadata.FieldDefinition{{Name: "name", FieldType: "text"}},
	}
	cat := &softDeletedFieldCatalog{
		fakeRecordCatalog: live,
		deleted: []metadata.FieldDefinition{
			{Name: FIELDSensitive, FieldType: "text", Options: jsonOptsPtr(t, map[string]any{"sensitive": true})},
		},
	}
	aud := &captureAuditor{}
	return NewRecordService(cat, store).WithAuditor(aud), aud
}

const FIELDSensitive = "so_cccd"

func TestAuditRedact_SoftDeletedSensitiveField_StillMaskedOnDelete(t *testing.T) {
	store := &sensitiveStore{record: &dynrepo.Record{
		ID: "rec-1", Data: map[string]any{"name": "Trần Văn A", "so_cccd": "001099001122"},
	}}
	svc, aud := softDeletedSensitiveSvc(t, store)

	if err := svc.Delete(context.Background(), nonAdminScope(), "tenant-1", "customers", "rec-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	wantMasked(t, soleEvent(t, aud).Diff.Before, "so_cccd", "001099001122")
}

func TestAuditRedact_SoftDeletedSensitiveField_StillMaskedOnRestore(t *testing.T) {
	store := &sensitiveStore{record: &dynrepo.Record{
		ID: "rec-1", Data: map[string]any{"name": "Trần Văn A", "so_cccd": "001099001122"},
	}}
	svc, aud := softDeletedSensitiveSvc(t, store)

	if err := svc.Restore(context.Background(), nonAdminScope(), "tenant-1", "customers", "rec-1"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	wantMasked(t, soleEvent(t, aud).Diff.After, "so_cccd", "001099001122")
}

// A catalog with no including-deleted capability must still redact everything it CAN see, rather
// than falling back to no redaction at all.
func TestAuditRedact_CatalogWithoutDeletedLister_StillRedactsLiveFields(t *testing.T) {
	store := &sensitiveStore{record: &dynrepo.Record{
		ID: "rec-1", Data: map[string]any{"name": "Trần Văn A", "so_cccd": "001099001122"},
	}}
	svc, aud := sensitiveSvc(t, store) // plain fakeRecordCatalog — no ListFieldsIncludingDeleted

	if err := svc.Delete(context.Background(), nonAdminScope(), "tenant-1", "customers", "rec-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	wantMasked(t, soleEvent(t, aud).Diff.Before, "so_cccd", "001099001122")
}

// A security flag that fails OPEN when mis-typed is worse than no flag: boolOpt returns false for
// anything that is not a real bool, so `{"sensitive": "true"}` (a string — what a hand-written
// curl, an AI-proposed field or a pack manifest produces) would store happily, read back false,
// and stream PII into the trail with no 422, no log and no badge anywhere. Reject it at config
// time, for every field type, so the only way to have the key is to have it working.
func TestValidateFieldConfig_SensitiveMustBeBool(t *testing.T) {
	for _, bad := range []any{"true", "false", 1, float64(1), map[string]any{}} {
		if err := validateFieldConfig("so_cccd", "text", map[string]any{"sensitive": bad}); err == nil {
			t.Errorf("options.sensitive = %#v (%T) must be rejected, got nil error", bad, bad)
		}
	}
	for _, ok := range []any{true, false} {
		if err := validateFieldConfig("so_cccd", "text", map[string]any{"sensitive": ok}); err != nil {
			t.Errorf("options.sensitive = %v must be accepted, got %v", ok, err)
		}
	}
	// The check is type-wide, not bolted onto the text branch: a date of birth and a salary are
	// as sensitive as an ID number.
	for _, ft := range []string{"date", "datetime", "email", "number", "boolean"} {
		if err := validateFieldConfig("f", ft, map[string]any{"sensitive": "true"}); err == nil {
			t.Errorf("fieldType %q: mis-typed sensitive must be rejected", ft)
		}
	}
	// Absent key stays absent-ok (every field that never heard of the flag must still validate).
	if err := validateFieldConfig("f", "text", map[string]any{"maxLength": float64(10)}); err != nil {
		t.Errorf("absent sensitive must not be an error, got %v", err)
	}
}
