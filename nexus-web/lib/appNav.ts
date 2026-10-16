import { adminApp, replaceAppNav, type AdminAppNavRow, type AppNavReplaceItem } from "@/lib/api";

/** An existing menu row as the replace-all PUT wants it: type + ref + the custom label (if any). */
export const navRow = (i: AdminAppNavRow): AppNavReplaceItem => ({
  itemType: i.itemType,
  refId: i.refId,
  ...(i.label ? { label: i.label } : {}),
});

/**
 * The ONE menu-merge rule (Go twin: internal/ai mergeAppNav; both read
 * nexus-platform/internal/ai/testdata/app_nav_merge_cases.json): keep every current row (type,
 * custom label, order), append `add` rows not already there, dedupe by (itemType, refId).
 */
export function mergeAppNavKeys(current: AppNavReplaceItem[], add: AppNavReplaceItem[]): AppNavReplaceItem[] {
  const seen = new Set<string>();
  return [...current, ...add].filter((n) => {
    const k = `${n.itemType}:${n.refId}`;
    return !!n.refId && !seen.has(k) && !!seen.add(k);
  });
}

/**
 * Append a table to an app's menu. PUT /admin/apps/:id/nav is replace-all, so this is a
 * read-modify-write that keeps EVERY existing row (tables, dashboards, custom labels) and adds the
 * table last. Already in the menu ⇒ no PUT at all. Throws when either call fails — callers decide
 * how to say so (never swallow: a table the menu lacks is invisible to staff).
 */
export async function addEntityToAppNav(appId: string, entityId: string): Promise<void> {
  const detail = await adminApp(appId);
  if (detail.items.some((i) => i.itemType === "entity" && i.refId === entityId)) return;
  await replaceAppNav(appId, mergeAppNavKeys(detail.items.map(navRow), [{ itemType: "entity", refId: entityId }]));
}
