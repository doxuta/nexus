// screenEditorModel — pure edit steps of the form screen editor (specs/075 L1a). Built on lib/layoutDraft (one
// draft model for the whole Studio); every function returns a new draft or the SAME array when nothing changed.
import type { FieldMeta, LayoutView } from "@/lib/api";
import { clearDependents, dependentsOf } from "@/lib/visibleWhenRules";
import {
  addFieldCell, addSection, addSpacer, findCell, moveCell, moveSection, placedFieldNames, removeSection, updateCell, updateSection,
  type DraftSection, type SectionColumns,
} from "@/lib/layoutDraft";

/** Place a field at the end of `sectionId` (else the last section; a new titled section if there is none). */
export function placeField(
  draft: DraftSection[],
  fieldName: string,
  sectionId: string | null,
  newSectionTitle: string,
): { draft: DraftSection[]; cellId: string | null } {
  if (placedFieldNames(draft).has(fieldName)) return { draft, cellId: null };
  let next = draft;
  if (next.length === 0) next = addSection(next, newSectionTitle);
  const target = next.find((s) => s.id === sectionId) ?? next[next.length - 1];
  next = addFieldCell(next, target.id, fieldName);
  const cellId = next.find((s) => s.id === target.id)?.cells.at(-1)?.id ?? null;
  return { draft: next, cellId };
}

/** Take a cell off the form; a required field must stay (the server 422s the save otherwise). */
export function removeFieldGuarded(
  draft: DraftSection[],
  cellId: string,
  fields: FieldMeta[],
): { draft: DraftSection[]; blocked?: "requiredMustStay" } {
  const hit = findCell(draft, cellId);
  if (!hit) return { draft };
  if (hit.cell.kind === "field") {
    const f = fields.find((x) => x.name === (hit.cell as { field: string }).field);
    if (f?.isRequired) return { draft, blocked: "requiredMustStay" };
  }
  return { draft: draft.map((s) => (s.id === hit.section.id ? { ...s, cells: s.cells.filter((c) => c.id !== cellId) } : s)) };
}

/** One step up (-1) or down (+1); at a section edge the cell crosses into the neighbouring section. */
export function moveCellStep(draft: DraftSection[], cellId: string, dir: -1 | 1): DraftSection[] {
  const hit = findCell(draft, cellId);
  if (!hit) return draft;
  const si = draft.findIndex((s) => s.id === hit.section.id);
  const to = hit.index + dir;
  if (to >= 0 && to < hit.section.cells.length) return moveCell(draft, cellId, hit.section.id, dir === 1 ? to + 1 : to);
  const other = draft[si + dir];
  if (!other) return draft;
  return moveCell(draft, cellId, other.id, dir === 1 ? 0 : other.cells.length);
}

export function moveCellToSection(draft: DraftSection[], cellId: string, sectionId: string): DraftSection[] {
  const target = draft.find((s) => s.id === sectionId);
  const hit = findCell(draft, cellId);
  if (!target || !hit || hit.section.id === sectionId) return draft;
  return moveCell(draft, cellId, sectionId, target.cells.length);
}

/** "Nhãn khách thấy": writes labelOverride only; empty clears it. */
export function setCellLabel(draft: DraftSection[], cellId: string, label: string): DraftSection[] {
  return updateCell(draft, cellId, { labelOverride: label.trim() ? label : undefined });
}

/** Drop an unplaced field at slot `index` of a section (drag from "Trường chưa đặt"); a placed field is refused. */
export function placeFieldAt(draft: DraftSection[], fieldName: string, sectionId: string, index: number): DraftSection[] {
  if (placedFieldNames(draft).has(fieldName) || !draft.some((s) => s.id === sectionId)) return draft;
  return addFieldCell(draft, sectionId, fieldName, index);
}

/**
 * Remove a section. Required fields must stay on the form: they move to the end of the previous section (else the
 * next); when there is no other section the removal is blocked.
 */
export function removeSectionGuarded(
  draft: DraftSection[],
  sectionId: string,
  fields: FieldMeta[],
): { draft: DraftSection[]; blocked?: "requiredMustStay" } {
  const i = draft.findIndex((s) => s.id === sectionId);
  if (i < 0) return { draft };
  const required = new Set(fields.filter((f) => f.isRequired).map((f) => f.name));
  const keep = draft[i].cells.filter((c) => c.kind === "field" && required.has(c.field));
  const rest = removeSection(draft, sectionId);
  if (keep.length === 0) return { draft: rest };
  const host = rest[i - 1] ?? rest[i];
  if (!host) return { draft, blocked: "requiredMustStay" };
  return { draft: rest.map((s) => (s.id === host.id ? { ...s, cells: [...s.cells, ...keep] } : s)) };
}

