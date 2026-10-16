"use client";

// BeforeTestPanel (specs/048 FE-before-save-hooks-4, US5): the part of the dry-run dialog that only a
// before rule needs — WHICH write to rehearse (create / update), the values that write sets, and
// whether to judge the rule inside the table's whole rule chain (#442). Owns the draft rows; hands
// the dialog the finished `{op, changes, withEntityRules}` through onChange. A before_delete rule
// has no simulated change (the delete sets none) — only a note.
import { useEffect } from "react";
import { Plus } from "lucide-react";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Checkbox } from "@/components/slds/Checkbox";
import { Combobox } from "@/components/slds/Combobox";
import { Input } from "@/components/slds/Input";
import { RadioGroup } from "@/components/slds/RadioGroup";
import { X } from "lucide-react";
import type { FieldMeta } from "@/lib/api";
import { NUMERIC_FIELD_TYPES, changesFromRows, type ChangeRow } from "@/lib/workflowBefore";
import { t } from "@/lib/i18n";

export interface BeforeSim {
  op?: "create" | "update";
  changes: Record<string, unknown>;
  withEntityRules: boolean;
}

export interface BeforeTestDraft {
  op: "create" | "update";
  rows: ChangeRow[];
  withEntityRules: boolean;
}

/** Fields a person can type a value for: no computed / relation / system-filled ones. */
const NOT_TYPABLE = new Set(["formula", "rollup", "lookup", "relation", "polymorphic_relation", "auto_number", "user", "file"]);

export function beforeSimOf(d: BeforeTestDraft, fields: FieldMeta[]): BeforeSim {
  const typeOf = (name: string) => fields.find((f) => f.name === name)?.fieldType ?? "text";
  return { op: d.op, changes: changesFromRows(d.rows, typeOf), withEntityRules: d.withEntityRules };
}

export function BeforeTestPanel({
  trigger,
  fields,
  draft,
  onDraft,
}: {
  trigger: string;
  fields: FieldMeta[];
  draft: BeforeTestDraft;
  onDraft: (d: BeforeTestDraft) => void;
}) {
  const isDelete = trigger === "before_delete";
  const typable = fields.filter((f) => !NOT_TYPABLE.has(f.fieldType) && !f.readOnly);
  const fieldOptions = typable.map((f) => ({ value: f.name, label: f.label || f.name }));
  const typeOf = (name: string) => fields.find((f) => f.name === name)?.fieldType ?? "text";
  const setRow = (i: number, patch: Partial<ChangeRow>) =>
    onDraft({ ...draft, rows: draft.rows.map((r, j) => (j === i ? { ...r, ...patch } : r)) });

  // A delete rule judges the record as it is: the chain flag does not apply.
  useEffect(() => {
    if (isDelete && draft.withEntityRules) onDraft({ ...draft, withEntityRules: false });
  }, [isDelete, draft, onDraft]);

  if (isDelete) return <p className="slds-text-color_weak">{t("workflow.test.before.deleteNote")}</p>;

  return (
    <div className="flex flex-col gap-3">
      <RadioGroup
        label={t("workflow.test.before.op")}
        options={[
          { value: "create", label: t("workflow.test.before.op.create") },
          { value: "update", label: t("workflow.test.before.op.update") },
        ]}
        value={draft.op}
        onValueChange={(v) => onDraft({ ...draft, op: v === "create" ? "create" : "update" })}
      />

      <fieldset className="flex flex-col gap-2">
        <legend className="slds-form-element__legend slds-form-element__label">{t("workflow.test.before.changes")}</legend>
        {draft.rows.map((r, i) => {
          const ft = typeOf(r.field);
          const picks = (typable.find((f) => f.name === r.field)?.options?.values as string[] | undefined) ?? null;
          return (
            <div key={i} className="flex items-end gap-2">
              <div className="grow">
                <Combobox
                  label={t("workflow.test.before.field")}
                  options={fieldOptions}
                  value={r.field || null}
                  onChange={(v) => setRow(i, { field: v ?? "", value: "" })}
                />
              </div>
              <div className="grow">
                {ft === "boolean" || picks ? (
                  <Combobox
                    label={t("workflow.test.before.value")}
                    options={
                      ft === "boolean"
                        ? [{ value: "true", label: t("workflow.test.before.yes") }, { value: "false", label: t("workflow.test.before.no") }]
                        : (picks ?? []).map((p) => ({ value: p, label: p }))
                    }
                    value={r.value || null}
                    onChange={(v) => setRow(i, { value: v ?? "" })}
                  />
                ) : (
                  <Input
                    label={t("workflow.test.before.value")}
                    type={NUMERIC_FIELD_TYPES.has(ft) ? "number" : ft === "date" ? "date" : "text"}
                    value={r.value}
                    onChange={(e) => setRow(i, { value: e.target.value })}
                  />
                )}
              </div>
              <ButtonIcon
                icon={X}
                label={t("workflow.test.before.removeChange")}
                onClick={() => onDraft({ ...draft, rows: draft.rows.filter((_, j) => j !== i) })}
              />
            </div>
          );
        })}
        <div>
          <Button iconLeft={Plus} onClick={() => onDraft({ ...draft, rows: [...draft.rows, { field: "", value: "" }] })}>
            {t("workflow.test.before.addChange")}
          </Button>
        </div>
      </fieldset>

      <Checkbox
        label={t("workflow.test.before.chain")}
        checked={draft.withEntityRules}
        onChange={(e) => onDraft({ ...draft, withEntityRules: e.target.checked })}
      />
    </div>
  );
}
