"use client";

// PicklistDependency — the "Phụ thuộc" block of the Studio field inspector for a single picklist
// (queue #37 dependent-picklist-b, specs/052 §8): Tỉnh/TP → Quận/Huyện declared on screen instead
// of PUT /admin/fields by hand. SLDS 2.264.1, composed from basic blueprints:
//   • the controlling field is a `combobox` (base, read-only listbox) inside the inspector's
//     `form-element` fieldset;
//   • the map is edited in a `modals/base` dialog — a `combobox` (autocomplete: 63 provinces, typed
//     without diacritics) picks ONE parent value, a `dueling-picklist` (no reorder) moves the child
//     values it allows. 63 × 700 does not fit a matrix; one parent at a time does.
//   • configs the engine tolerates but the record form trips on are `scoped-notifications`
//     (warning): child values no parent allows, map keys the parent dropped, the pair at different
//     field-ACL levels for a role, a child default the parent default cannot carry.
// The block edits the inspector draft only; the inspector's Lưu sends it (lib/fieldInspector).

import { useMemo, useState } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { useQuery } from "@tanstack/react-query";
import { listRoles, type FieldMeta } from "@/lib/api";
import { getFieldAcl } from "@/lib/admin";
import {
  controllingCandidates, dependencyWarnings, parseDependentCsv, pruneDependentValues, visibilityMismatch, type DependentValues,
} from "@/lib/dependentPicklist";
import type { InspectorDraft } from "@/lib/fieldInspector";
import { t, tf } from "@/lib/i18n";
import { useWorkshopScope } from "@/lib/workshopScope";
import { projectSlugAt } from "@/lib/studioContext";
import { studioHref } from "@/lib/studioRoutes";
import { rowsFromValues, rowsToValues } from "@/lib/picklistRows";
import { qk } from "@/lib/query/keys";
import { Button } from "@/components/slds/Button";
import { Combobox, type ComboboxOption } from "@/components/slds/Combobox";
import { DuelingPicklist } from "@/components/slds/DuelingPicklist";
import { FormElement } from "@/components/slds/FormElement";
import { Textarea } from "@/components/slds/Textarea";
import { Modal } from "@/components/slds/Modal";
import { ScopedNotification } from "@/components/slds/ScopedNotification";

const LIST_MAX = 5;
/** "a, b, c, d, e … và 12 giá trị khác" — 700 districts do not fit one warning line. */
function listOf(values: string[]): string {
  const head = values.slice(0, LIST_MAX).join(", ");
  return values.length > LIST_MAX ? `${head} ${tf("page.fieldEditor.dep.more", { n: values.length - LIST_MAX })}` : head;
}

/** True when the draft declares a controlling field but no parent value allows any child yet —
 *  the engine refuses an empty dependentValues (422 FR-2), so the inspector must not save it. */
export function dependencyUnmapped(draft: InspectorDraft): boolean {
  if (!draft.controllingField) return false;
  const map = pruneDependentValues(draft.dependentValues, rowsToValues(draft.rows));
  return !Object.values(map).some((vs) => vs.length > 0);
}

