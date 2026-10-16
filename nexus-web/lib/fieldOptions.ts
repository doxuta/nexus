// lib/fieldOptions.ts — pure assembly of `sys_fields.options` for the Studio field-builder
// (create path). Extracted from components/studio/DataModelBuilder.tsx so the option-shape logic for
// the advanced/computed field types (relation cardinality/inverseField, lookup, rollup, auto_number)
// is unit-testable without mounting the 2k-line builder component. The option KEYS mirror the
// backend contract (internal/services/field_service.go + api-conventions §5–§7): sending exactly the
// keys the server validates means a mistyped shape surfaces as a friendly 422 (errorMessages map),
// not a silent drop.

import { rowsToValues, rowsToValueLabels, type PicklistRow } from "@/lib/picklistRows";
import { slugifyAppName } from "@/lib/slug";
import type { FieldType } from "@/lib/metadata";

// ── Type-specific config shapes for the 4 "advanced" field types (grouped state, not flat
// useState-per-option, to keep the already-large builder surface manageable). ──
export interface LookupCfg { via: string; sourceField: string }
export interface RollupCfg {
  childEntity: string; metric: "count" | "sum" | "avg" | "min" | "max"; aggField: string;
  mode: "viaRelation" | "relationship"; viaRelation: string; relationship: string;
  /** specs/083: keep the total in the record (1-N only) so it can be filtered and summed in a report. */
  stored?: boolean;
}
export interface AutoNumberCfg { prefix: string; padding: string }
export interface RelationCfg { cardinality: "1-N" | "1-1"; inverseField: string }

// isRollupAggregatable — may a rollup SUM/AVG/MIN/MAX this child field? Mirrors the engine's
// validateRollupCrossEntity (rollup_validate.go): number/currency/rating, or a formula whose result
// is a number or money (#262 — missing resultType means number). A text formula stores a string
// and is refused 422 FIELD_ROLLUP_AGG_FIELD_FORMULA_NOT_NUMBER, so the picker never offers it.
export function isRollupAggregatable(
  f: { fieldType: string; options?: Record<string, unknown> },
  siblings?: ReadonlyArray<{ name: string; fieldType: string }>,
): boolean {
  if (f.fieldType === "formula") {
    if (readsReadTimeField(f, siblings)) return false;
    const rt = f.options?.resultType ?? "number";
    return rt === "number" || rt === "currency";
  }
  return f.fieldType === "number" || f.fieldType === "currency" || f.fieldType === "rating";
}

// readsReadTimeField — does a formula read a lookup/rollup sibling (specs/080)? Such a formula is
// computed at VIEW time and never stored, so nothing can SUM/aggregate it (BE: AGGREGATE_FIELD_NOT_NUMERIC
// / FIELD_ROLLUP_AGG_FIELD_FORMULA_DERIVED). Quoted strings are dropped before reading identifiers.
export function readsReadTimeField(
  f: { fieldType: string; options?: Record<string, unknown> },
  siblings?: ReadonlyArray<{ name: string; fieldType: string }>,
): boolean {
  if (f.fieldType !== "formula" || !siblings) return false;
  const expr = typeof f.options?.formula === "string" ? f.options.formula : "";
  const readTime = new Set(siblings.filter((s) => s.fieldType === "lookup" || s.fieldType === "rollup").map((s) => s.name));
  const idents = expr.replace(/"[^"]*"|'[^']*'/g, " ").match(/[A-Za-z_][A-Za-z0-9_]*/g) ?? [];
  return idents.some((id) => readTime.has(id));
}

export const emptyLookup = (): LookupCfg => ({ via: "", sourceField: "" });
export const emptyRollup = (): RollupCfg => ({ childEntity: "", metric: "count", aggField: "", mode: "viaRelation", viaRelation: "", relationship: "" });
export const emptyAutoNumber = (): AutoNumberCfg => ({ prefix: "", padding: "" });
export const emptyRelation = (): RelationCfg => ({ cardinality: "1-N", inverseField: "" });

