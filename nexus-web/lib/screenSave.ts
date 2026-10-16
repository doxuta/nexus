// screenSave — the three-phase write of the screen editor (specs/075 L1c, T066). The server reads the STORED is_required
// when it judges a layout (054 FR-6) and a field edit reads the STORED layout, so one Lưu is ordered:
//   1. fields turned OFF "Bắt buộc nhập"  (so a layout may now hide them or drop them off the form)
//   2. the layout                         (POST the first time, PUT after)
//   3. fields turned ON "Bắt buộc nhập"   (so they are already on the form)
// Phase 1 failing stops before the layout; phase 2 failing leaves phase 1 done and skips phase 3; phase 3 goes on past a
// failed field and reports the partial result. `remaining` is what the editor must keep as pending edits.
import { ApiError, createLayout, updateField, updateLayout, type FieldMeta } from "@/lib/api";
import { sectionsToJson, type DraftSection } from "@/lib/layoutDraft";
import { tf } from "@/lib/i18n";

export type RequiredEdits = Record<string, boolean>;

export type ScreenSaveResult =
  | { ok: true; layoutId: string | null }
  | {
      ok: false;
      phase: 1 | 2 | 3;
      error: unknown;
      failedField?: string;
      /** The layout was deleted elsewhere (PUT 404 LAYOUT_NOT_FOUND). */
      layoutGone?: boolean;
      /** Layout id after phase 2 (null when it was not written). */
      layoutId: string | null;
      done: number;
      total: number;
      remaining: RequiredEdits;
    };

export async function saveScreen(a: {
  entity: string;
  viewType?: "edit" | "detail";
  layoutId: string | null;
  asNew: boolean;
  draft: DraftSection[];
  fields: FieldMeta[];
  edits: RequiredEdits;
}): Promise<ScreenSaveResult> {
  const byName = new Map(a.fields.map((f) => [f.name, f]));
  // an edit equal to the stored value is no edit
  const pending = Object.entries(a.edits).filter(([n, to]) => byName.has(n) && !!byName.get(n)!.isRequired !== to);
  const offs = pending.filter(([, to]) => !to);
  const ons = pending.filter(([, to]) => to);
  const rest = (list: Array<[string, boolean]>): RequiredEdits => Object.fromEntries(list);
  const write = (name: string, to: boolean) => updateField(byName.get(name)!.id as string, { isRequired: to });

  for (let i = 0; i < offs.length; i++) {
    try {
      await write(offs[i][0], false);
    } catch (error) {
      return { ok: false, phase: 1, error, failedField: offs[i][0], layoutId: a.layoutId, done: i, total: offs.length, remaining: rest([...offs.slice(i), ...ons]) };
    }
  }

  let layoutId = a.layoutId;
  try {
    const layoutJson = sectionsToJson(a.draft) as unknown as Record<string, unknown>;
    if (layoutId && !a.asNew) await updateLayout(layoutId, layoutJson);
    else layoutId = (await createLayout({ entityName: a.entity, viewType: a.viewType ?? "edit", layoutJson, isDefault: false })).id;
  } catch (error) {
    const layoutGone = error instanceof ApiError && error.status === 404 && error.code === "LAYOUT_NOT_FOUND";
    return { ok: false, phase: 2, error, layoutGone, layoutId: a.layoutId, done: 0, total: ons.length, remaining: rest(ons) };
  }

  const failed: Array<[string, boolean]> = [];
  let firstErr: { name: string; error: unknown } | null = null;
  for (const [name, to] of ons) {
    try {
      await write(name, to);
    } catch (error) {
      failed.push([name, to]);
      firstErr ??= { name, error };
    }
  }
  if (firstErr) {
    return { ok: false, phase: 3, error: firstErr.error, failedField: firstErr.name, layoutId, done: ons.length - failed.length, total: ons.length, remaining: rest(failed) };
  }
  return { ok: true, layoutId };
}

/** "Đã đặt bắt buộc 3 trên 5 trường; lỗi: <trường>: <câu>" for a partial phase 3. */
export function savedRequiredSummary(r: Extract<ScreenSaveResult, { ok: false }>, fields: FieldMeta[], sentence: (e: unknown) => string): string {
  const f = fields.find((x) => x.name === r.failedField);
  return tf("studio.build.screens.save.partial", { done: r.done, total: r.total, field: f?.label || r.failedField || "", message: sentence(r.error) });
}

/** Save the LIST view: a single layout write (no field flags involved). Same 404 reading as phase 2 above. */
export async function saveListScreen(a: {
  entity: string;
  layoutId: string | null;
  asNew: boolean;
  layoutJson: Record<string, unknown>;
}): Promise<{ ok: true; layoutId: string } | { ok: false; error: unknown; layoutGone: boolean; layoutId: string | null }> {
  try {
    if (a.layoutId && !a.asNew) {
      await updateLayout(a.layoutId, a.layoutJson);
      return { ok: true, layoutId: a.layoutId };
    }
    const ref = await createLayout({ entityName: a.entity, viewType: "list", layoutJson: a.layoutJson, isDefault: false });
    return { ok: true, layoutId: ref.id };
  } catch (error) {
    const layoutGone = error instanceof ApiError && error.status === 404 && error.code === "LAYOUT_NOT_FOUND";
    return { ok: false, error, layoutGone, layoutId: a.layoutId };
  }
}
