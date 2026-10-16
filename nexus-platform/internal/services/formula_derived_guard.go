package services

import (
	"context"
	"fmt"
	"strings"

	"nexus/platform/internal/formula"
	"nexus/platform/internal/metadata"
)

// GuardFormulaBecomesDerived refuses (422 FIELD_ROLLUP_AGG_FIELD_FORMULA_DERIVED) editing a stored
// formula that a rollup aggregates into one that reads a lookup/rollup: it would stop being
// written to the record, so the rollup's SUM would read an empty column. The create-time twin is
// in validateRollupCrossEntity. siblings is the entity's field list the edit was vetted against.
func GuardFormulaBecomesDerived(ctx context.Context, finder RollupAggFinder, tenantID, entityName string, stored metadata.FieldDefinition, storedOpts, newOpts map[string]any, siblings []metadata.FieldDefinition) error {
	if finder == nil || stored.FieldType != "formula" || newOpts == nil {
		return nil
	}
	infos := formulaFieldInfos(siblings, nil)
	before, _ := storedOpts["formula"].(string)
	after, _ := newOpts["formula"].(string)
	if len(formula.Derived(after, infos)) == 0 || len(formula.Derived(before, infos)) > 0 {
		return nil
	}
	users, err := finder.ListRollupsAggregating(ctx, tenantID, entityName, stored.Name)
	if err != nil {
		return fmt.Errorf("services.GuardFormulaBecomesDerived: %w", err)
	}
	if len(users) == 0 {
		return nil
	}
	label := stored.Label
	if label == "" {
		label = stored.Name
	}
	return &ValidationError{
		Code: "FIELD_ROLLUP_AGG_FIELD_FORMULA_DERIVED",
		Msg: fmt.Sprintf("Không sửa được công thức %q thành công thức đọc trường tra cứu/tổng hợp: trường tổng hợp %s đang cộng theo nó, mà giá trị kiểu này chỉ tính khi xem nên không cộng gộp được. Gỡ trường tổng hợp đó trước, hoặc tạo công thức mới.",
			label, strings.Join(users, ", ")),
		Field:   stored.Name,
		Details: map[string]any{"field": stored.Name, "rollups": users},
	}
}
