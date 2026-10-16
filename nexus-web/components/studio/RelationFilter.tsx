"use client";

// RelationFilter — the "Lọc theo ô khác" block of the Studio field inspector for a relation field
// (queue #304): Thiết bị lists only the devices of the Khách hàng already chosen on the same form.
// Two read-only comboboxes: the sibling relation to filter by (this entity), then the field of the
// target records that must equal it. The block edits the inspector draft only; Lưu sends it
// (lib/fieldInspector → options.filterBy, validated server-side as FIELD_RELATION_FILTER_INVALID).

import { useQuery } from "@tanstack/react-query";
import { listFields, type FieldMeta } from "@/lib/api";
import type { InspectorDraft } from "@/lib/fieldInspector";
import { t } from "@/lib/i18n";
import { qk } from "@/lib/query/keys";
import { Combobox, type ComboboxOption } from "@/components/slds/Combobox";
import { FormElement } from "@/components/slds/FormElement";
import { ScopedNotification } from "@/components/slds/ScopedNotification";

const targetOf = (f: FieldMeta | undefined) => (f?.options?.relatedEntity as string | undefined) ?? "";

export function RelationFilter({ field, fields, draft, onChange }: {
  field: FieldMeta;
  /** Every live field of the entity (the sibling relations to filter by). */
  fields: FieldMeta[];
  draft: InspectorDraft;
  onChange: (p: Partial<InspectorDraft>) => void;
}) {
  const siblings = fields.filter((f) => f.fieldType === "relation" && f.name !== field.name);
  const parent = siblings.find((f) => f.name === draft.filterParent);
  const target = targetOf(field);
  const targetQ = useQuery({ queryKey: qk.fields(target), queryFn: () => listFields(target), enabled: !!target && !!parent });
  // Only relations on the target that point at the same kind of record as the chosen sibling make
  // sense (device.customer ↔ ticket.customer); anything else could never match.
  const candidates = (targetQ.data ?? []).filter((f) => f.fieldType === "relation" && targetOf(f) === targetOf(parent));

  const parentOptions: ComboboxOption[] = [
    { value: "", label: t("page.fieldEditor.relFilter.none") },
    ...siblings.map((f) => ({ value: f.name, label: f.label || f.name })),
  ];
  const fieldOptions: ComboboxOption[] = candidates.map((f) => ({ value: f.name, label: f.label || f.name }));

  return (
    <FormElement as="fieldset" label={t("page.fieldEditor.relFilter.title")}>
      <div className="flex flex-col gap-2">
        <Combobox label={t("page.fieldEditor.relFilter.parent")} options={parentOptions} value={draft.filterParent}
          onChange={(v) => onChange({ filterParent: v ?? "", filterField: "" })} help={t("page.fieldEditor.relFilter.help")} />
        {parent && fieldOptions.length > 0 && (
          <Combobox label={t("page.fieldEditor.relFilter.field")} options={fieldOptions} value={draft.filterField}
            onChange={(v) => onChange({ filterField: v ?? "" })} />
        )}
        {parent && targetQ.isSuccess && fieldOptions.length === 0 && (
          <ScopedNotification theme="warning">{t("page.fieldEditor.relFilter.noCandidate")}</ScopedNotification>
        )}
      </div>
    </FormElement>
  );
}

/** A parent chosen without the target field is half a filter: the inspector must not save it silently. */
export function relationFilterIncomplete(draft: InspectorDraft): boolean {
  return !!draft.filterParent !== !!draft.filterField;
}
