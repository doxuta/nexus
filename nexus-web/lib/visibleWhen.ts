// Conditional cells (specs/054-layout-visible-when FR-7) — the FE port of the Go evaluator
// (nexus-platform/internal/services/layout_visible_when.go: conditionHolds · sameScalar ·
// fieldVisible, and isEmpty in record_service.go). Both run the golden file
// internal/services/testdata/visible_when_cases.json (lib/visibleWhen.test.ts), so the form, the
// record page and the server judge a cell the same way. Change the two together or not at all.
//
// Which values to judge on is the caller's business: the record page passes the stored record;
// the form's create mode must pass the create image (payload + coerced defaults — FR-7 "one
// image"), never the raw widget values.
import { cellField, type FieldMeta, type LayoutSection, type VisibleWhen } from "@/lib/api";

const OPS = new Set<VisibleWhen["op"]>(["eq", "neq", "in", "notIn", "empty", "notEmpty"]);

// Own properties only: a field named "constructor" must not read Object.prototype.
const read = (values: Record<string, unknown>, name: string): unknown =>
  Object.hasOwn(values, name) ? values[name] : undefined;

// services.isEmpty: absent / null / blank string / empty array / empty object. 0 and false are values.
function isEmpty(v: unknown): boolean {
  if (v === undefined || v === null) return true;
  if (typeof v === "string") return v.trim() === "";
  if (Array.isArray(v)) return v.length === 0;
  if (typeof v === "object") return Object.keys(v).length === 0;
  return false;
}

// services.sameScalar: same JSON type and equal — the string "5" is not the number 5.
function sameScalar(a: unknown, b: unknown): boolean {
  const t = typeof a;
  return (t === "string" || t === "number" || t === "boolean") && typeof b === t && a === b;
}

function conditionHolds(c: VisibleWhen, values: Record<string, unknown>): boolean {
  const v = read(values, c.field);
  switch (c.op) {
    case "empty":
      return isEmpty(v);
    case "notEmpty":
      return !isEmpty(v);
    case "eq":
      return !isEmpty(v) && sameScalar(v, c.value);
    case "neq":
      return isEmpty(v) || !sameScalar(v, c.value);
    case "in":
    case "notIn": {
      const inList = Array.isArray(c.value) && !isEmpty(v) && c.value.some((x) => sameScalar(v, x));
      return inList === (c.op === "in");
    }
  }
  return true; // unknown op: showing is the safe side, as in Go
}

/** Is `name` shown? Its condition holds AND its controller is shown, up the chain (transitive).
 *  No condition ⇒ shown. A cycle (only from rows written straight to the DB) ⇒ shown. */
export function fieldVisible(name: string, conds: Map<string, VisibleWhen>, values: Record<string, unknown>): boolean {
  for (let steps = 0; steps <= conds.size; steps++) {
    const c = conds.get(name);
    if (!c) return true;
    if (!conditionHolds(c, values)) return false;
    name = c.field;
  }
  return true;
}

// services.defaultValue() for the six controller types: the field's configured default (always a
// string on the wire) coerced to its type. undefined = no default / does not coerce ⇒ the field
// stays absent (the server then 422s the whole record, so the image is moot).
function coerceDefault(f: FieldMeta): unknown {
  const raw = f.defaultValue?.trim();
  if (!raw) return undefined;
  switch (f.fieldType) {
    case "text":
    case "email":
    case "picklist":
      return f.defaultValue;
    case "number":
    case "rating": {
      const n = Number(raw);
      return Number.isFinite(n) ? n : undefined;
    }
    case "boolean": {
      const b = raw.toLowerCase();
      if (["true", "1", "t"].includes(b)) return true;
      if (["false", "0", "f"].includes(b)) return false;
      return undefined;
    }
  }
  return undefined;
}

/** The image a CREATE is judged on (FR-7 "one image", mirror of services.createImage): the POST
 *  payload as sent, plus for every field it omits or sends blank the field's default. Judge
 *  visibility on THIS, never on what the widgets show. Used only to judge — never sent. */
export function createImage(fields: FieldMeta[], payload: Record<string, unknown>): Record<string, unknown> {
  const image = { ...payload };
  for (const f of fields) {
    if (!isEmpty(read(image, f.name))) continue;
    const d = coerceDefault(f);
    if (d !== undefined) image[f.name] = d;
  }
  return image;
}

/** target field → its condition, across every section of one view (edit or detail). */
export function layoutConditions(sections: LayoutSection[]): Map<string, VisibleWhen> {
  const conds = new Map<string, VisibleWhen>();
  for (const s of sections) {
    for (const cell of s.fields) {
      const fc = cellField(cell);
      const vw = fc?.visibleWhen;
      if (fc && vw && typeof vw.field === "string" && vw.field !== "" && OPS.has(vw.op)) conds.set(fc.field, vw);
    }
  }
  return conds;
}

/** The sections a read-only surface shows for `values`: cells whose condition fails are dropped
 *  (spacers kept), and a section left with no field at all drops out rather than rendering an empty
 *  titled box. A layout without conditions comes back as the same array. */
export function visibleSections(sections: LayoutSection[], values: Record<string, unknown>): LayoutSection[] {
  const conds = layoutConditions(sections);
  if (conds.size === 0) return sections;
  const out: LayoutSection[] = [];
  for (const s of sections) {
    const fields = s.fields.filter((cell) => {
      const fc = cellField(cell);
      return !fc || fieldVisible(fc.field, conds, values);
    });
    if (fields.length === s.fields.length) out.push(s);
    else if (fields.some((cell) => cellField(cell))) out.push({ ...s, fields });
  }
  return out;
}
