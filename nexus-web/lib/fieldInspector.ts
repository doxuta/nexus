// lib/fieldInspector.ts — state of the Studio field inspector (EntityFieldEditor, hub tab
// "Trường") and the PUT /admin/fields/:id payload it saves.
//
// Queue #92 (field-inspector-stale-overwrite): the inspector used to send its WHOLE state on every
// save, so a save made from a stale screen wrote back whatever someone had changed elsewhere in the
// meantime (turning "nhạy cảm" on put isRequired=true back over an API change). The endpoint is
// apply-when-non-nil — an omitted key keeps the stored value — so the fix is to send only the keys
// the maker changed since the inspector loaded the field: `changedUpdate(base, draft)` compares the
// payload built from the draft with the payload built from `draftFromField(base)`, where `base` is
// the snapshot taken when the field was opened (NOT a later refetch — diffing against a refetched
// row would turn "unchanged on this screen" into "revert to what this screen shows").
// `options` stays a full replacement when it IS sent (field_service.go assigns it wholesale), so
// its builder below keeps carrying every stored key it does not own.

import type { FieldMeta } from "@/lib/api";
import type { FieldType } from "@/lib/metadata";
import { rowsFromValues, rowsToValues, rowsToValueLabels, type PicklistRow } from "@/lib/picklistRows";
import { dependencyOf, pruneDependentValues, relationFilterOf, type DependentValues } from "@/lib/dependentPicklist";
import {
  decodeDefaultValue, encodeDefaultValue, formulaEditOptions, supportsDefaultValue, supportsIndexed,
  supportsSensitive, withIndexed, withSensitive,
  type AutoNumberCfg, type LookupCfg, type RollupCfg,
} from "@/lib/fieldOptions";

export interface InspectorDraft {
  label: string;
  isRequired: boolean;
  description: string;
  /** picklist / multi_select values (code + display label per row). */
  rows: PicklistRow[];
  currency: string;
  precision: string;
  curMin: string;
  curMax: string;
  numMin: string;
  numMax: string;
  numStep: string;
  maxLen: string;
  pattern: string;
  ratingMax: string;
  formula: string;
  lookup: LookupCfg;
  rollup: RollupCfg;
  autoNumber: AutoNumberCfg;
  defaultScalar: string;
  defaultMulti: string[];
  indexed: boolean;
  sensitive: boolean;
  /** single picklist only (specs/052): the controlling sibling picklist, "" = not dependent. */
  controllingField: string;
  /** parent value → allowed child values (meaningful only with controllingField). */
  dependentValues: DependentValues;
  /** relation only (queue #304): list target records where target.filterField = the value of the
   *  sibling relation filterParent. "" / "" = the picker is not filtered. */
  filterParent: string;
  filterField: string;
}

/** PUT /admin/fields/:id body (lib/api updateField) — every key optional, omitted = unchanged. */
export interface FieldUpdate {
  label?: string;
  isRequired?: boolean;
  options?: Record<string, unknown>;
  defaultValue?: string;
  description?: string;
}

const str = (v: unknown) => (v != null ? String(v) : "");