// Build the type-specific options object for field CREATION (addField). Returns undefined when a type
// has no options to send (server fills defaults). Covers all field types; the advanced group
// (lookup/rollup/auto_number + relation cardinality/inverseField) is passed via `advanced`.
export function buildOptions(
  type: FieldType,
  currencyCode: string,
  picklistValues: PicklistRow[], // W5: rows từ PicklistRowsEditor
  relatedEntity: string,
  numberMin: string, numberMax: string, numberStep: string,
  textMaxLen: string,
  ratingMax: string,
  // formula-specific
  formulaExpression?: string,
  formulaResultType?: string,
  // composite-specific (sub-field labels+types as a serialised list "label:type,...")
  compositeSubFields?: string,
  // lookup/rollup/auto_number + relation cardinality/inverseField (grouped config, see LookupCfg
  // et al. above) — undefined when the caller (edit-mode paths) builds options differently.
  advanced?: { lookup: LookupCfg; rollup: RollupCfg; autoNumber: AutoNumberCfg; relation: RelationCfg },
  // polymorphic_relation targetEntities (ACTIVITY-CORE #3a) — the allowlist of entities the
  // {type,id} value may point at. Mirrors relation's single relatedEntity, but MANY.
  polymorphicTargets?: string[],
  // D1 (Slice 1.2): options.indexed = true materializes a VIRTUAL generated column + non-unique
  // index server-side. Only emitted when true (absent = false, mirroring the BE boolOpt read);
  // the caller gates on supportsIndexed() so an ineligible type never carries the key.
  indexed?: boolean,
): Record<string, unknown> | undefined {
  const base = typeOptions(type, currencyCode, picklistValues, relatedEntity, numberMin, numberMax,
    numberStep, textMaxLen, ratingMax, formulaExpression, formulaResultType, compositeSubFields,
    advanced, polymorphicTargets);
  // indexed rides ON TOP of the per-type shape: a date/email field has no other options, so the
  // key may be the sole reason an object is sent at all.
  if (indexed) return { ...(base ?? {}), indexed: true };
  return base;
}

