"use client";

// The "Chuẩn hoá chữ" action of a before_save rule (specs/048 S5 / FE-before-save-hooks-3), on SLDS:
// a `select` over the text/email fields and a `dueling-picklist` for the ORDERED operations (the order
// is the order they run in). Params in, params out — ops travels as a JSON array string (lib/workflowNormalize).
import { DuelingPicklist } from "@/components/slds/DuelingPicklist";
import { Select } from "@/components/slds/Select";
import type { FieldMeta } from "@/lib/api";
import { t } from "@/lib/i18n";
import { MAX_NORMALIZE_OPS, NORMALIZE_OPS, normOps, normalizeTargets } from "@/lib/workflowNormalize";

export function NormalizeActionEditor({
  params,
  fields,
  error,
  onChange,
}: {
  params: Record<string, string>;
  /** The rule entity's fields. */
  fields: FieldMeta[];
  /** A save-time 422 pinned on this action (NORMALIZE_* / RECORD_UNKNOWN_FIELD) and the field it names. */
  error?: string;
  onChange: (next: Record<string, string>) => void;
}) {
  const targets = normalizeTargets(fields);
  const field = params.field ?? "";
  // A valid target means the refusal is about the operations; otherwise about the field picked.
  const opsError = error && targets.some((f) => f.name === field) ? error : undefined;
  const fieldError = error && !opsError ? error : undefined;
  return (
    <div className="flex flex-col gap-3">
      <Select
        label={t("workflow.before.normalize.field")}
        placeholder={t("page.workflow.param.fieldPh")}
        value={field}
        error={fieldError}
        options={[
          ...targets.map((f) => ({ value: f.name, label: f.label || f.name })),
          ...(field && !targets.some((f) => f.name === field) ? [{ value: field, label: t("workflow.before.normalize.staleField").replace("{name}", field) }] : []),
        ]}
        onChange={(e) => onChange({ ...params, field: e.target.value })}
      />
      <DuelingPicklist
        label={t("workflow.before.normalize.ops")}
        options={NORMALIZE_OPS.map((op) => ({ value: op, label: t(`workflow.before.normalize.op.${op}`) }))}
        value={normOps(params)}
        error={opsError}
        responsive
        onChange={(v) => onChange({ ...params, ops: JSON.stringify(v.slice(0, MAX_NORMALIZE_OPS)) })}
      />
      <p className="slds-text-color_weak">{t("workflow.before.normalize.help")}</p>
    </div>
  );
}
