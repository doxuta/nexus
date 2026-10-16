// fieldSpec — chuẩn hoá FieldMeta (API) → FieldSpec (widget) + merge layout-cell override.
// Doc §2.3: layout chỉ tham chiếu field bằng TÊN; type/required/options gốc từ listFields(); cell
// override (labelOverride/required/readonly) đè lên. Tách riêng (pure) để unit-test + CellRenderer dùng.

import type { FieldMeta, FieldCell } from "@/lib/api";
import type { FieldSpec, FieldType, FieldOptions } from "@/lib/metadata";

// Base mapping: FieldMeta (snake API shape) → FieldSpec (what FieldInput/FieldDisplay consume).
export function metaToSpec(base: FieldMeta): FieldSpec {
  return {
    name: base.name,
    label: base.label ?? base.name,
    fieldType: base.fieldType as FieldType,
    required: base.isRequired ?? false,
    unique: base.isUnique ?? false,
    options: base.options as FieldOptions | undefined,
    // W5: per-FIELD admin description → FieldRow renders it as a "?" tooltip next to the label.
    description: base.description || undefined,
  };
}

// Merge a layout cell's per-cell overrides on top of the base field meta (doc §2.3 floor-rule:
// layout may tighten `required` and override label/readonly; type/options always come from meta).
// W5: cell.helpText (per-VIEW, contract §2.3) → spec.helpText, rendered under the widget.
// field-lock-signal: `base.readOnly` is the CALLER's field-ACL lock (can_edit=0) served by
// GET /metadata/fields/:entity — a permission, not a layout choice, so it ORs with the cell's
// readonly and a cell can never unlock it. Either source lands in CellRenderer's read-only branch
// (label + text, no input).
export function mergeFieldSpec(base: FieldMeta, cell: FieldCell): FieldSpec {
  const spec = metaToSpec(base);
  return {
    ...spec,
    label: cell.labelOverride ?? spec.label,
    required: cell.required ?? spec.required ?? false,
    readonly: !!base.readOnly || (cell.readonly ?? false),
    helpText: cell.helpText || undefined,
  };
}
