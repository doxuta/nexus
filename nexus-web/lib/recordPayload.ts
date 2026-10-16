// recordPayload — pure transforms between the API record shape and widget-native form state.
//
// The widgets (components/fields/widgets.tsx) store some types in a different shape than the
// backend persists/returns:
//   • currency  → widget keeps a decimal STRING; backend wants { amount, currencyCode }.
//   • number/rating → widget keeps a number; backend stores a number.
//   • multi_select  → widget keeps string[]; backend stores string[].
//   • relation  → widget keeps the related record id (string); backend returns either the bare
//                 id OR (when ?expand=) a { id, label } ref → reverse picks the id.
//
// LayoutForm (T5) seeds react-hook-form defaultValues via `seedValues` and POSTs/PUTs via
// `buildPayload`. Kept pure (no React) so it is unit-testable and shared by any form surface.

import type { FieldMeta } from "@/lib/api";
import { isRelationRef } from "@/lib/api";
import { formatMoney } from "@/lib/format";
import { dependentChoice } from "@/lib/dependentPicklist";

// System / auto-managed fields a user never fills in a create/edit form. The layout itself
// should not reference these, but guard anyway so a hand-edited layout can't surface them.
export const SYSTEM_FIELDS = new Set([
  "id", "tenant_id", "team_set_id", "created_by", "modified_by",
  "created_at", "updated_at", "deleted_at",
]);

// Server-computed / server-assigned field types — never client-writable (mirrors backend's
// computedReadonlyCode, internal/services/record_service.go, the single source of truth
// ValidateRecord/ValidatePartialRecord/CreateBulk all share). All four are modeled in
// lib/metadata.ts's FieldType union (Studio field-builder can create lookup/rollup/auto_number
// since the Studio field-builder update — see components/studio/DataModelBuilder.tsx). Listing
// all four here keeps a form honest for any entity carrying one of these fields (Studio-created
// or seeded via a builtin pack) instead of silently rendering them as a plain (and
// rejected-on-submit) text input.
//
// The record FORM widget (components/fields/widgets.tsx) now renders all four of these as a shared
// read-only ReadonlyComputed badge (Σ / ↦ / # / ƒ) — never an editable input — so a computed or
// server-assigned value can neither be typed over nor submitted from the form. buildPayload still
// strips them defensively before submit as a second line of defence (a mistyped payload can never
// 422 on a read-only field).
export const READONLY_FIELD_TYPES = new Set([
  "formula", "lookup", "rollup", "auto_number",
]);

// Attachment field types. Their value is a [{id,name}] ref array the FILE API owns: upload/delete
// on the record's "Tệp" tab re-sync it from the sys_files catalog. The form only shows a static note
// (AttachmentNote in components/fields/widgets.tsx), so buildPayload never sends them — a partial PUT
// without the key keeps the stored refs. Echoing them back is what broke (specs/060 #270): file went
// out as "[object Object]" (422 "phải là mảng tham chiếu file") and image as null, which silently
// dropped the photo refs; resending the array verbatim would instead make the form a second writer
// that overwrites a file uploaded in another tab after the form opened.
export const ATTACHMENT_FIELD_TYPES = new Set(["file", "image"]);

/**
 * Normalize a stored date/datetime into the wire form the server ACCEPTS on write.
 *
 * The server reads and writes two different dialects for the same field: `GET /:entity/:id`
 * returns a datetime as `"2026-07-14 09:00:00"` (space separator, no zone), while `PUT` demands
 * ISO-8601/RFC3339 and answers the server's OWN output with
 * 422 `RECORD_INVALID_DATE: field "due_date" không phải datetime hợp lệ`.
 *
 * Consequence before this fix, reproduced in the browser 2026-07-29: open ANY ticket, press Sửa,
 * press "Lưu thay đổi" without touching a thing → 422. Every record carrying a datetime was
 * unsaveable unless the user happened to re-pick the date, because the form faithfully echoed
 * back the value the server had just handed it.
 *
 * Applied at BOTH boundaries on purpose: reverseValue (where the foreign format enters the form)
 * and buildPayload (where anything — a draft written by an older build, a clone, a batch row —
 * leaves for the server). The date branch already carried a defensive conversion for exactly this
 * class; this keeps that shape instead of trusting one entry point.
 */
export function toWireDateTime(v: string): string {
  // #278 — every datetime on the wire names its zone, so one instant has one reading:
  //   "YYYY-MM-DD HH:mm(:ss)"  the server's echo, stored UTC (D1)      → …Z
  //   "YYYY-MM-DDTHH:mm(:ss)"  no zone (an old draft; the form used to emit this) → Vietnam wall
  //                             clock, the zone the user typed it in (D6)           → …+07:00
  // Anything already carrying Z / an offset is left verbatim — a legitimate RFC3339 is never rewritten.
  const m = v.match(/^(\d{4}-\d{2}-\d{2})([ T])(\d{2}:\d{2})(:\d{2})?$/);
  if (!m) return v;
  return `${m[1]}T${m[3]}${m[4] ?? ":00"}${m[2] === " " ? "Z" : VN_OFFSET}`;
}

