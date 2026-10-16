// Authoring rules for a cell's "Chỉ hiện khi" (specs/054 FR-3/5/6 + FilterRow mapping). Pure helpers: the Go validator
// (layout_visible_when.go validateVisibleWhenCells) stays the judge; these only stop the obvious bad choices on screen so
// the builder never has to learn them from a 422.
import type { FieldMeta, VisibleWhen } from "@/lib/api";
import type { FilterRow } from "@/lib/filterModel";
import { updateCell, type DraftFieldCell, type DraftSection } from "@/lib/layoutDraft";

// FR-3: single-valued types the FE and BE compare identically.
const CONTROLLER_TYPES = new Set(["picklist", "boolean", "text", "email", "number", "rating"]);

const fieldCells = (draft: DraftSection[]): DraftFieldCell[] =>
  draft.flatMap((s) => s.cells.filter((c): c is DraftFieldCell => c.kind === "field"));

/** Fields the target may read: on the same form, allowed type, not itself, and no chain leading back to it (FR-5). */
export function controllerChoices(target: string, fields: FieldMeta[], draft: DraftSection[]): FieldMeta[] {
  const cells = fieldCells(draft);
  const cond = new Map(cells.map((c) => [c.field, c.visibleWhen?.field]));
  const loops = (from: string): boolean => {
    for (let n = 0, cur: string | undefined = from; cur && n <= cells.length; n++, cur = cond.get(cur)) {
      if (cur === target) return true;
    }
    return false;
  };
  const onForm = new Set(cells.map((c) => c.field));
  return fields.filter((f) => f.name !== target && onForm.has(f.name) && CONTROLLER_TYPES.has(f.fieldType) && !loops(f.name));
}

/** FR-6: why the target cannot take a condition ("field" = the field is required, "cell" = this cell is). */
export function whenLockedReason(field: FieldMeta, cell: DraftFieldCell): "field" | "cell" | null {
  if (field.isRequired) return "field";
  return cell.required ? "cell" : null;
}

/** The six server operators map onto FilterRow ones; notIn is read-only (only a DB write can make it). */
export function visibleWhenToRow(vw: VisibleWhen, id: string): FilterRow | null {
  switch (vw.op) {
    case "eq": case "neq": return { id, field: vw.field, op: vw.op, value: String(vw.value ?? "") };
    case "in": return { id, field: vw.field, op: "in", value: Array.isArray(vw.value) ? vw.value.join(",") : "" };
    case "empty": return { id, field: vw.field, op: "isnull", value: "" };
    case "notEmpty": return { id, field: vw.field, op: "notnull", value: "" };
    default: return null;
  }
}

const scalar = (raw: string, f: FieldMeta | undefined): string | number | boolean => {
  if (f?.fieldType === "boolean") return raw === "true";
  if (f?.fieldType === "number" || f?.fieldType === "rating") return Number(raw);
  return raw;
};

/** null = the row is not a complete condition yet (no field, or a value-taking operator without a value). */
export function rowToVisibleWhen(row: FilterRow, f: FieldMeta | undefined): VisibleWhen | null {
  if (!row.field) return null;
  if (row.op === "isnull") return { field: row.field, op: "empty" };
  if (row.op === "notnull") return { field: row.field, op: "notEmpty" };
  const parts = row.value.split(",").map((s) => s.trim()).filter(Boolean);
  if (parts.length === 0) return null;
  if (row.op === "in") return { field: row.field, op: "in", value: parts.map((p) => scalar(p, f)) };
  if (row.op !== "eq" && row.op !== "neq") return null;
  const v = scalar(row.value.trim(), f);
  return typeof v === "number" && !Number.isFinite(v) ? null : { field: row.field, op: row.op, value: v };
}

/** Field names whose cell reads `controller`. */
export function dependentsOf(draft: DraftSection[], controller: string): string[] {
  return fieldCells(draft).filter((c) => c.visibleWhen?.field === controller).map((c) => c.field);
}

/** Drop every condition that reads `controller` (used when its cell leaves the form: the server would 422 FR-3). */
export function clearDependents(draft: DraftSection[], controller: string): DraftSection[] {
  return fieldCells(draft)
    .filter((c) => c.visibleWhen?.field === controller)
    .reduce((d, c) => updateCell(d, c.id, { visibleWhen: undefined }), draft);
}
