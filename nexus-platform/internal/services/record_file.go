package services

import (
	"context"
	"fmt"
)

// ErrNotFileField is returned by ResolveFileField when the named field exists on the entity but
// is not a file/image attachment field — uploading to a non-file field is a 422 client error.
var ErrNotFileField = invalid("FIELD_NOT_FILE", "field không phải kiểu file/image")

// ResolveFileField verifies, for the file API, that:
//   - the entity exists (else ErrEntityNotFound),
//   - the named field is a declared file/image field on it (else RECORD_UNKNOWN_FIELD / FIELD_NOT_FILE),
//   - the record is readable by the caller under tenant + A5 row scope (else ErrRecordNotFound).
//
// It is the single authorization chokepoint the FileService calls before any upload/list/delete:
// a caller who cannot READ the parent record (wrong tenant, or A5 scope hides it) gets
// ErrRecordNotFound, exactly like the record GET — so file access never leaks a record the caller
// may not see. Returns the resolved entity's canonical name on success.
func (s *RecordService) ResolveFileField(ctx context.Context, scope RecordScope, tenantID, entityName, recordID, fieldName string) (entity string, err error) {
	e, specs, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return "", err
	}
	var spec *FieldSpec
	for i := range specs {
		if specs[i].Name == fieldName {
			spec = &specs[i]
			break
		}
	}
	if spec == nil {
		return "", invalid("RECORD_UNKNOWN_FIELD", "field %q không tồn tại trong metadata", fieldName)
	}
	if spec.FieldType != "file" && spec.FieldType != "image" {
		return "", ErrNotFileField
	}
	// Authorize: the record must be readable under tenant + A5 row scope. A record the caller
	// may not see surfaces as ErrRecordNotFound (no cross-team/cross-tenant existence oracle).
	if _, gerr := s.store.GetRecord(ctx, e.Name, tenantID, recordID, scope.toRowScope()); gerr != nil {
		return "", gerr
	}
	return e.Name, nil
}

// SetFileRefs writes the file field's reference array ([{id,name}] objects) into the record's
// data JSON, going through the normal partial-update path (validation via checkFile, A5 scope,
// audit, workflow) so the file field stays consistent with the catalog. refs is the full,
// authoritative list for the field (the FileService rebuilds it from sys_files after each
// upload/delete). An empty refs slice clears the field. The update is unconditional
// (last-writer-wins) — file mutations are not version-pinned. The caller has already authorized
// the record via ResolveFileField.
func (s *RecordService) SetFileRefs(ctx context.Context, scope RecordScope, tenantID, entityName, recordID, fieldName string, refs []map[string]any) error {
	// Build the partial payload carrying just the file field. When there are no remaining
	// attachments, send an explicit JSON null so ValidatePartialRecord clears the optional
	// field (JSON_MERGE_PATCH removes the key); a non-empty list is sent as the [{id,name}]
	// array. (An empty [] would be treated as "no-op empty" by the partial-update contract and
	// leave a stale value, so null is the correct clear signal. A required file field rejects
	// the clear with RECORD_REQUIRED_MISSING — sensible: it must keep at least one attachment.)
	var payload map[string]any
	if len(refs) == 0 {
		payload = map[string]any{fieldName: nil}
	} else {
		arr := make([]any, len(refs))
		for i, r := range refs {
			arr[i] = r
		}
		payload = map[string]any{fieldName: arr}
	}
	if err := s.Update(ctx, scope, tenantID, entityName, recordID, payload, nil); err != nil {
		return fmt.Errorf("services.SetFileRefs: %w", err)
	}
	return nil
}