function typeOptions(
  type: FieldType,
  currencyCode: string,
  picklistValues: PicklistRow[],
  relatedEntity: string,
  numberMin: string, numberMax: string, numberStep: string,
  textMaxLen: string,
  ratingMax: string,
  formulaExpression?: string,
  formulaResultType?: string,
  compositeSubFields?: string,
  advanced?: { lookup: LookupCfg; rollup: RollupCfg; autoNumber: AutoNumberCfg; relation: RelationCfg },
  polymorphicTargets?: string[],
): Record<string, unknown> | undefined {
  switch (type) {
    case "currency":
      return { currencyCode: currencyCode || "VND" };
    case "picklist":
    case "multi_select":
      {
        // #274: the create path must carry the Vietnamese label layer too (edit path already does,
        // lib/fieldInspector.ts) — else customers see the raw codes 'kham', 'dang_hoc'.
        const vl = rowsToValueLabels(picklistValues);
        return { values: rowsToValues(picklistValues), ...(vl ? { valueLabels: vl } : {}) };
      }
    case "relation": {
      if (!relatedEntity) return undefined;
      const opts: Record<string, unknown> = { relatedEntity };
      const rc = advanced?.relation;
      // Explicit on CREATE is always safe (no stored value to drift from — the "don't add
      // cardinality to an old field's payload" warning in api-conventions §6.6 only applies to
      // EDITING a pre-ENG-REL2-#5 field that was persisted without the key).
      opts.cardinality = rc?.cardinality === "1-1" ? "1-1" : "1-N";
      if (rc?.inverseField.trim()) opts.inverseField = rc.inverseField.trim();
      return opts;
    }
    case "polymorphic_relation": {
      // Send options.targetEntities in the BE shape (non-empty string array). Empty → undefined
      // so the server answers FIELD_POLYMORPHIC_NO_TARGETS (the builder also guards ≥1 pre-submit).
      const targets = (polymorphicTargets ?? []).map((s) => s.trim()).filter(Boolean);
      return targets.length > 0 ? { targetEntities: targets } : undefined;
    }
    case "lookup": {
      const lk = advanced?.lookup;
      if (!lk) return undefined;
      const opts: Record<string, unknown> = {};
      if (lk.via) opts.via = lk.via;
      if (lk.sourceField) opts.sourceField = lk.sourceField;
      return Object.keys(opts).length > 0 ? opts : undefined;
    }
    case "rollup": {
      const ru = advanced?.rollup;
      if (!ru) return undefined;
      const opts: Record<string, unknown> = {};
      if (ru.childEntity) opts.childEntity = ru.childEntity;
      if (ru.metric) opts.metric = ru.metric;
      if (ru.metric !== "count" && ru.aggField) opts.aggField = ru.aggField;
      if (ru.mode === "viaRelation" && ru.viaRelation) opts.viaRelation = ru.viaRelation;
      if (ru.mode === "relationship" && ru.relationship) opts.relationship = ru.relationship;
      if (ru.stored && ru.mode === "viaRelation") opts.stored = true; // BE refuses N-N (FIELD_ROLLUP_STORED_NN_UNSUPPORTED)
      return Object.keys(opts).length > 0 ? opts : undefined;
    }
    case "auto_number": {
      const an = advanced?.autoNumber;
      if (!an) return undefined;
      const opts: Record<string, unknown> = {};
      if (an.prefix) opts.prefix = an.prefix;
      if (an.padding !== "") {
        const p = parseInt(an.padding, 10);
        if (!Number.isNaN(p)) opts.padding = p;
      }
      return Object.keys(opts).length > 0 ? opts : undefined;
    }
    case "number":
      return Object.fromEntries(
        [["min", numberMin], ["max", numberMax], ["step", numberStep]]
          .filter(([, v]) => v !== "")
          .map(([k, v]) => [k, parseFloat(v)])
      ) as Record<string, unknown>;
    case "text":
    case "rich_text":
      return textMaxLen ? { maxLength: parseInt(textMaxLen) } : undefined;
    case "rating":
      return ratingMax ? { max: parseInt(ratingMax) } : undefined;
    case "formula": {
      const resultType = formulaResultType || "number";
      // A currency result without currencyCode is written as "USD" at record time
      // (computed_fields.go), so a VND commission would show as dollars.
      return {
        formula: formulaExpression || "",
        resultType,
        ...(resultType === "currency" ? { currencyCode: currencyCode || "VND" } : {}),
      };
    }
    case "composite": {
      // Parse sub-fields: "Họ:text, Tên:text, Mã:text" → [{name, label, fieldType}]
      // #292: sub-field NAME must match ddl.go ^[a-z][a-z0-9_]{0,25}$ — slug the label (accents
      // stripped), fall back to f<n> when nothing survives, suffix on duplicates; label keeps accents.
      const used = new Set<string>();
      const subFields = (compositeSubFields || "").split(",")
        .map((s) => s.trim())
        .filter(Boolean)
        .map((s, i) => {
          const [label = "", fType] = s.split(":").map((x) => x.trim());
          const base = slugifyAppName(label).slice(0, 23) || `f${i + 1}`;
          let name = base;
          for (let n = 2; used.has(name); n++) name = `${base}_${n}`;
          used.add(name);
          return { name, label, fieldType: fType || "text" };
        })
        .filter((sf) => sf.label);
      return subFields.length > 0 ? { subFields } : undefined;
    }
    default:
      return undefined;
  }
}

// ── Slice 1.2 (TOTAL-UPGRADE): default value + indexed flag for the Studio field builder ──
// Pure eligibility/encode logic so the "what the UI may offer" contract is unit-testable
// without mounting the 2k-line builder (same rationale as buildOptions above).

