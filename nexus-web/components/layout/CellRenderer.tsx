"use client";

// CellRenderer — render MỘT cell của layout (doc §2.2). Phân 3 nhánh theo hợp đồng L1:
//   • spacer  → <div/> trống (giữ căn cột trong grid), KHÔNG đụng field.
//   • string  → { field: name } kế thừa default từ sys_fields.
//   • object  → merge override (labelOverride/required/readonly) lên field meta.
// mode="edit"   → FieldRow + FieldInput (write).  readonly override → FieldDisplay (read).
// mode="detail" → nhãn + FieldDisplay (read).
// Field thiếu trong metadata → render placeholder nhẹ thay vì crash (forward-compat §2.4).
//
// SLDS 2.264.1 (form-element/record-detail): edit = stacked form element in `slds-is-editing`
// (the SLDS control draws it, see FieldRow); read = stacked form element in view mode
// (`slds-form-element_readonly`, static value) — also used for a field-ACL-locked field inside the
// edit form (#12/#13: text, never an input).

import type { ReactElement } from "react";
import type { LayoutCell, FieldMeta } from "@/lib/api";
import { isSpacerCell, cellField } from "@/lib/api";
import { mergeFieldSpec } from "@/lib/fieldSpec";
import { t } from "@/lib/i18n";
import { FormElement } from "@/components/slds/FormElement";
import { FieldRow, FieldInput } from "@/components/fields/widgets";
import { FieldDisplay } from "@/components/fields/display";

export type CellMode = "edit" | "detail";

export interface CellRendererProps {
  cell: LayoutCell;
  mode: CellMode;
  // Field metadata keyed by field name (from listFields(entity)).
  fieldsByName: Record<string, FieldMeta>;
  value: unknown;
  // Every value of the record, keyed by field name (edit mode): a dependent picklist lists by its
  // controlling field's value (specs/052). Omitted = no sibling-aware widget behaviour.
  values?: Record<string, unknown>;
  // Only used in edit mode; detail mode ignores it.
  onChange?: (v: unknown) => void;
  // Field-level error message (edit mode), e.g. from react-hook-form / a 422 mapping.
  error?: string;
}

export function CellRenderer({
  cell,
  mode,
  fieldsByName,
  value,
  values,
  onChange,
  error,
}: CellRendererProps): ReactElement {
  // ── spacer: empty grid slot ──
  if (isSpacerCell(cell)) {
    return <div aria-hidden className="hidden md:block" />;
  }

  const fc = cellField(cell); // string → { field }, object passthrough
  if (!fc) return <div aria-hidden className="hidden md:block" />;

  const base = fieldsByName[fc.field];
  if (!base) {
    // Field referenced by the layout but missing from metadata — degrade gracefully.
    return (
      <FormElement layout="stacked" readOnly="static" label={fc.field}>
        <span className="slds-text-color_weak">{t("page.recordForm.missingField")}</span>
      </FormElement>
    );
  }

  const spec = mergeFieldSpec(base, fc);

  // ── detail mode (or readonly override in edit) → read-only display ──
  if (mode === "detail" || spec.readonly) {
    // W5 contract §2.3: cell helpText renders under the value on detail too.
    return (
      <FormElement layout="stacked" readOnly label={spec.label} help={spec.helpText}>
        <FieldDisplay field={spec} value={value} />
      </FormElement>
    );
  }

  // ── edit mode → labelled input ──
  return (
    <FieldRow field={spec} error={error}>
      <FieldInput field={spec} value={value} onChange={onChange ?? (() => {})} error={!!error}
        siblings={values ? { values, fieldsByName } : undefined} />
    </FieldRow>
  );
}