export const moveSectionStep = (draft: DraftSection[], sectionId: string, dir: -1 | 1): DraftSection[] => moveSection(draft, sectionId, dir);

/** 1-4 columns; narrowing re-clamps every cell's colspan (layoutDraft.updateSection). */
export const setSectionColumns = (draft: DraftSection[], sectionId: string, columns: SectionColumns): DraftSection[] =>
  updateSection(draft, sectionId, { columns });

/** An empty slot at the end of the section ("Ô trống"). */
export const addSpacerCell = (draft: DraftSection[], sectionId: string): DraftSection[] => addSpacer(draft, sectionId);

export const setSectionTitle = (draft: DraftSection[], sectionId: string, title: string): DraftSection[] =>
  updateSection(draft, sectionId, { title });

/** Pending "Bắt buộc nhập" edits after the user flips the switch: an edit equal to the stored flag is dropped. */
export function setRequiredEdit(edits: Record<string, boolean>, field: FieldMeta, on: boolean): Record<string, boolean> {
  const next = { ...edits };
  if (!!field.isRequired === on) delete next[field.name];
  else next[field.name] = on;
  return next;
}

/** Fields as the editor judges them: the stored flag with the pending edits applied. */
export function withRequiredEdits(fields: FieldMeta[], edits: Record<string, boolean>): FieldMeta[] {
  return Object.keys(edits).length === 0 ? fields : fields.map((f) => (f.name in edits ? { ...f, isRequired: edits[f.name] } : f));
}

// ── List screen (specs/075 L2b) ──
// The list layout is not a section draft: it is the ordered columns, the default sort and the two declared view fields.
export const LIST_SORT_FIELDS = ["created_at", "updated_at", "id"] as const;
export type ListSortField = (typeof LIST_SORT_FIELDS)[number];
const NOT_A_COLUMN = new Set(["relation", "polymorphic_relation", "rich_text", "multi_select"]);

export interface ListDraft {
  cols: string[];
  /** Column label overrides already saved (kept, not edited here). */
  labels: Record<string, string>;
  sortField: ListSortField;
  sortOrder: "asc" | "desc";
  /** "" = not declared (the app guesses). */
  kanban: string;
  calendar: string;
}

/** Fields a list column may show (the server drops the rest at serve time). */
export const listCandidateFields = (fields: FieldMeta[]): FieldMeta[] => fields.filter((f) => !NOT_A_COLUMN.has(f.fieldType));

export function listDraftFromView(view: LayoutView | null | undefined, fields: FieldMeta[]): ListDraft {
  const served = view?.columns?.map((c) => c.field);
  const sort = view?.defaultSort?.field;
  return {
    cols: served?.length ? served : listCandidateFields(fields).slice(0, 5).map((f) => f.name),
    labels: Object.fromEntries((view?.columns ?? []).filter((c) => c.labelOverride).map((c) => [c.field, c.labelOverride as string])),
    sortField: (LIST_SORT_FIELDS as readonly string[]).includes(sort ?? "") ? (sort as ListSortField) : "created_at",
    sortOrder: view?.defaultSort?.order === "asc" ? "asc" : "desc",
    kanban: view?.kanbanField ?? "",
    calendar: view?.calendarField ?? "",
  };
}

export const listDraftEquals = (a: ListDraft, b: ListDraft): boolean => JSON.stringify(a) === JSON.stringify(b);

export function toggleListCol(d: ListDraft, name: string, on: boolean, fields: FieldMeta[]): ListDraft {
  if (on) {
    if (d.cols.includes(name) || !listCandidateFields(fields).some((f) => f.name === name)) return d;
    return { ...d, cols: [...d.cols, name] };
  }
  return d.cols.includes(name) ? { ...d, cols: d.cols.filter((c) => c !== name) } : d;
}

export function moveListCol(d: ListDraft, name: string, dir: -1 | 1): ListDraft {
  const i = d.cols.indexOf(name);
  const j = i + dir;
  if (i < 0 || j < 0 || j >= d.cols.length) return d;
  const cols = [...d.cols];
  [cols[i], cols[j]] = [cols[j], cols[i]];
  return { ...d, cols };
}

