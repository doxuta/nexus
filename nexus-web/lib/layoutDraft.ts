// layoutDraft — pure grid model behind the W5 Layout Builder WYSIWYG canvas (plan §3.3).
//
// The stored contract (metadata-to-render-contract §2.3) mixes THREE cell shapes: bare string,
// override object, spacer. Editing that union directly is error-prone, so the builder works on a
// normalized draft (every cell = object with a stable local id for dnd-kit/React keys) and
// serializes back on save:
//   • string→object promotion happens on load (draftFromSections),
//   • object→string DEMOTION happens on save when a cell carries no overrides (sectionsToJson) —
//     so the round trip of an untouched layout is byte-identical modulo key order, and admins who
//     hand-wrote terse string cells don't see them "inflate".
//
// Bounds enforced HERE (not just server-side 422): colspan clamped to 1..section.columns,
// fullWidth ⟂ colspan (mutually exclusive — fullWidth wins), helpText ≤200 chars trimmed,
// columns ∈ {1,2,3,4}. The server stays authoritative; this keeps drafts always-saveable.

import type { EntityLayout, FieldMeta, LayoutCell, LayoutSection, VisibleWhen } from "@/lib/api";
import { cellField, isSpacerCell } from "@/lib/api";

export const HELP_TEXT_MAX = 200;
export const DESCRIPTION_MAX = 500;
export type SectionColumns = 1 | 2 | 3 | 4;

export interface DraftFieldCell {
  id: string; // editor-local stable id — never persisted
  kind: "field";
  field: string;
  labelOverride?: string;
  required?: boolean;
  readonly?: boolean;
  fullWidth?: boolean;
  colspan?: number;
  helpText?: string;
  visibleWhen?: VisibleWhen; // specs/054 — carried verbatim; the builder does not edit it yet (#23)
}
export interface DraftSpacerCell {
  id: string;
  kind: "spacer";
}
export type DraftCell = DraftFieldCell | DraftSpacerCell;

export interface DraftSection {
  id: string; // editor-local stable id
  title: string;
  columns: SectionColumns;
  collapsed?: boolean; // preserved round-trip; the builder doesn't edit it (yet)
  cells: DraftCell[];
}

let seq = 0;
function nextId(prefix: string): string {
  seq += 1;
  return `${prefix}-${seq}`;
}

function clampColumns(n: number | undefined): SectionColumns {
  if (n === 1 || n === 2 || n === 3 || n === 4) return n;
  return 2;
}

// ── load: stored sections → draft ──
export function draftFromSections(sections: LayoutSection[] | undefined): DraftSection[] {
  return (sections ?? []).map((s) => ({
    id: nextId("sec"),
    title: s.title ?? "",
    columns: clampColumns(s.columns),
    collapsed: s.collapsed || undefined,
    cells: s.fields.map((cell) => draftCellFrom(cell)),
  }));
}

function draftCellFrom(cell: LayoutCell): DraftCell {
  if (isSpacerCell(cell)) return { id: nextId("cell"), kind: "spacer" };
  const fc = cellField(cell)!;
  return {
    id: nextId("cell"),
    kind: "field",
    field: fc.field,
    labelOverride: fc.labelOverride || undefined,
    required: fc.required,
    readonly: fc.readonly || undefined,
    fullWidth: fc.fullWidth || undefined,
    colspan: typeof fc.colspan === "number" ? fc.colspan : undefined,
    helpText: fc.helpText || undefined,
    visibleWhen: fc.visibleWhen,
  };
}

// ── save: draft → layout_json (v1 grammar, cells demoted when no overrides) ──
export function sectionsToJson(draft: DraftSection[]): { version: 1; sections: Record<string, unknown>[] } {
  return {
    version: 1,
    sections: draft.map((s) => {
      const out: Record<string, unknown> = {
        columns: s.columns,
        fields: s.cells.map((c) => serializeCell(c, s.columns)),
      };
      if (s.title.trim()) out.title = s.title.trim();
      if (s.collapsed) out.collapsed = true;
      return out;
    }),
  };
}

