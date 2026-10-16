// What the screen editors draw for an assistant proposal (specs/075 US9, FR-011 + FR-071 f): pure, from the server's
// `change.before/after` (specs/067 plan §7). null = nothing to draw here, the card keeps its ChangeList.
import type { ProposedChange } from "@/components/studio/assistant/ProposalOverlay";

export interface ScreenGhostAdd {
  /** Index of the section in the draft; -1 = the last one. */
  section: number;
  label: string;
  field?: string;
}
export interface ScreenGhostRelabel { field: string; before?: string; after?: string }
export interface ScreenOverlay {
  change: ProposedChange;
  adds: ScreenGhostAdd[];
  /** Field names of cells the proposal takes off the screen ("Sẽ bỏ"). */
  removes: string[];
  relabels: ScreenGhostRelabel[];
}

type Obj = Record<string, unknown>;
const obj = (v: unknown): Obj | null => (v && typeof v === "object" && !Array.isArray(v) ? (v as Obj) : null);
const str = (v: unknown): string | undefined => (typeof v === "string" && v.trim() ? v.trim() : undefined);

interface Cell { field: string; label?: string; section: number }
function cellsOf(layout: unknown): Cell[] | null {
  const sections = obj(layout)?.sections;
  if (!Array.isArray(sections)) return null;
  return sections.flatMap((s, section) => {
    const fields = obj(s)?.fields;
    return (Array.isArray(fields) ? fields : []).flatMap((c): Cell[] => {
      if (typeof c === "string") return [{ field: c, section }];
      const o = obj(c);
      const field = str(o?.field);
      return field ? [{ field, label: str(o?.labelOverride), section }] : [];
    });
  });
}

/** `view`: the screen the editor is on ("edit" = form, "detail" = record page). */
export function screenOverlayFrom(change: ProposedChange, view: "edit" | "detail", labelOf: (field: string) => string): ScreenOverlay | null {
  const out: ScreenOverlay = { change, adds: [], removes: [], relabels: [] };
  if (change.docType === "field") {
    const field = change.target.field;
    const before = str(obj(change.before)?.label);
    const after = str(obj(change.after)?.label);
    if (change.op === "create") {
      if (!after) return null;
      out.adds.push({ section: -1, label: after, field });
    } else if (!field) return null;
    else if (change.op === "delete") out.removes.push(field);
    else out.relabels.push({ field, before, after });
    return out;
  }
  if (change.docType !== "layout") return null;
  const after = obj(change.after);
  const before = obj(change.before);
  const kind = str(after?.viewType) ?? str(before?.viewType) ?? change.target.view;
  // the closed pair the route maps (create/edit = form, detail = record page); list screens have their own editor
  if (kind && (kind === "detail" ? "detail" : kind === "edit" || kind === "create" || kind === "form" ? "edit" : "other") !== view) return null;
  const aCells = change.op === "delete" ? [] : cellsOf(after?.layout);
  const bCells = change.op === "create" ? [] : cellsOf(before?.layout);
  if (!aCells || !bCells) return null;
  const was = new Map(bCells.map((c) => [c.field, c]));
  const now = new Map(aCells.map((c) => [c.field, c]));
  for (const c of aCells) {
    const old = was.get(c.field);
    const nameOf = (x: Cell | undefined) => x?.label ?? labelOf(c.field);
    if (!old) out.adds.push({ section: c.section, label: nameOf(c), field: c.field });
    else if (old.label !== c.label) out.relabels.push({ field: c.field, before: nameOf(old), after: nameOf(c) });
  }
  for (const c of bCells) if (!now.has(c.field)) out.removes.push(c.field);
  return out.adds.length || out.removes.length || out.relabels.length ? out : null;
}