/** Inspector state for a stored field (what the inputs show when it is opened). */
export function draftFromField(f: FieldMeta): InspectorDraft {
  const o = f.options ?? {};
  const ft = f.fieldType as FieldType;
  const dv = decodeDefaultValue(ft, f.defaultValue);
  const viaRelation = (o.viaRelation as string | undefined) ?? "";
  return {
    label: f.label || f.name,
    isRequired: !!f.isRequired,
    description: f.description ?? "",
    rows: ft === "picklist" || ft === "multi_select"
      ? rowsFromValues((o.values as string[] | undefined) ?? [], o.valueLabels as Record<string, string> | undefined)
      : [],
    currency: ft === "currency" ? ((o.currencyCode as string | undefined) ?? "VND") : "VND",
    precision: ft === "currency" ? str(o.precision) : "",
    curMin: ft === "currency" ? str(o.min) : "",
    curMax: ft === "currency" ? str(o.max) : "",
    numMin: ft === "number" ? str(o.min) : "",
    numMax: ft === "number" ? str(o.max) : "",
    numStep: ft === "number" ? str(o.step) : "",
    maxLen: ft === "text" || ft === "rich_text" ? str(o.maxLength) : "",
    pattern: ft === "text" || ft === "rich_text" ? ((o.pattern as string | undefined) ?? "") : "",
    ratingMax: ft === "rating" ? str(o.max) : "",
    formula: ft === "formula" ? ((o.formula as string | undefined) ?? "") : "",
    lookup: { via: (o.via as string | undefined) ?? "", sourceField: (o.sourceField as string | undefined) ?? "" },
    rollup: {
      childEntity: (o.childEntity as string | undefined) ?? "",
      metric: (o.metric as RollupCfg["metric"] | undefined) ?? "count",
      aggField: (o.aggField as string | undefined) ?? "",
      mode: viaRelation ? "viaRelation" : "relationship",
      viaRelation,
      relationship: (o.relationship as string | undefined) ?? "",
      stored: o.stored === true,
    },
    autoNumber: { prefix: (o.prefix as string | undefined) ?? "", padding: str(o.padding) },
    defaultScalar: dv.scalar,
    defaultMulti: dv.multi,
    indexed: o.indexed === true,
    sensitive: o.sensitive === true,
    controllingField: ft === "picklist" ? (dependencyOf(o)?.controllingField ?? "") : "",
    dependentValues: ft === "picklist" ? structuredClone(dependencyOf(o)?.dependentValues ?? {}) : {},
    filterParent: ft === "relation" ? (relationFilterOf(o)?.parentField ?? "") : "",
    filterField: ft === "relation" ? (relationFilterOf(o)?.field ?? "") : "",
  };
}

/** The per-type options object the inspector owns, or undefined when it owns none (relation). */
function buildOptions(f: FieldMeta, d: InspectorDraft): Record<string, unknown> | undefined {
  const ft = f.fieldType as FieldType;
  let options: Record<string, unknown> | undefined;
  if (ft === "picklist" || ft === "multi_select") {
    // valueLabels MUST be carried through (options is a full replacement) or the Vietnamese value
    // labels of a whole pack vanish on the first rename. The rows own the label layer, so labels
    // serialize from them; every OTHER stored key (indexed, sensitive, …) is carried untouched —
    // the pre-B2 shape `{values}` silently dropped `indexed` (caught live on crm_leads.status).
    // valueLabels is excluded from the spread so clearing every label removes the key.
    // specs/052: on a single picklist the inspector also owns the dependency pair — excluded from
    // the carried keys so "Không phụ thuộc" removes it, and its map pruned to the values this save
    // keeps (a child value dropped here while the map still names it is 422 FR-2 otherwise).
    const vl = rowsToValueLabels(d.rows);
    const owned = new Set(["valueLabels", "values", ...(ft === "picklist" ? ["controllingField", "dependentValues"] : [])]);
    const rest = Object.fromEntries(Object.entries(f.options ?? {}).filter(([k]) => !owned.has(k)));
    const values = rowsToValues(d.rows);
    options = { ...rest, values, ...(vl ? { valueLabels: vl } : {}) };
    if (ft === "picklist" && d.controllingField) {
      options.controllingField = d.controllingField;
      options.dependentValues = pruneDependentValues(d.dependentValues, values);
    }
  } else if (ft === "currency") {
    options = { currencyCode: d.currency };
    if (d.precision !== "") { const p = parseInt(d.precision, 10); if (!Number.isNaN(p)) options.precision = p; }
    if (d.curMin !== "") { const v = parseFloat(d.curMin); if (!Number.isNaN(v)) options.min = v; }
    if (d.curMax !== "") { const v = parseFloat(d.curMax); if (!Number.isNaN(v)) options.max = v; }
  } else if (ft === "number") {
    options = Object.fromEntries(
      ([["min", d.numMin], ["max", d.numMax], ["step", d.numStep]] as const)
        .filter(([, v]) => v !== "")
        .map(([k, v]) => [k, parseFloat(v)]),
    );
  } else if (ft === "text" || ft === "rich_text") {
    options = {};
    if (d.maxLen !== "") { const n = parseInt(d.maxLen, 10); if (!Number.isNaN(n)) options.maxLength = n; }
    if (d.pattern.trim()) options.pattern = d.pattern.trim();
  } else if (ft === "rating") {
    options = {};
    if (d.ratingMax !== "") { const n = parseInt(d.ratingMax, 10); if (!Number.isNaN(n)) options.max = n; }
  } else if (ft === "formula") {
    // Live validation (FormulaEditor) already dry-ran the exact save validator.
    options = formulaEditOptions(f.options, d.formula);
  } else if (ft === "lookup") {
    options = {};
    if (d.lookup.via) options.via = d.lookup.via;
    if (d.lookup.sourceField) options.sourceField = d.lookup.sourceField;
  } else if (ft === "rollup") {
    options = {};
    if (d.rollup.childEntity) options.childEntity = d.rollup.childEntity;
    if (d.rollup.metric) options.metric = d.rollup.metric;
    if (d.rollup.metric !== "count" && d.rollup.aggField) options.aggField = d.rollup.aggField;
    if (d.rollup.mode === "viaRelation" && d.rollup.viaRelation) options.viaRelation = d.rollup.viaRelation;
    if (d.rollup.mode === "relationship" && d.rollup.relationship) options.relationship = d.rollup.relationship;
    // A stored roll-up's definition is fixed (BE FIELD_ROLLUP_STORED_IMMUTABLE): resend it verbatim.
    if (d.rollup.stored) options.stored = true;
  } else if (ft === "auto_number") {
    options = {};
    if (d.autoNumber.prefix) options.prefix = d.autoNumber.prefix;
    if (d.autoNumber.padding !== "") {
      const p = parseInt(d.autoNumber.padding, 10);
      if (!Number.isNaN(p)) options.padding = p;
    }
  }
  if (ft === "relation") {
    // queue #304: filterBy is the one relation option the inspector owns. Unchanged => undefined
    // (keep stored verbatim); changed => the stored options carried whole (options is a full
    // replacement) with filterBy set or removed.
    const was = relationFilterOf(f.options);
    const now = d.filterParent && d.filterField ? { field: d.filterField, parentField: d.filterParent } : null;
    if (was?.field === now?.field && was?.parentField === now?.parentField) return undefined;
    const out: Record<string, unknown> = { ...(f.options ?? {}) };
    delete out.filterBy;
    if (now) out.filterBy = now;
    return out;
  }
  // relation: undefined on purpose — relatedEntity/cardinality/inverseField are immutable
  // (api-conventions §6.6, 422 FIELD_RELATION_OPTIONS_IMMUTABLE) and an omitted `options` keeps
  // the stored value verbatim. The indexed / sensitive overlays pass ineligible types through
  // untouched, and always return an object for eligible ones so switching a flag OFF reaches the
  // backend as an absent key inside a present payload. Sensitive goes on AFTER indexed so both
  // survive on a type with no other options (email/date).
  options = withIndexed(options, supportsIndexed(ft, !!f.isUnique), d.indexed);
  return withSensitive(options, supportsSensitive(ft), d.sensitive);
}

