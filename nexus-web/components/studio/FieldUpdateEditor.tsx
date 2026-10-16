"use client";

// "Cập nhật trường" editor (specs/058): which record → which field → a typed value.
//
//   Sửa bản ghi  — the triggering record, or "Bản ghi liên quan qua ‹relation› (‹entity›)" for every
//                  to-one relation of the rule's entity (the action then carries `via`).
//   Trường       — a picker over the writable fields of THAT record's entity (no free typing: a
//                  mistyped name used to save and then fail every run).
//   Giá trị      — the record form's own FieldInput for the field's type, so a picklist is a
//                  picklist and a number is a number on the wire.
//
// Self-contained (params in, params out) so the Studio redesign can mount it outside the old
// WorkflowBuilder frame; all state rules live in lib/workflowFieldUpdate.
import type { FieldMeta } from "@/lib/api";
import { t } from "@/lib/i18n";
import { metaToSpec } from "@/lib/fieldSpec";
import {
  childChoices, childOptionValue, fuRetarget, fuValueMissing, fuWidgetValue, fuWithValue, fuWritableFields, relationChoices,
} from "@/lib/workflowFieldUpdate";
import { isFromTriggerFor } from "@/lib/workflowValueSource";
import { FieldInput, FieldRow } from "@/components/fields/widgets";
import { TriggerValueSource } from "./TriggerValueSource";
import { fill } from "@/components/slds/DataTable";
import { Input } from "@/components/slds/Input";
import { Select } from "@/components/slds/Select";
import { Spinner } from "@/components/slds/Spinner";

