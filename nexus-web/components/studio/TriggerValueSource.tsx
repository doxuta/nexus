"use client";

// "Giá trị lấy từ": type the value, or take it from the record that fired the rule (queue #305/#333) —
// "copy the appointment date from the invoice", "link the new commission back to its deal". Used by the
// "Cập nhật trường" value box and by each row of "Tạo bản ghi mới". Emits the engine's own strings
// ("$record.<field>" / "$record", lib/workflowValueSource) and hands back the field's empty default when
// the person goes back to typing. Renders nothing when no field of the triggering record fits the target.
import type { FieldMeta } from "@/lib/api";
import { t } from "@/lib/i18n";
import { emptyDefault } from "@/lib/recordPayload";
import { RECORD_SENTINEL, isFromTriggerFor, triggerSourceOptions } from "@/lib/workflowValueSource";
import { fill } from "@/components/slds/DataTable";
import { Select } from "@/components/slds/Select";

export function TriggerValueSource({
  target,
  ruleFields,
  ruleEntity,
  value,
  onChange,
}: {
  target: FieldMeta;
  /** Fields of the entity the rule runs on. */
  ruleFields: FieldMeta[];
  ruleEntity: string;
  /** The row's current value (widget-native, or a "$record…" string when taken from the record). */
  value: unknown;
  onChange: (next: unknown) => void;
}) {
  const fromTrigger = isFromTriggerFor(value, target, ruleFields);
  const found = triggerSourceOptions(target, ruleFields, ruleEntity);
  if (!found.length && !fromTrigger) return null;
  const current = fromTrigger ? String(value) : "";
  const options = [
    { value: "", label: t("page.workflow.param.srcTyped") },
    ...found.map((o) => ({
      value: o.value,
      label: o.value === RECORD_SENTINEL ? t("page.workflow.param.srcRecord") : fill(t("page.workflow.param.srcField"), { field: o.label }),
    })),
    // A stored source that no longer fits stays visible — never silently back to "typed".
    ...(fromTrigger && !found.some((o) => o.value === current)
      ? [{ value: current, label: fill(t("page.workflow.param.staleField"), { name: current }) }]
      : []),
  ];
  return (
    <Select
      label={t("page.workflow.param.srcLabel")}
      placeholder={false}
      value={current}
      options={options}
      onChange={(e) => onChange(e.target.value === "" ? emptyDefault(target) : e.target.value)}
    />
  );
}