/** Everything the inspector would save for `d` (the pre-#92 payload). */
export function fullUpdate(f: FieldMeta, d: InspectorDraft): FieldUpdate {
  const ft = f.fieldType as FieldType;
  const out: FieldUpdate = { label: d.label, isRequired: d.isRequired, description: d.description };
  const options = buildOptions(f, d);
  if (options !== undefined) out.options = options;
  // Only for types whose default this UI owns — "" clears (explicit set).
  if (supportsDefaultValue(ft)) out.defaultValue = encodeDefaultValue(ft, d.defaultScalar, d.defaultMulti);
  return out;
}

// Key-order-insensitive equality for JSON-shaped values (options objects, arrays, scalars).
function canonical(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(canonical);
  if (v && typeof v === "object") {
    return Object.fromEntries(Object.keys(v).sort().map((k) => [k, canonical((v as Record<string, unknown>)[k])]));
  }
  return v;
}
const same = (a: unknown, b: unknown) => JSON.stringify(canonical(a)) === JSON.stringify(canonical(b));

/** Only the keys `draft` changed relative to the snapshot `base` the inspector opened with. */
export function changedUpdate(base: FieldMeta, draft: InspectorDraft): FieldUpdate {
  const before = fullUpdate(base, draftFromField(base));
  const after = fullUpdate(base, draft);
  const out: FieldUpdate = {};
  for (const key of Object.keys(after) as (keyof FieldUpdate)[]) {
    if (!same(before[key], after[key])) Object.assign(out, { [key]: after[key] });
  }
  return out;
}