// Which input widget the "Giá trị mặc định" block renders per field type — null = the builder
// hides the block entirely. The wire format is ALWAYS a string (sys_fields.default_value;
// PUT /admin/fields/:id {defaultValue}); the kind only picks the native input.
//
// Mirrors the backend contract exactly (verified 2026-07-13):
//   • REJECTED 422 FIELD_DEFAULT_NOT_SUPPORTED (services.ValidateFieldDefaultNotSupported,
//     entity_service.go): currency, rich_text, lookup, rollup, auto_number, polymorphic_relation,
//     user, and (queue #62, 2026-09-28) composite/file/image — which used to be accepted and then
//     silently dropped by record_service.go defaultValue() (no scalar form).
//   • SILENTLY IGNORED at record time: formula (server-computed) → also null.
//   • relation: the BE accepts a record UUID; the builder offers the record picker (queue #320),
//     never a hand-typed UUID (that fails record-create later with RECORD_RELATION_INVALID).
export type DefaultInputKind =
  | "text" | "number" | "boolean" | "select" | "multi" | "date" | "datetime" | "record";
export function defaultInputKind(type: FieldType): DefaultInputKind | null {
  switch (type) {
    case "text": case "email": return "text";
    case "number": case "rating": return "number";
    // boolean needs THREE states (no default / true / false) — a 2-state toggle cannot say
    // "không đặt", so the builder renders a select despite the field itself being a toggle.
    case "boolean": return "boolean";
    case "picklist": return "select";
    case "multi_select": return "multi";
    case "relation": return "record"; // record picker of the target table; wire value = record id
    case "date": return "date";         // native <input type="date"> = BE's exact YYYY-MM-DD
    case "datetime": return "datetime"; // native datetime-local; encode pads seconds (BE layout "2006-01-02T15:04:05")
    default: return null;
  }
}
export function supportsDefaultValue(type: FieldType): boolean {
  return defaultInputKind(type) !== null;
}

// D1 indexed allowlist — POSITIVE mirror of ddl.indexedColExpr (nexus-platform/internal/ddl/
// alter.go): the only types with a generated-column projection. Everything else 422s
// (datetime FIELD_INDEX_UNSUPPORTED_TYPE, file/image FIELD_FILE_NOT_INDEXED, lookup/rollup
// FIELD_COMPUTED_NOT_INDEXED, polymorphic FIELD_POLYMORPHIC_NOT_INDEXED, rest = indexedColExpr
// unknown-type error). unique fields already materialize an indexed physical column (AddField
// skips the indexed path; EditField skips DDL) — offering the toggle there would be a no-op lie.
// relation is likewise excluded: its <field>_id FK column is always indexed at creation.
const INDEXABLE_TYPES = new Set<FieldType>([
  "text", "email", "picklist", "auto_number", "number", "rating", "currency", "date",
]);
export function supportsIndexed(type: FieldType, isUnique: boolean): boolean {
  return !isUnique && INDEXABLE_TYPES.has(type);
}

// Encode the builder's default-input state into the wire string. multi_select is the one
// non-scalar shape the BE accepts: a JSON array string (record_service.go RM-33 (b)).
export function encodeDefaultValue(type: FieldType, scalar: string, multi: string[]): string {
  if (type === "multi_select") return multi.length > 0 ? JSON.stringify(multi) : "";
  const v = scalar.trim();
  // datetime-local emits "YYYY-MM-DDTHH:mm" (no seconds) — BE's naive layout requires them.
  if (type === "datetime" && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(v)) return v + ":00";
  return v;
}

// Decode a stored default for hydrating the inspector inputs (round-trip of encode above).
export function decodeDefaultValue(
  type: FieldType, stored: string | undefined,
): { scalar: string; multi: string[] } {
  const raw = stored ?? "";
  if (type === "multi_select") {
    try {
      const arr: unknown = JSON.parse(raw);
      return { scalar: "", multi: Array.isArray(arr) ? arr.filter((x): x is string => typeof x === "string") : [] };
    } catch { return { scalar: "", multi: [] }; }
  }
  if (type === "datetime") {
    // Best-effort display of an API-written RFC3339 default: datetime-local cannot render a
    // zone suffix, so strip fraction+offset (keeps the wall-clock digits; FE-written defaults
    // are always naive so THEIR round-trip is exact).
    return { scalar: raw.replace(/(\.\d+)?(Z|[+-]\d{2}:\d{2})$/, ""), multi: [] };
  }
  return { scalar: raw, multi: [] };
}