function serializeCell(cell: DraftCell, columns: SectionColumns): LayoutCell {
  if (cell.kind === "spacer") return { spacer: true };
  const out: Record<string, unknown> = { field: cell.field };
  if (cell.labelOverride?.trim()) out.labelOverride = cell.labelOverride.trim();
  if (cell.required !== undefined) out.required = cell.required;
  if (cell.readonly) out.readonly = true;
  if (cell.fullWidth) {
    out.fullWidth = true; // fullWidth ⟂ colspan — fullWidth wins, colspan dropped
  } else if (typeof cell.colspan === "number") {
    const span = Math.min(Math.max(Math.round(cell.colspan), 1), columns);
    if (span > 1) out.colspan = span; // colspan 1 = default → omit
  }
  const help = cell.helpText?.trim();
  if (help) out.helpText = help.slice(0, HELP_TEXT_MAX);
  if (cell.visibleWhen) out.visibleWhen = cell.visibleWhen;
  // DEMOTION: a cell that is nothing but { field } serializes as the terse string form.
  if (Object.keys(out).length === 1) return cell.field;
  return out as LayoutCell;
}

// ── section ops ──
export function addSection(draft: DraftSection[], title = "Section mới"): DraftSection[] {
  return [...draft, { id: nextId("sec"), title, columns: 2, cells: [] }];
}

export function removeSection(draft: DraftSection[], sectionId: string): DraftSection[] {
  return draft.filter((s) => s.id !== sectionId);
}

export function moveSection(draft: DraftSection[], sectionId: string, dir: -1 | 1): DraftSection[] {
  const idx = draft.findIndex((s) => s.id === sectionId);
  const to = idx + dir;
  if (idx < 0 || to < 0 || to >= draft.length) return draft;
  const next = [...draft];
  const [moved] = next.splice(idx, 1);
  next.splice(to, 0, moved);
  return next;
}

export function updateSection(
  draft: DraftSection[],
  sectionId: string,
  patch: Partial<Pick<DraftSection, "title" | "columns">>,
): DraftSection[] {
  return draft.map((s) => {
    if (s.id !== sectionId) return s;
    const columns = patch.columns !== undefined ? clampColumns(patch.columns) : s.columns;
    // Narrowing the grid re-clamps every cell's colspan so the draft never exceeds bounds.
    const cells =
      columns < s.columns
        ? s.cells.map((c) =>
            c.kind === "field" && typeof c.colspan === "number" && c.colspan > columns
              ? { ...c, colspan: columns }
              : c,
          )
        : s.cells;
    return { ...s, ...patch, columns, cells };
  });
}

// ── cell ops ──
export function addFieldCell(draft: DraftSection[], sectionId: string, fieldName: string, index?: number): DraftSection[] {
  const cell: DraftFieldCell = { id: nextId("cell"), kind: "field", field: fieldName };
  return insertCell(draft, sectionId, cell, index);
}

export function addSpacer(draft: DraftSection[], sectionId: string, index?: number): DraftSection[] {
  return insertCell(draft, sectionId, { id: nextId("cell"), kind: "spacer" }, index);
}

function insertCell(draft: DraftSection[], sectionId: string, cell: DraftCell, index?: number): DraftSection[] {
  return draft.map((s) => {
    if (s.id !== sectionId) return s;
    const cells = [...s.cells];
    const at = index === undefined ? cells.length : Math.min(Math.max(index, 0), cells.length);
    cells.splice(at, 0, cell);
    return { ...s, cells };
  });
}

export function removeCell(draft: DraftSection[], cellId: string): DraftSection[] {
  return draft.map((s) =>
    s.cells.some((c) => c.id === cellId) ? { ...s, cells: s.cells.filter((c) => c.id !== cellId) } : s,
  );
}

