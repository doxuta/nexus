"use client";

// The "Tự điền giá trị" action of a before_save rule (specs/048 S4 / FE-before-save-hooks-3), on SLDS.
// Target field (never a link / person / file / computed one) → where the value comes from — a fixed value
// typed for the field's own type, a copy of another field of the SAME type, or a {{field}} sentence (text
// targets only) — and a toggle "Chỉ điền khi trống". It always writes the record being saved: no `via`.
// Params in, params out; the wire shape lives in lib/workflowFieldUpdate (fuPayload).
import { FieldInput, FieldRow } from "@/components/fields/widgets";
import { Input } from "@/components/slds/Input";
import { RadioGroup } from "@/components/slds/RadioGroup";
import { Select } from "@/components/slds/Select";
import { Toggle } from "@/components/slds/Toggle";
import type { FieldMeta } from "@/lib/api";
import { metaToSpec } from "@/lib/fieldSpec";
import { t } from "@/lib/i18n";
import { beforeTargetOk, beforeTemplateSourceOk, beforeTemplateTarget } from "@/lib/workflowBefore";
import { fuRetarget, fuWidgetValue, fuWithValue, fuWritableFields } from "@/lib/workflowFieldUpdate";

export function MutateActionEditor({
  params,
  fields,
  error,
  errorField,
  onChange,
}: {
  params: Record<string, string>;
  /** The rule entity's fields. */
  fields: FieldMeta[];
  /** A save-time 422 pinned on this action (WORKFLOW_BEFORE_*, RECORD_*) and the field it names. */
  error?: string;
  errorField?: string | null;
  onChange: (next: Record<string, string>) => void;
}) {
  const writable = fuWritableFields(fields).filter((f) => beforeTargetOk(f.fieldType));
  const field = params.field ?? "";
  const meta = writable.find((f) => f.name === field);
  const source = params.valueSource === "field" || params.valueSource === "template" ? params.valueSource : "constant";
  const canTemplate = !!meta && beforeTemplateTarget(meta.fieldType);
  const copyFrom = fields.filter((f) => meta && f.name !== field && f.fieldType === meta.fieldType);
  const templateFrom = fields.filter((f) => f.name !== field && beforeTemplateSourceOk(f.fieldType));
  // The 422 sentence belongs at the value when the server names this target; otherwise at the picker.
  const valueErr = error && meta && errorField === field ? error : undefined;
  const fieldErr = error && !valueErr ? error : undefined;
  const spec = meta ? { ...metaToSpec(meta), required: true } : undefined;

  function setSource(next: string) {
    const rest = { ...params };
    delete rest.valueSource; delete rest.srcValue; delete rest.raw; delete rest.values;
    onChange(next === "constant" ? rest : { ...rest, valueSource: next, srcValue: "" });
  }

  return (
    <div className="flex flex-col gap-3">
      <Select
        label={t("page.workflow.param.field")}
        placeholder={t("page.workflow.param.fieldPh")}
        value={field}
        error={fieldErr}
        options={[
          ...writable.map((f) => ({ value: f.name, label: f.label || f.name })),
          ...(field && !meta ? [{ value: field, label: t("workflow.before.normalize.staleField").replace("{name}", field) }] : []),
        ]}
        onChange={(e) => onChange(fuRetarget(params, { field: e.target.value }, writable.find((f) => f.name === e.target.value)))}
      />
      {field && (
        <>
          <RadioGroup
            label={t("workflow.before.mutate.source")}
            value={source}
            onValueChange={setSource}
            options={[
              { value: "constant", label: t("workflow.before.mutate.source.constant") },
              { value: "field", label: t("workflow.before.mutate.source.field") },
              ...(canTemplate ? [{ value: "template", label: t("workflow.before.mutate.source.template") }] : []),
            ]}
          />
          {source === "constant" && meta && spec && (
            <FieldRow field={spec} error={valueErr}>
              <FieldInput field={spec} value={fuWidgetValue(params, meta)} onChange={(v) => onChange(fuWithValue(params, v))} />
            </FieldRow>
          )}
          {source === "field" && (
            <Select
              label={t("workflow.before.mutate.copyFrom")}
              placeholder={t("page.workflow.param.fieldPh")}
              value={params.srcValue ?? ""}
              error={valueErr}
              help={copyFrom.length ? t("workflow.before.mutate.copyHelp") : t("workflow.before.mutate.copyNone")}
              options={copyFrom.map((f) => ({ value: f.name, label: f.label || f.name }))}
              onChange={(e) => onChange({ ...params, srcValue: e.target.value })}
            />
          )}
          {source === "template" && (
            <div className="flex flex-col gap-2">
              <Input
                label={t("workflow.before.mutate.template")}
                value={params.srcValue ?? ""}
                error={valueErr}
                help={t("workflow.before.mutate.templateHelp")}
                onChange={(e) => onChange({ ...params, srcValue: e.target.value })}
              />
              <Select
                label={t("workflow.before.mutate.insert")}
                placeholder={t("workflow.before.mutate.insertPh")}
                value=""
                options={templateFrom.map((f) => ({ value: f.name, label: f.label || f.name }))}
                onChange={(e) => e.target.value && onChange({ ...params, srcValue: `${params.srcValue ?? ""}{{${e.target.value}}}` })}
              />
            </div>
          )}
        </>
      )}
      <Toggle
        label={t("workflow.before.mutate.onlyIfEmpty")}
        help={t("workflow.before.mutate.onlyIfEmptyHelp")}
        checked={params.onlyIfEmpty === "true"}
        onChange={(e) => {
          const next = { ...params };
          if (e.target.checked) next.onlyIfEmpty = "true"; else delete next.onlyIfEmpty;
          onChange(next);
        }}
      />
    </div>
  );
}