// Edit path for a formula field: PUT /admin/fields/:id replaces options wholesale, so keep every
// stored key (currencyCode, indexed, sensitive…), swap only the expression, and keep the stored
// resultType (column semantics).
export function formulaEditOptions(
  stored: Record<string, unknown> | undefined, formula: string,
): Record<string, unknown> {
  return {
    ...(stored ?? {}),
    formula,
    resultType: (stored?.resultType as string | undefined) ?? "number",
  };
}

// Edit-path companion of buildOptions' indexed param: overlay the flag onto the inspector's
// rebuilt per-type options WITHOUT disturbing sibling keys (values/min/max/…). Ineligible
// fields pass through untouched (undefined stays undefined — PUT omits options, stored value
// kept verbatim, e.g. relation's immutable relatedEntity). For an eligible type this always
// returns an object, so toggling OFF also works on types whose rebuild is otherwise undefined
// (date/email): sending {} replaces options and the absent key reads as indexed=false.
export function withIndexed(
  options: Record<string, unknown> | undefined, eligible: boolean, indexed: boolean,
): Record<string, unknown> | undefined {
  if (!eligible) return options;
  const out: Record<string, unknown> = { ...(options ?? {}) };
  if (indexed) out.indexed = true;
  else delete out.indexed;
  return out;
}

// pii-sensitive-audit-redact (queue #19): field types on which "Trường nhạy cảm" may be set.
// The flag itself is BE-agnostic — internal/services/audit_redact.go masks any flagged field —
// but the Studio inspector sends `options` as a FULL REPLACEMENT, so it may only add a key to
// types whose options this editor already owns end-to-end. relation (immutable options),
// polymorphic_relation / composite (structural options the inspector does not rebuild) and the
// derived read-only types (formula / lookup / rollup / auto_number) are excluded: overlaying a
// key there would send back an options object missing their stored structure and wipe it.
//
// Two honest notes on the edges of this set, both verified against the running server:
//   - `datetime` is the one member with neither an inspector rebuild branch nor membership in
//     INDEXABLE_TYPES, so this flag is the first thing to send it an options object at all
//     ({} when the toggle is off, where the PUT previously omitted options entirely). No engine
//     key lives on a datetime field's options today, and POST/PUT accept {} for it, so nothing
//     is wiped — but it is the member to re-check if datetime ever gains stored config.
//   - `boolean`, `rating` and `user` are LEFT OUT for now, not because they are safe: an HIV
//     status or a disciplinary flag is exactly the PII this exists for. They are out because
//     nothing has asked for them yet and each one widens what a full-replacement PUT touches.
//     Adding one is a one-line change here plus a round-trip check.
const SENSITIVE_TYPES = new Set<FieldType>([
  "text", "rich_text", "number", "date", "datetime", "email", "currency", "picklist", "multi_select",
]);

export function supportsSensitive(type: FieldType): boolean {
  return SENSITIVE_TYPES.has(type);
}

// withSensitive overlays the PII flag on an options object, mirroring withIndexed: an eligible
// type ALWAYS sends an object so toggling OFF reaches the backend as an absent key inside a
// present payload (an omitted `options` would mean "unchanged" and the flag could never be
// cleared). Ineligible types are returned untouched, preserving relation's undefined-options
// contract.
export function withSensitive(
  options: Record<string, unknown> | undefined, eligible: boolean, sensitive: boolean,
): Record<string, unknown> | undefined {
  if (!eligible) return options;
  const out: Record<string, unknown> = { ...(options ?? {}) };
  if (sensitive) out.sensitive = true;
  else delete out.sensitive;
  return out;
}