export function FieldUpdateEditor({
  params,
  entityFields,
  fieldsByEntity,
  entityLabel,
  ruleEntity,
  error,
  errorField,
  onChange,
}: {
  params: Record<string, string>;
  /** Fields of the rule's own entity. */
  entityFields: FieldMeta[];
  /** Fields of other entities by name — a missing key means "still loading". */
  fieldsByEntity: Record<string, FieldMeta[]>;
  entityLabel: (name?: string) => string;
  /** The entity the rule runs on — "$record" is only offered to a relation pointing back at it. */
  ruleEntity: string;
  /** A save-time 422 pinned on this action, and the field it names (via or target field). */
  error?: string;
  errorField?: string | null;
  onChange: (next: Record<string, string>) => void;
}) {
  const choices = relationChoices(entityFields);
  const via = params.via ?? "";
  // specs/084: "mọi bản ghi con qua X" — the rows of another entity whose relation points back here.
  const childEntity = params.childEntity ?? "";
  const childVia = params.childVia ?? "";
  const kids = childChoices(ruleEntity, fieldsByEntity);
  const childValue = childEntity ? childOptionValue(childEntity, childVia) : "";
  const kidKnown = kids.some((k) => childOptionValue(k.entity, k.name) === childValue);
  const target = via ? choices.find((c) => c.name === via)?.target : undefined;
  const targetFields = childEntity ? fieldsByEntity[childEntity] : via ? (target ? fieldsByEntity[target] : []) : entityFields;
  const writable = fuWritableFields(targetFields ?? []);
  const field = params.field ?? "";
  const meta = writable.find((f) => f.name === field);
  // Where an error goes: the record picker when it names the via; the value box when the value is
  // missing (save-time check) or a 422 names a field that IS writable here (so the value is what is
  // wrong — type/picklist/required); the field picker otherwise (unknown / stale / computed field).
  const viaErr = error && ((via && errorField === via) || (childVia && errorField === childVia)) ? error : undefined;
  const valueErr = error && !viaErr && meta && (fuValueMissing(params, targetFields ?? []) || errorField === field) ? error : undefined;
  const fieldErr = error && !viaErr && !valueErr ? error : undefined;
  // The value is required here: an empty one would clear the field on every run (specs/058 §2.9).
  const spec = meta ? { ...metaToSpec(meta), required: true } : undefined;

  return (
    <div className="flex flex-col gap-3">
      <Select
        label={t("page.workflow.param.fuTarget")}
        placeholder={false}
        value={childEntity ? childValue : via}
        error={viaErr}
        help={!choices.length && !kids.length && !via && !childEntity ? t("page.workflow.param.fuNoRelation") : undefined}
        options={[
          { value: "", label: t("page.workflow.param.fuSelf") },
          ...choices.map((c) => ({ value: c.name, label: fill(t("page.workflow.param.fuVia"), { field: c.label, entity: entityLabel(c.target) }) })),
          ...kids.map((k) => ({
            value: childOptionValue(k.entity, k.name),
            label: fill(t("page.workflow.param.fuChildren"), { entity: entityLabel(k.entity), field: k.label }),
          })),
          // A saved children pick whose entity fields are still loading stays visible too.
          ...(childEntity && !kidKnown ? [{ value: childValue, label: fill(t("page.workflow.param.fuChildren"), { entity: entityLabel(childEntity), field: childVia }) }] : []),
          // A saved via that is no longer a relation stays visible, never silently back to "this record".
          ...(via && !target ? [{ value: via, label: fill(t("page.workflow.param.staleField"), { name: via }) }] : []),
        ]}
        onChange={(e) => {
          const k = kids.find((x) => childOptionValue(x.entity, x.name) === e.target.value);
          onChange(k
            ? fuRetarget(params, { via: "", childEntity: k.entity, childVia: k.name }, undefined)
            : fuRetarget(params, { via: e.target.value, childEntity: "", childVia: "" }, undefined));
        }}
      />
      {targetFields === undefined ? (
        // A <div>, not a <p>: the SLDS spinner is a <div>, which a <p> cannot contain (hydration error).
        <div className="flex items-center gap-2">
          <Spinner size="x-small" inline label={t("page.workflow.param.loadingFields")} />
          <span className="slds-text-color_weak">{t("page.workflow.param.loadingFields")}</span>
        </div>
      ) : (
        <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
          <Select
            label={t("page.workflow.param.field")}
            placeholder={t("page.workflow.param.fieldPh")}
            value={field}
            error={fieldErr}
            options={[
              ...writable.map((f) => ({ value: f.name, label: f.label || f.name })),
              ...(field && !meta ? [{ value: field, label: fill(t("page.workflow.param.staleField"), { name: field }) }] : []),
            ]}
            onChange={(e) => onChange(fuRetarget(params, { field: e.target.value }, writable.find((f) => f.name === e.target.value)))}
          />
          {!field ? null : meta && spec ? (
            <div className="flex flex-col gap-2">
              {/* queue #305/#333: type the value, or copy it from the record that fired the rule. */}
              <TriggerValueSource
                target={meta}
                ruleFields={entityFields}
                ruleEntity={ruleEntity}
                value={fuWidgetValue(params, meta)}
                onChange={(v) => onChange(fuWithValue(params, v))}
              />
              {isFromTriggerFor(fuWidgetValue(params, meta), meta, entityFields) ? (
                <p className={valueErr ? "slds-text-color_error" : "slds-text-color_weak"}>{valueErr ?? t("page.workflow.param.srcNote")}</p>
              ) : (
                // FieldRow gives the widget its visible stacked label (the field's own) so it lines up
                // with the "Trường" picker beside it instead of floating label-less.
                <FieldRow field={spec} error={valueErr}>
                  <FieldInput field={spec} value={fuWidgetValue(params, meta)} onChange={(v) => onChange(fuWithValue(params, v))} />
                </FieldRow>
              )}
            </div>
          ) : (
            // Unknown/stale field: a plain box so the stored value is shown and kept, not lost.
            <Input
              label={t("page.workflow.param.value")}
              value={(() => { const v = fuWidgetValue(params, undefined); return typeof v === "string" ? v : JSON.stringify(v); })()}
              onChange={(e) => onChange(fuWithValue(params, e.target.value))}
            />
          )}
        </div>
      )}
      {via && <p className="slds-text-color_weak">{t("page.workflow.param.fuViaNote")}</p>}
      {childEntity && <p className="slds-text-color_weak">{t("page.workflow.param.fuChildrenNote")}</p>}
    </div>
  );
}
