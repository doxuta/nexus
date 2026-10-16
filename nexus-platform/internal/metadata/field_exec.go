package metadata

import (
	"context"
	"fmt"
)

// UpdateFieldMutableExec writes the editable attributes of a field (label, isRequired, defaultValue,
// options, fieldOrder, description) through any Execer — the same statement UpdateFieldMutable
// issues through GORM, here for the publish transaction (specs/063-draft-publish S7-8). Both WHERE
// terms re-pin id AND tenant_id: a forged id of another tenant matches 0 rows -> ErrFieldNotFound.
// A soft-deleted field is not updated.
func UpdateFieldMutableExec(ctx context.Context, ex Execer, tenantID string, f *FieldDefinition) error {
	res, err := ex.ExecContext(ctx, `UPDATE sys_fields
		SET label = ?, is_required = ?, default_val = ?, options = ?, field_order = ?, description = ?, updated_at = NOW()
		WHERE id = ? AND tenant_id = ? AND deleted_at IS NULL`,
		f.Label, f.IsRequired, f.DefaultVal, f.Options, f.FieldOrder, f.Description, f.ID, tenantID)
	if err != nil {
		return fmt.Errorf("metadata.UpdateFieldMutableExec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("metadata.UpdateFieldMutableExec: rows affected: %w", err)
	}
	if n == 0 {
		return ErrFieldNotFound
	}
	return nil
}
