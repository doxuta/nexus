// CREATE-RECORD value helpers — make the WorkflowBuilder create_record editor TYPE-AWARE (#80).
//
// The action wire shape is { type:"create_record", entityName, values: { field: value, … } } and
// each value must be the SAME per-type shape a normal record create sends (RecordService.Create →
// ValidatePartialRecord): number as number, currency as { amount, currencyCode }, boolean as bool,
// date as "YYYY-MM-DD", datetime as RFC3339-local, picklist/relation as string. The old editor
// emitted every value as a STRING → a number/currency/boolean/rating target 422'd RECORD_INVALID_TYPE.
//
// These pure transforms bridge the reused record-form widgets (components/fields/widgets.tsx
// FieldInput) and that wire shape, reusing lib/recordPayload's buildPayload/reverseValue for every
// type EXCEPT date/datetime — the shared DateInput widget stores DD/MM/YYYY (and buildPayload passes
// it through unchanged, which is why the App record form itself sends DD/MM for a freshly-picked
// date — a pre-existing shared-widget nuance, NOT fixed here). We convert DD/MM ↔ ISO at the
// create_record boundary ONLY, so date/datetime targets are actually 422-safe without touching the
// App-form-wide buildPayload/DateInput. Kept React-free so vitest exercises the emit/hydrate paths.

import type { FieldMeta } from "@/lib/api";
import { buildPayload, reverseValue } from "@/lib/recordPayload";

// Sentinel meaning "the trigger record's owner"; the backend (workflow.resolveCreateValues) swaps it
// for the resolved owner id at run time. Emitted/stored verbatim so it survives round-trips.
export const OWNER_SENTINEL = "$owner";

// A value the engine resolves at FIRE time (mirror of services.isDeferredValue): $owner, $record, a
// $record.<field> copy, or a string with a {{field}} placeholder. Studio has no box that builds the
// last three (API-only), so it must carry them verbatim — type-converting one would drop the key or
// invent false/0 (queue #282: open + save must not change the rule).
export function isDeferredValue(v: unknown): v is string {
  if (typeof v !== "string") return false;
  const t = v.trim();
  return t === OWNER_SENTINEL || t === "$record" || t.startsWith("$record.") || t.includes("{{");
}

// Field types the value editor can render a type-aware input for (v1). Others
// (polymorphic_relation/file/image/composite) are omitted from the target-field picker with a note —
// no editor could emit a correct value for them yet, so offering one would only 422.
export const CR_SUPPORTED_TYPES = new Set<string>([
  "text", "email", "picklist", "multi_select", "number", "currency",
  "boolean", "rating", "date", "datetime", "relation", "rich_text",
]);

// Field types for which the "$owner" quick-pick makes sense (a user/owner-type target holding a
// user id, or a free-text owner column). Number/date/etc. never carry an owner sentinel.
export const OWNER_ELIGIBLE_TYPES = new Set<string>(["relation", "text"]);

// A value row: [fieldName, widgetValue]. widgetValue is WIDGET-NATIVE (exactly what FieldInput binds
// to): currency = decimal string, number = number|"", boolean = bool, date = "DD/MM/YYYY",
// datetime = "DD/MM/YYYY · HH:mm", relation = id string, multi_select = string[], or OWNER_SENTINEL.
export type CrRow = [string, unknown];

// Parse the params.values JSON (array of [field, value] pairs) back to rows. Malformed → []. Widens
// the pre-#80 [string, string] pairs harmlessly (a stored string value stays a string).
export function crValuePairs(raw: string | undefined): CrRow[] {
  if (!raw) return [];
  try {
    const parsed = JSON.parse(raw);
    if (Array.isArray(parsed)) return parsed.map((p) => [String(p?.[0] ?? ""), p?.[1]] as CrRow);
  } catch { /* malformed → empty */ }
  return [];
}

// Serialize rows back into the params.values JSON string (keeps the editor fully controlled — order
// and blank rows preserved, no local state to drift when switching rules).
export function crPairsToString(rows: CrRow[]): string {
  return JSON.stringify(rows);
}

// ── date ⇄ backend converters (isolated to create_record — do NOT touch shared buildPayload/DateInput) ──
const DMY = /^(\d{1,2})\/(\d{1,2})\/(\d{4})$/;               // widget date: DD/MM/YYYY
const ISO_DATE = /^(\d{4})-(\d{2})-(\d{2})$/;                 // backend date: YYYY-MM-DD
const ISO_DATETIME = /^(\d{4})-(\d{2})-(\d{2})[T ](\d{2}):(\d{2})/; // RFC3339 or MySQL literal

// widget "DD/MM/YYYY" → backend "YYYY-MM-DD". Already-ISO or blank passes through (build the local
// string directly — never toISOString, which UTC-shifts the calendar day).
export function dmyToIso(v: string): string {
  const m = v.trim().match(DMY);
  if (!m) return v.trim();
  const [, d, mo, y] = m;
  return `${y}-${mo.padStart(2, "0")}-${d.padStart(2, "0")}`;
}

