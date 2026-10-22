// lib/newEntity.ts — "create a table inside an app", ported from DataModelBuilder (specs/074 T018). Pure of React so the
// list page and any later "Bảng mới" entry point share one flow: create (with the app's id so the server seeds the
// default grants), then add the table to the app's menu. A table without grants or without a menu row is invisible to
// staff and the builder never notices (admin bypasses ACL), so nothing here swallows a failure.

import {
  ApiError, adminApp, createEntityWithAcl, listEntities, replaceAppNav,
  type AclSeedSignal, type AppSummary, type EntityMeta,
} from "@/lib/api";
import { addEntityToAppNav, navRow } from "@/lib/appNav";
import { emitDraftsChanged } from "@/lib/draftEvents";

/** lowercase, Vietnamese diacritics stripped, non-alphanumerics -> "_", must start with a letter. */
export function slugify(str: string): string {
  const s = str.toLowerCase().replace(/đ/g, "d").normalize("NFD")
    .replace(/[̀-ͯ]/g, "").replace(/[^a-z0-9]+/g, "_").replace(/^_+|_+$/g, "");
  return /^[0-9]/.test(s) ? `b_${s}` : s;
}

export interface CreatedEntity {
  entity: EntityMeta;
  aclSeed: AclSeedSignal;
  /** Joined the app's menu (also true when it was already there). */
  addedToNav: boolean;
  /** The table exists but the menu write failed: the message to show. */
  navError: string | null;
  /** Takes the table out of the app's menu again; the table itself stays. */
  undo: () => Promise<void>;
}

export async function createEntityInApp(input: { name: string; label: string; app?: AppSummary | null }): Promise<CreatedEntity> {
  const { app } = input;
  const created = await createEntityWithAcl(input.name, input.label, "", [], app?.id);
  const entity = created.entity;
  let addedToNav = false;
  let navError: string | null = null;
  let entityId = entity.id;
  if (app) {
    try {
      entityId ??= (await listEntities()).find((e) => e.name === entity.name)?.id;
      if (!entityId) throw new Error("entity id unresolved");
      await addEntityToAppNav(app.id, entityId);
      addedToNav = true;
    } catch (e) {
      navError = e instanceof Error ? e.message : String(e);
    }
  }
  // A new table (and its menu row) is a pending change on a delivered workspace: the header recounts.
  emitDraftsChanged("entity-create");
  const undo = async () => {
    if (!app || !entityId) return;
    const cur = await adminApp(app.id);
    await replaceAppNav(app.id, cur.items.filter((i) => !(i.itemType === "entity" && i.refId === entityId)).map(navRow));
    emitDraftsChanged("app"); // the menu PUT is an app draft on a delivered workspace: the header recounts
  };
  return { entity, aclSeed: created.aclSeed, addedToNav, navError, undo };
}

/** Which box of the "Bảng mới" form an error belongs to; null = the form as a whole. */
export function createErrorField(e: unknown): "name" | "label" | null {
  if (!(e instanceof ApiError)) return null;
  if (e.status === 409) return "name";
  const f = e.details?.field;
  return f === "name" || f === "label" ? f : null;
}