/** Vietnam has no DST; the product shows and takes every wall clock in this zone (D6). */
export const VN_OFFSET = "+07:00";

/**
 * How buildPayload should treat an OPTIONAL field the user left empty.
 *
 *   • "create" — omit the key. Correct for POST: the server applies its own default.
 *   • "update" — send an explicit `null`. Correct for PUT, which is a PARTIAL update: a missing
 *     key means "don't touch this field", so omitting an emptied field silently keeps the old
 *     value. `null` is the engine's clear signal for EVERY type — the store merges with MySQL
 *     JSON_MERGE_PATCH, where a null member removes the key (see mergeAfterImage's note in
 *     internal/services/record_orchestration.go). Verified live against text, picklist, relation,
 *     date, datetime, number, currency and user fields.
 *
 * "create" is the default so existing callers keep their behaviour — MassEditModal in particular
 * DEPENDS on empties being dropped (it disables submit when the payload comes out empty rather
 * than letting the server answer 422 BULK_VALUES_EMPTY).
 */
export type PayloadMode = "create" | "update";

// Type-appropriate empty value so controlled widgets never receive `undefined`
// (prevents React's uncontrolled→controlled flip warning).
export function emptyDefault(f: FieldMeta): unknown {
  switch (f.fieldType) {
    case "boolean": return false;
    case "number": return "";        // number widget shows "" until typed (coerced on submit)
    case "rating": return 0;
    case "multi_select": return [];
    case "currency": return "";      // currency widget works with a decimal string internally
    case "composite": return {};
    case "image": return "";         // display-only in the form (ATTACHMENT_FIELD_TYPES)
    case "formula": return "";       // formula is read-only, never user-entered
    case "polymorphic_relation": return null; // atomic { type, id } | null; picker emits null until both are set
    default: return "";              // text, email, picklist, date, datetime, relation, rich_text
  }
}

// #275: the create form opens with the field's configured default (sys_fields.default_value, always a
// string on the wire — mirror of the server's defaultValue() coercion). Absent/unparseable default →
// the type's empty, so the server stays the authority (it re-applies the default if the key is omitted).
export function defaultSeed(f: FieldMeta): unknown {
  const raw = f.defaultValue?.trim();
  if (!raw) return emptyDefault(f);
  switch (f.fieldType) {
    case "number": case "rating": {
      const n = Number(raw);
      return Number.isFinite(n) ? n : emptyDefault(f);
    }
    case "boolean": {
      const b = raw.toLowerCase();
      if (["true", "1", "t"].includes(b)) return true;
      if (["false", "0", "f"].includes(b)) return false;
      return emptyDefault(f);
    }
    case "multi_select": {
      try {
        const a = JSON.parse(raw);
        return Array.isArray(a) ? a : emptyDefault(f);
      } catch { return emptyDefault(f); }
    }
    case "text": case "email": case "picklist": case "date": case "datetime": case "rich_text":
      return f.defaultValue;
    default: return emptyDefault(f); // currency/composite/relation/...: no string-encodable default
  }
}