export const setListSort = (d: ListDraft, sortField: ListSortField, sortOrder: "asc" | "desc"): ListDraft => ({ ...d, sortField, sortOrder });
export const setListKanban = (d: ListDraft, kanban: string): ListDraft => ({ ...d, kanban });
export const setListCalendar = (d: ListDraft, calendar: string): ListDraft => ({ ...d, calendar });

/**
 * layoutJson of the list view. No `sortable` (the server stamps it at serve time); an undeclared Kanban/Calendar field is
 * omitted, not sent blank (the server strict-rejects a blank declaration).
 */
export function listDraftToJson(d: ListDraft): Record<string, unknown> {
  return {
    version: 1,
    columns: d.cols.map((field) => (d.labels[field] ? { field, labelOverride: d.labels[field] } : { field })),
    defaultSort: { field: d.sortField, order: d.sortOrder },
    ...(d.kanban ? { kanbanField: d.kanban } : {}),
    ...(d.calendar ? { calendarField: d.calendar } : {}),
  };
}


// ── bulk steps of the list mode (specs/075 FR-072): pure, and every field left alone comes back with the reason ──
export type BulkSkipReason = "required" | "hasCondition" | "controls" | "noSection";
export interface BulkSkip { field: string; reason: BulkSkipReason; /** Names of the cells that read this field ("controls"). */ detail: string[] }
const skip = (field: string, reason: BulkSkipReason, detail: string[] = []): BulkSkip => ({ field, reason, detail });

const cellOf = (draft: DraftSection[], name: string) => {
  for (const s of draft) for (const c of s.cells) if (c.kind === "field" && c.field === name) return c;
  return null;
};

/** "Đặt bắt buộc": same step as the switch (a field off the form joins the last section); a field with "Chỉ hiện khi" is skipped (FR-6). */
export function applyBulkRequired(
  draft: DraftSection[], stored: FieldMeta[], edits: Record<string, boolean>, names: string[], newSectionTitle: string,
): { draft: DraftSection[]; edits: Record<string, boolean>; skipped: BulkSkip[] } {
  let d = draft;
  let e = edits;
  const skipped: BulkSkip[] = [];
  const byName = new Map(stored.map((f) => [f.name, f]));
  for (const n of names) {
    const f = byName.get(n);
    if (!f) continue;
    if (cellOf(d, n)?.visibleWhen) { skipped.push(skip(n, "hasCondition")); continue; }
    e = setRequiredEdit(e, f, true);
    if (!cellOf(d, n)) d = placeField(d, n, null, newSectionTitle).draft;
  }
  return { draft: d, edits: e, skipped };
}

/** "Bỏ khỏi form": a required field stays (the server 422s; a record page may drop it), and so does the controller of a cell that is not leaving too. */
export function applyBulkRemove(
  draft: DraftSection[], fields: FieldMeta[], names: string[], opts: { ignoreRequired?: boolean } = {},
): { draft: DraftSection[]; skipped: BulkSkip[] } {
  const going = new Set(names);
  const required = new Set(opts.ignoreRequired ? [] : fields.filter((f) => f.isRequired).map((f) => f.name));
  const skipped: BulkSkip[] = [];
  let d = draft;
  for (const n of names) {
    const cell = cellOf(d, n);
    if (!cell) continue;
    if (required.has(n)) { skipped.push(skip(n, "required")); continue; }
    const readers = dependentsOf(d, n).filter((x) => !going.has(x));
    if (readers.length > 0) { skipped.push(skip(n, "controls", readers)); continue; }
    d = clearDependents(d.map((s) => ({ ...s, cells: s.cells.filter((c) => c.id !== cell.id) })), n);
  }
  return { draft: d, skipped };
}

/** "Đưa vào khung … Chuyển": placed cells go to the end of the section, a field off the form joins it there. */
export function applyBulkMoveToSection(
  draft: DraftSection[], names: string[], sectionId: string,
): { draft: DraftSection[]; skipped: BulkSkip[] } {
  if (!draft.some((s) => s.id === sectionId)) return { draft, skipped: names.map((n) => skip(n, "noSection")) };
  let d = draft;
  for (const n of names) {
    const cell = cellOf(d, n);
    d = cell ? moveCellToSection(d, cell.id, sectionId) : placeField(d, n, sectionId, "").draft;
  }
  return { draft: d, skipped: [] };
}