// backend "YYYY-MM-DD" → widget "DD/MM/YYYY" (hydration, so the calendar highlights instead of
// showing a raw ISO string — item #5, no stale-string downgrade). Non-ISO/blank passes through.
export function isoToDmy(v: string): string {
  const m = v.trim().match(ISO_DATE);
  if (!m) return v.trim();
  const [, y, mo, d] = m;
  return `${d}/${mo}/${y}`;
}

// widget "DD/MM/YYYY · HH:mm" → backend "YYYY-MM-DDTHH:mm:00" (matches datetimeLayout 2006-01-02T15:04:05).
export function dmyTimeToIso(v: string): string {
  const [datePart, timePart] = v.split("·").map((s) => s.trim());
  const iso = dmyToIso(datePart ?? "");
  if (!ISO_DATE.test(iso)) return v.trim();
  const t = /^\d{1,2}:\d{2}/.test(timePart ?? "") ? (timePart as string) : "00:00";
  const [hh, mm] = t.split(":");
  return `${iso}T${hh.padStart(2, "0")}:${mm}:00`;
}

// backend datetime (RFC3339 or "YYYY-MM-DD HH:mm:ss") → widget "DD/MM/YYYY · HH:mm" (hydration).
export function isoTimeToDmy(v: string): string {
  const m = v.trim().match(ISO_DATETIME);
  if (!m) return v.trim();
  const [, y, mo, d, hh, mm] = m;
  return `${d}/${mo}/${y} · ${hh}:${mm}`;
}

// Lossy for the DD/MM widget: non-zero seconds/fraction, or a non-UTC offset ("Z" stays the old, tested path).
const CR_LOSSY = /^\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}(:(?!00(?:\.0+)?(?:Z|[+-]|$))\d{2}|:\d{2}(\.\d+)?[+-]\d{2}:?\d{2}|[+-]\d{2}:?\d{2})/;
const ISO_KEPT = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/;   // an ISO datetime string (never a DD/MM widget value)

// Build the backend { field: value } map from widget-native rows. Reuses the record form's per-field
// buildPayload for the type transform (currency → { amount, currencyCode }, number/rating coerce,
// empty-optional drop) EXCEPT date/datetime (converted above). $owner passes through verbatim.
// Unknown field (metadata not loaded / stale) → value passes through raw so an in-flight edit is
// never silently dropped. Iterating ROWS (not fields) keeps a row referencing a not-yet-loaded field.
export function crEmitValues(rows: CrRow[], fields: FieldMeta[]): Record<string, unknown> {
  const byName = new Map(fields.map((f) => [f.name, f]));
  const out: Record<string, unknown> = {};
  for (const [rawF, v] of rows) {
    const field = rawF.trim();
    if (!field) continue;                                    // blank row
    if (v === OWNER_SENTINEL) { out[field] = OWNER_SENTINEL; continue; }
    if (isDeferredValue(v)) { out[field] = v; continue; }     // resolved at fire time — verbatim
    const meta = byName.get(field);
    if (!meta) { out[field] = v; continue; }                 // fields not loaded → non-destructive passthrough
    if (meta.fieldType === "date" || meta.fieldType === "datetime") {
      const s = typeof v === "string" ? v.trim() : "";
      if (s === "" && !meta.isRequired) continue;
      if (ISO_KEPT.test(s)) { out[field] = s; continue; }    // lossy stored literal kept verbatim (see hydrate)            // optional empty → omit (matches buildPayload)
      out[field] = meta.fieldType === "date" ? dmyToIso(s) : dmyTimeToIso(s);
      continue;
    }
    // Reuse the record form's per-field transform verbatim (same shape a normal record create sends).
    const one = buildPayload([meta], { [field]: v });
    if (field in one) out[field] = one[field];               // else buildPayload dropped an empty optional → omit
  }
  return out;
}

// Reverse a stored create_record values OBJECT into widget-native rows for the editor (round-trip,
// item #5). Mirrors the record form's reverseValue (currency { amount } → decimal string, etc.) but
// for date/datetime converts the stored ISO back to the DateInput's DD/MM display. $owner stays
// literal. reverseValue is NOT idempotent for currency ({amount}→"5"→"") so the caller MUST pass the
// target entity's fields (pre-fetched) and run this exactly ONCE.
export function crHydrateRows(values: Record<string, unknown>, fields: FieldMeta[]): CrRow[] {
  const byName = new Map(fields.map((f) => [f.name, f]));
  return Object.entries(values).map(([k, raw]): CrRow => {
    if (raw === OWNER_SENTINEL) return [k, OWNER_SENTINEL];
    if (isDeferredValue(raw)) return [k, raw];
    const meta = byName.get(k);
    if (!meta) return [k, raw];
    // A stored datetime the DD/MM widget cannot show without loss (seconds, UTC offset) stays as the
    // stored string; crEmitValues sends it back untouched.
    if (meta.fieldType === "datetime" && typeof raw === "string" && ISO_KEPT.test(raw.trim())
      && CR_LOSSY.test(raw.trim())) return [k, raw.trim()];
    if (meta.fieldType === "date") return [k, isoToDmy(typeof raw === "string" ? raw : "")];
    if (meta.fieldType === "datetime") return [k, isoTimeToDmy(typeof raw === "string" ? raw : "")];
    return [k, reverseValue(meta, raw)];
  });
}