export function findCell(draft: DraftSection[], cellId: string): { section: DraftSection; cell: DraftCell; index: number } | null {
  for (const s of draft) {
    const i = s.cells.findIndex((c) => c.id === cellId);
    if (i >= 0) return { section: s, cell: s.cells[i], index: i };
  }
  return null;
}

/** Move a cell (possibly across sections) to `targetIndex` inside `targetSectionId`. */
export function moveCell(draft: DraftSection[], cellId: string, targetSectionId: string, targetIndex: number): DraftSection[] {
  const found = findCell(draft, cellId);
  if (!found) return draft;
  const sameSection = found.section.id === targetSectionId;
  // Remove first, then insert — indices in the TARGET are computed after removal.
  const without = draft.map((s) =>
    s.id === found.section.id ? { ...s, cells: s.cells.filter((c) => c.id !== cellId) } : s,
  );
  return without.map((s) => {
    if (s.id !== targetSectionId) return s;
    const cells = [...s.cells];
    let at = Math.min(Math.max(targetIndex, 0), cells.length);
    // Moving down within the same section: the removal shifted later indices left by one.
    if (sameSection && targetIndex > found.index) at = Math.min(Math.max(targetIndex - 1, 0), cells.length);
    cells.splice(at, 0, found.cell);
    return { ...s, cells };
  });
}

export function updateCell(
  draft: DraftSection[],
  cellId: string,
  patch: Partial<Omit<DraftFieldCell, "id" | "kind" | "field">>,
): DraftSection[] {
  return draft.map((s) => {
    if (!s.cells.some((c) => c.id === cellId)) return s;
    return {
      ...s,
      cells: s.cells.map((c) => {
        if (c.id !== cellId || c.kind !== "field") return c;
        const next: DraftFieldCell = { ...c, ...patch };
        // Bounds + exclusivity enforced at edit time so the draft is always saveable.
        if (next.fullWidth) next.colspan = undefined;
        if (typeof next.colspan === "number") {
          next.colspan = Math.min(Math.max(Math.round(next.colspan), 1), s.columns);
        }
        if (next.helpText !== undefined) next.helpText = next.helpText.slice(0, HELP_TEXT_MAX);
        return next;
      }),
    };
  });
}

// ── derived views ──
export function placedFieldNames(draft: DraftSection[]): Set<string> {
  const names = new Set<string>();
  for (const s of draft) for (const c of s.cells) if (c.kind === "field") names.add(c.field);
  return names;
}

export function unplacedFields(fields: FieldMeta[], draft: DraftSection[]): FieldMeta[] {
  const placed = placedFieldNames(draft);
  return fields.filter((f) => !placed.has(f.name));
}

/** Floor rule (edit view): required fields NOT placed anywhere — backend 422s the save. */
export function missingRequired(fields: FieldMeta[], draft: DraftSection[]): FieldMeta[] {
  const placed = placedFieldNames(draft);
  return fields.filter((f) => f.isRequired && !placed.has(f.name));
}

/**
 * Names of the fields the edit form shows, or null while the layout is unknown (not loaded / none).
 * A default or empty edit view shows every field; a custom one only the placed cells (spacers carry no
 * field; a visibleWhen cell is still on the form, it just appears conditionally).
 */
export function onForm(layout: EntityLayout | null | undefined, fields: FieldMeta[]): Set<string> | null {
  const edit = layout?.views?.edit;
  if (!edit) return null;
  if (edit.isDefault || !edit.sections || edit.sections.length === 0) return new Set(fields.map((f) => f.name));
  return placedFieldNames(draftFromSections(edit.sections));
}

/** Canonical dirty check: serialize both sides and compare (key order is deterministic here). */
export function draftEquals(a: DraftSection[], b: DraftSection[]): boolean {
  return JSON.stringify(sectionsToJson(a)) === JSON.stringify(sectionsToJson(b));
}