// Convert a stored API value back to the widget-native form value (mirror of buildPayload).
// Falls back to emptyDefault when the value is missing / wrong shape (flip guard).
export function reverseValue(f: FieldMeta, raw: unknown): unknown {
  if (raw == null) return emptyDefault(f);
  switch (f.fieldType) {
    case "currency": {
      if (typeof raw === "object" && raw !== null && "amount" in raw) {
        return String((raw as { amount: unknown }).amount);
      }
      if (typeof raw === "number") return String(raw);
      return "";
    }
    case "number":
      // Number widget binds to a number; keep "" when unparseable so the field reads empty.
      return typeof raw === "number" ? raw : Number(raw);
    case "rating":
      return typeof raw === "number" ? raw : Number(raw) || 0;
    case "boolean":
      return typeof raw === "boolean" ? raw : false;
    case "multi_select":
      return Array.isArray(raw) ? raw : [];
    case "composite":
      return typeof raw === "object" && raw !== null && !Array.isArray(raw) ? raw : {};
    case "file":
    case "image":
      // Display-only in the form (ATTACHMENT_FIELD_TYPES): keep the stored [{id,name}] refs as-is
      // rather than coercing them to a string.
      return raw;
    case "formula":
      // Formula is server-computed; reverse to a display string only (never submitted). A
      // currency-resultType formula stores the SAME { amount, currencyCode } shape as a plain
      // currency field (computed_fields.go) — String(raw) on that object would render the
      // literal text "[object Object]" in FieldInput's disabled ƒ box, so format it the same way
      // FieldDisplay does for real currency fields instead of falling through to String().
      if (typeof raw === "object" && raw !== null && "amount" in raw) {
        const cv = raw as { amount: unknown; currencyCode?: string };
        return formatMoney(Number(cv.amount), cv.currencyCode);
      }
      return typeof raw === "string" ? raw : raw == null ? "" : String(raw);
    case "relation":
      // ?expand= resolves a relation to { id, label }; the picker stores the id string.
      if (isRelationRef(raw)) return raw.id;
      return typeof raw === "string" ? raw : "";
    case "polymorphic_relation":
      // Stored atomically as { type, id } (no label expansion server-side). The picker binds to
      // that object directly; keep it verbatim, else null so the picker reads as unset.
      if (typeof raw === "object" && raw !== null && "type" in raw && "id" in raw) return raw;
      return null;
    case "date":
    case "datetime":
      // The server hands datetimes back in a dialect its own writer rejects — normalize on the way
      // IN so the widget, any saved draft, and the eventual PUT all speak one format.
      return typeof raw === "string" ? toWireDateTime(raw) : String(raw);
    default:
      // picklist, text, rich_text
      return typeof raw === "string" ? raw : String(raw);
  }
}

// Strip a source record's data down to what a NEW cloned record may legitimately carry
// (contract §7.1 "?cloneFrom=<id>"). Two classes of keys are removed:
//   • System / auto-managed fields (id, tenant_id, created_by, timestamps…) — a clone is a brand-new
//     row; carrying the source id/audit columns would be wrong (and the server ignores them anyway).
//   • Server-computed / server-assigned field types (formula/lookup/rollup/auto_number, see
//     READONLY_FIELD_TYPES) — these are re-derived / re-sequenced for the new record, so copying the
//     source's value would show a stale figure (and an auto_number would collide semantically).
// Everything else (user-entered field values) is copied verbatim; seedValues() then reverses each
// value into its widget-native form via reverseValue(). Pure (no React) → unit-testable + shared.
export function cloneRecordData(
  fields: FieldMeta[],
  source: Record<string, unknown> | undefined,
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  if (!source) return out;
  const byName = new Map(fields.map((f) => [f.name, f]));
  for (const [key, value] of Object.entries(source)) {
    if (SYSTEM_FIELDS.has(key)) continue;
    const f = byName.get(key);
    // Unknown key (not in this entity's field set) or a computed/assigned type → don't carry it.
    if (!f) continue;
    if (READONLY_FIELD_TYPES.has(f.fieldType)) continue;
    out[key] = value;
  }
  return out;
}

// A dependent picklist child whose DEFAULT the seeded parent does not allow (parent empty, or its
// value lists other children) is seeded empty: the server treats a key the form sends as a user
// choice and would 422 it (specs/052 #255). Repeats to a fixpoint so parent→child→grandchild chains
// settle whatever the field order. Only defaulted (absent-from-recordData) cells are touched.
function dropDisallowedChildDefaults(
  fields: FieldMeta[],
  recordData: Record<string, unknown> | undefined,
  seed: Record<string, unknown>,
): void {
  for (let changed = true; changed; ) {
    changed = false;
    for (const f of fields) {
      if (f.fieldType !== "picklist" || (recordData && f.name in recordData)) continue;
      const cur = seed[f.name];
      if (cur == null || cur === "") continue;
      const choice = dependentChoice(f.options, seed);
      if (choice.kind === "flat" || (choice.kind === "filtered" && choice.values.includes(String(cur)))) continue;
      seed[f.name] = emptyDefault(f);
      changed = true;
    }
  }
}

// Seed react-hook-form defaultValues for the given fields. Edit mode pulls from record.data via
// reverseValue; create mode uses emptyDefault. Keyed by field.name so they line up 1:1 with the
// zod schema (buildLayoutSchema) and SectionGrid's value lookup.
export function seedValues(
  fields: FieldMeta[],
  recordData?: Record<string, unknown>,
  // true on the CREATE form only: absent keys take the field's configured default (#275). Edit
  // keeps empties — a stored record with no value must not be shown (and re-saved) as the default.
  applyDefaults = false,
): Record<string, unknown> {
  const seed: Record<string, unknown> = {};
  for (const f of fields) {
    const absent = !recordData || !(f.name in recordData);
    seed[f.name] = applyDefaults && absent ? defaultSeed(f)
      : recordData ? reverseValue(f, recordData[f.name]) : emptyDefault(f);
  }
  if (applyDefaults) dropDisallowedChildDefaults(fields, recordData, seed);
  return seed;
}

