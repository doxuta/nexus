"use client";

// "Chỉ hiện khi" of the selected cell (specs/075 US2 T067, specs/054 #23): one condition built with ConditionFields,
// limited to the operators the server knows. The rules (FR-3/5/6) come from lib/visibleWhenRules; the server still judges.
import { useState } from "react";
import { Button } from "@/components/slds/Button";
import { ConditionFields } from "@/components/slds/ExpressionBuilder";
import { opsForType, type FilterRow } from "@/lib/filterModel";
import type { FieldMeta, VisibleWhen } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import type { DraftFieldCell, DraftSection } from "@/lib/layoutDraft";
import { controllerChoices, rowToVisibleWhen, visibleWhenToRow, whenLockedReason } from "@/lib/visibleWhenRules";

const SERVER_OPS = new Set(["eq", "neq", "in", "isnull", "notnull"]);

export interface VisibleWhenRowProps {
  cell: DraftFieldCell;
  field: FieldMeta;
  fields: FieldMeta[];
  draft: DraftSection[];
  disabled?: boolean;
  /** Server sentence for a 422 LAYOUT_VISIBLE_WHEN_INVALID, shown under the section. */
  error?: string | null;
  /** undefined = no condition. */
  onChange: (vw: VisibleWhen | undefined) => void;
}

const opsFor = (ft: string) => opsForType(ft).filter((o) => SERVER_OPS.has(o) && (o !== "in" || ft === "picklist"));

export function VisibleWhenRow({ cell, field, fields, draft, disabled, error, onChange }: VisibleWhenRowProps) {
  const [row, setRow] = useState<FilterRow | null>(() => (cell.visibleWhen ? visibleWhenToRow(cell.visibleWhen, "vw") : null));
  const choices = controllerChoices(field.name, fields, draft);
  const locked = whenLockedReason(field, cell);
  const readOnly = cell.visibleWhen && !visibleWhenToRow(cell.visibleWhen, "vw");
  const controllerLabel = (name: string) => fields.find((f) => f.name === name)?.label || name;

  let body;
  if (readOnly) {
    body = <p className="slds-text-body_small">{tf("studio.build.screens.visibleWhen.readOnly", { field: controllerLabel(cell.visibleWhen!.field) })}</p>;
  } else if (locked) {
    body = <p className="slds-text-color_weak slds-text-body_small">{t(`studio.build.screens.visibleWhen.locked.${locked}`)}</p>;
  } else if (choices.length === 0) {
    body = <p className="slds-text-color_weak slds-text-body_small">{t("studio.build.screens.visibleWhen.noChoices")}</p>;
  } else {
    body = (
      <ConditionFields
        layout="stacked"
        row={row ?? { id: "vw", field: "", op: "eq", value: "" }}
        fields={choices}
        opsFor={opsFor}
        disabled={disabled}
        onChange={(next) => {
          setRow(next);
          onChange(rowToVisibleWhen(next, fields.find((f) => f.name === next.field)) ?? undefined);
        }}
      />
    );
  }
  return (
    <div className="slds-m-top_medium">
      <h4 className="slds-text-title_caps slds-m-bottom_x-small">{t("studio.build.screens.visibleWhen.title")}</h4>
      {body}
      {(row || cell.visibleWhen) && !disabled && (
        <Button variant="base" onClick={() => { setRow(null); onChange(undefined); }}>{t("studio.build.screens.visibleWhen.clear")}</Button>
      )}
      {error && <p role="alert" className="slds-text-color_error slds-m-top_x-small">{error}</p>}
    </div>
  );
}