export function PicklistDependency({ entityName, field, fields, draft, onChange }: {
  entityName: string;
  field: FieldMeta;
  /** Every live field of the entity (the controlling candidates and their values). */
  fields: FieldMeta[];
  draft: InspectorDraft;
  onChange: (p: Partial<InspectorDraft>) => void;
}) {
  const workshop = useWorkshopScope();
  const pathname = usePathname();
  const cf = draft.controllingField;
  const candidates = useMemo(() => controllingCandidates(fields, field.name), [fields, field.name]);
  const parent = candidates.find((f) => f.name === cf);
  const parentLabel = parent?.label || cf;
  const childLabel = draft.label || field.label || field.name;
  const parentValues = useMemo(() => (parent?.options?.values as string[] | undefined) ?? [], [parent]);
  const parentText = (v: string) => (parent?.options?.valueLabels as Record<string, string> | undefined)?.[v] ?? v;
  // CSV paste (#254): child codes the paste introduced (joined into the draft rows on Áp dụng) and
  // the per-line errors of the last paste attempt.
  const [csv, setCsv] = useState("");
  const [added, setAdded] = useState<string[]>([]);
  const [csvErrors, setCsvErrors] = useState<{ line: number; kind: "format" | "parent"; text: string }[]>([]);
  const childValues = rowsToValues(draft.rows);
  // One option per code (a duplicate row blocks the save anyway), labelled like the record form.
  const seen = new Set<string>();
  const childOptions = [
    ...draft.rows.flatMap((r) => {
      const v = r.value.trim();
      if (!v || seen.has(v)) return [];
      seen.add(v);
      return [{ value: v, label: r.label?.trim() || v }];
    }),
    ...added.filter((v) => !seen.has(v)).map((v) => ({ value: v, label: v })),
  ];

  // Field-ACL levels of the pair (admin endpoint; Studio is admin-only). Advisory: a failed read
  // shows no warning rather than a false one.
  const aclQ = useQuery({ queryKey: qk.fieldAcl(entityName), queryFn: () => getFieldAcl(entityName), enabled: !!parent });
  const mismatch = parent && aclQ.data ? visibilityMismatch(aclQ.data, parent.name, field.name) : [];
  const rolesQ = useQuery({ queryKey: qk.roles(), queryFn: listRoles, enabled: mismatch.length > 0 });

  // The dialog edits a copy; Áp dụng writes it into the draft, Huỷ drops it.
  const [editing, setEditing] = useState<DependentValues | null>(null);
  const [sel, setSel] = useState("");

  const options: ComboboxOption[] = [
    { value: "", label: t("page.fieldEditor.dep.none") },
    ...candidates.map((f) => ({ value: f.name, label: f.label || f.name })),
    // A controlling field no longer live (soft-deleted): keep it visible, the warning below says so.
    ...(cf && !parent ? [{ value: cf, label: tf("page.fieldEditor.storedSuffix", { value: cf }) }] : []),
  ];

  const pickControlling = (v: string | null) => {
    const next = v ?? "";
    if (next === cf) return;
    // A map keyed by another field's values means nothing under the new one (the engine re-checks
    // every key when the controlling field changes) — start empty.
    onChange({ controllingField: next, dependentValues: {} });
  };

  const w = parent
    ? dependencyWarnings({
        childValues,
        dependentValues: draft.dependentValues,
        parentValues,
      })
    : null;
  const mapped = parentValues.filter((v) => (draft.dependentValues[v] ?? []).length > 0).length;
  const roleNames = mismatch.map((id) => {
    const r = rolesQ.data?.find((x) => x.id === id);
    return r?.label || r?.name || id;
  });

  const apply = () => {
    if (!editing) return;
    // An empty list and an absent key mean the same to the engine ("no child"); keep the map tidy.
    const known = new Set(childValues);
    const fresh = added.filter((v) => !known.has(v));
    onChange({
      dependentValues: Object.fromEntries(Object.entries(editing).filter(([, vs]) => vs.length > 0)),
      ...(fresh.length > 0 ? { rows: [...draft.rows, ...rowsFromValues(fresh)] } : {}),
    });
    setEditing(null);
  };

  const importCsv = () => {
    if (!editing) return;
    const r = parseDependentCsv(csv, parentValues, parentText);
    setCsvErrors(r.errors);
    if (r.errors.length > 0) return; // any bad line: apply nothing
    const merged = { ...editing };
    for (const [k, vs] of Object.entries(r.map)) merged[k] = [...new Set([...(merged[k] ?? []), ...vs])];
    setEditing(merged);
    setAdded((a) => [...new Set([...a, ...r.children])]);
    setCsv("");
  };

  return (
    <FormElement as="fieldset" label={t("page.fieldEditor.dep.title")}>
      <div className="flex flex-col gap-2">
        <Combobox label={t("page.fieldEditor.dep.controlling")} options={options} value={cf} onChange={pickControlling}
          help={t("page.fieldEditor.dep.help")} />

        {cf && !parent && (
          <ScopedNotification theme="warning">{tf("page.fieldEditor.dep.parentMissing", { name: cf, child: childLabel })}</ScopedNotification>
        )}

        {parent && w && (
          <>
            <div className="flex flex-wrap items-center gap-2">
              <span className="slds-text-body_small">
                {tf("page.fieldEditor.dep.summary", { n: mapped, total: parentValues.length, parent: parentLabel, child: childLabel })}
              </span>
              <Button onClick={() => { setSel(""); setCsv(""); setCsvErrors([]); setAdded([]); setEditing(structuredClone(draft.dependentValues)); }}>
                {t("page.fieldEditor.dep.edit")}
              </Button>
            </div>
            {/* Nothing mapped yet: every value is "orphan" — the summary and the locked Lưu already
                say so; listing all 700 districts as a warning would only shout. */}
            {w.orphanChildren.length > 0 && Object.keys(draft.dependentValues).length > 0 && (
              <ScopedNotification theme="warning">
                {tf("page.fieldEditor.dep.orphanChildren", { parent: parentLabel, values: listOf(w.orphanChildren) })}
              </ScopedNotification>
            )}
            {w.orphanKeys.length > 0 && (
              <ScopedNotification theme="warning">
                <span className="flex flex-wrap items-center gap-2">
                  <span>{tf("page.fieldEditor.dep.orphanKeys", { parent: parentLabel, values: listOf(w.orphanKeys) })}</span>
                  <Button variant="neutral" onClick={() => onChange({
                    dependentValues: Object.fromEntries(Object.entries(draft.dependentValues).filter(([k]) => !w.orphanKeys.includes(k))),
                  })}>
                    {t("page.fieldEditor.dep.remove")}
                  </Button>
                </span>
              </ScopedNotification>
            )}
            {mismatch.length > 0 && !rolesQ.isPending && (
              <ScopedNotification theme="warning">
                {tf("page.fieldEditor.dep.visibility", { parent: parentLabel, child: childLabel, roles: roleNames.join(", ") })}{" "}
                <Link href={workshop ? workshop.panelHref("quyen-truong", { entity: entityName, field: field.name }) : studioHref(projectSlugAt(pathname), "dung/nguoi-dung-quyen/quyen-truong", {}, { bang: entityName, truong: field.name })}>
                  {t("page.fieldEditor.fieldAcl")}
                </Link>
              </ScopedNotification>
            )}
          </>
        )}
      </div>

      {parent && (
        <Modal
          open={!!editing}
          onOpenChange={(o) => { if (!o) setEditing(null); }}
          size="medium"
          title={tf("page.fieldEditor.dep.modalTitle", { parent: parentLabel, child: childLabel })}
          tagline={tf("page.fieldEditor.dep.modalTagline", { parent: parentLabel, child: childLabel })}
          footer={
            <>
              <Button onClick={() => setEditing(null)}>{t("action.cancel")}</Button>
              <Button variant="brand" onClick={apply}>{t("page.fieldEditor.dep.apply")}</Button>
            </>
          }
        >
          {editing && (
            <div className="flex flex-col gap-3">
              <Textarea label={t("page.fieldEditor.dep.csv")} help={tf("page.fieldEditor.dep.csvHelp", { parent: parentLabel, child: childLabel })}
                rows={4} value={csv} onChange={(e) => setCsv(e.target.value)} />
              <div><Button onClick={importCsv} disabled={!csv.trim()}>{t("page.fieldEditor.dep.csvImport")}</Button></div>
              {csvErrors.length > 0 && (
                <ScopedNotification theme="error">
                  <ul>
                    {csvErrors.slice(0, 10).map((e) => (
                      <li key={e.line}>{tf(e.kind === "parent" ? "page.fieldEditor.dep.csvBadParent" : "page.fieldEditor.dep.csvBadLine",
                        { line: e.line, text: e.text, parent: parentLabel })}</li>
                    ))}
                    {csvErrors.length > 10 && <li>{tf("page.fieldEditor.dep.more", { n: csvErrors.length - 10 })}</li>}
                  </ul>
                </ScopedNotification>
              )}
              <Combobox variant="autocomplete" label={tf("page.fieldEditor.dep.parentValue", { parent: parentLabel })}
                options={parentValues.map((v) => ({ value: v, label: parentText(v) }))}
                value={sel || null} onChange={(v) => setSel(v ?? "")}
                placeholder={t("page.fieldEditor.dep.parentValuePh")} />
              {sel ? (
                <DuelingPicklist
                  label={tf("page.fieldEditor.dep.allowed", { child: childLabel, parent: parentLabel, value: parentText(sel) })}
                  options={childOptions}
                  value={editing[sel] ?? []}
                  onChange={(vs) => setEditing({ ...editing, [sel]: vs })}
                  sourceLabel={t("page.fieldEditor.dep.available")}
                  selectedLabel={t("page.fieldEditor.dep.allowedShort")}
                  reorder={false}
                  responsive
                />
              ) : (
                <p className="slds-text-color_weak">{tf("page.fieldEditor.dep.pickParentValue", { parent: parentLabel, child: childLabel })}</p>
              )}
              {(() => {
                const unmapped = parentValues.filter((v) => !(editing[v] ?? []).length).map(parentText);
                return unmapped.length > 0 ? (
                  <p className="slds-text-body_small slds-text-color_weak">{tf("page.fieldEditor.dep.unmapped", { values: listOf(unmapped) })}</p>
                ) : null;
              })()}
            </div>
          )}
        </Modal>
      )}
    </FormElement>
  );
}