// Build the submit payload from widget-native state. Currency string → { amount, currencyCode };
// optional empties are dropped (but 0 / false kept). The server still validates authoritatively.
export function buildPayload(
  fields: FieldMeta[],
  values: Record<string, unknown>,
  mode: PayloadMode = "create",
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  // On update an emptied OPTIONAL field must travel as an explicit null (a dropped key means
  // "leave it alone", which is why nothing could ever be cleared through the form). On create it
  // is simply omitted. See PayloadMode.
  const dropEmpty = (name: string): boolean => {
    if (mode === "update") out[name] = null;
    return true;
  };
  for (const f of fields) {
    // Server-computed/server-assigned fields (formula/lookup/rollup/auto_number) are never
    // client-writable (mirrors backend's computedReadonlyCode) — never submit them, else the
    // server rejects the whole write with 422 RECORD_*_READONLY.
    if (READONLY_FIELD_TYPES.has(f.fieldType)) continue;
    // Attachments are written by the file API only — see ATTACHMENT_FIELD_TYPES.
    if (ATTACHMENT_FIELD_TYPES.has(f.fieldType)) continue;
    const v = values[f.name];
    if (f.fieldType === "currency") {
      if (!f.isRequired && (v === "" || v == null) && dropEmpty(f.name)) continue;
      const amount = Number(v);
      const currencyCode = (f.options?.currencyCode as string | undefined) ?? "VND";
      out[f.name] = { amount, currencyCode };
      continue;
    }
    if (f.fieldType === "number" || f.fieldType === "rating") {
      // Widgets may hand back "" (empty number field) → drop when optional, else coerce.
      if (v === "" || v == null) {
        if (!f.isRequired && dropEmpty(f.name)) continue;
        // #275: a required number left empty is NOT a 0 — omit it so the server applies the field
        // default (or answers RECORD_REQUIRED_MISSING). Rating's empty IS 0 stars (emptyDefault).
        if (f.fieldType === "number") continue;
        out[f.name] = 0;
        continue;
      }
      out[f.name] = typeof v === "number" ? v : Number(v);
      continue;
    }
    if (f.fieldType === "date" || f.fieldType === "datetime") {
      // Wire format guard: DateInput emits wire ("YYYY-MM-DD" / "YYYY-MM-DDTHH:mm:ss") since the
      // 2026-07-15 fix, but a draft written under the old build (or any residual writer) may still
      // hold the DISPLAY form "DD/MM/YYYY( · HH:mm)" — convert here at the trust boundary instead
      // of letting the backend 422 RECORD_INVALID_DATE on a calendar-picked value.
      if (typeof v === "string" && v.trim() !== "") {
        const m = v.trim().match(/^(\d{1,2})\/(\d{1,2})\/(\d{4})(?:\s*·\s*(\d{2}):(\d{2}))?$/);
        if (m) {
          const iso = `${m[3]}-${m[2].padStart(2, "0")}-${m[1].padStart(2, "0")}`;
          out[f.name] = f.fieldType === "datetime" ? `${iso}T${m[4] ?? "00"}:${m[5] ?? "00"}:00${VN_OFFSET}` : iso;
          continue;
        }
      }
      if (!f.isRequired && (v === "" || v == null) && dropEmpty(f.name)) continue;
      // Second boundary for the read/write dialect split (see toWireDateTime): a value that
      // reached here straight from the server — an old draft, a clone — must still leave in the
      // form the server accepts, not the one it emitted.
      out[f.name] = typeof v === "string" ? toWireDateTime(v) : v;
      continue;
    }
    if (f.fieldType === "polymorphic_relation") {
      // ATOMIC contract: send { type, id } ONLY when BOTH are present (a bare { type } → 422). Strip
      // any stray keys to exactly {type,id}. Incomplete/unset → omit (server enforces required).
      const pv = v as { type?: unknown; id?: unknown } | null | undefined;
      if (pv && typeof pv === "object" &&
          typeof pv.type === "string" && pv.type !== "" &&
          typeof pv.id === "string" && pv.id !== "") {
        out[f.name] = { type: pv.type, id: pv.id };
      } else if (!f.isRequired) {
        dropEmpty(f.name);
      }
      continue;
    }
    // Drop empty optional scalars/arrays; keep 0 and false (valid values handled above).
    if (!f.isRequired) {
      if (v === "" || v == null) { dropEmpty(f.name); continue; }
      if (Array.isArray(v) && v.length === 0) { dropEmpty(f.name); continue; }
    }
    out[f.name] = v;
  }
  return out;
}
