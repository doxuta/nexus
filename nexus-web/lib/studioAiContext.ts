// lib/studioAiContext.ts — which screen the maker is on, as the assistant's `context` (specs/067, 076 FR-129).
// Lookup KEYS only (the server re-resolves them inside the session's project); never a project, never row content.
// A malformed key is dropped, not sent: the server would refuse the whole turn with AI_CONTEXT_INVALID.
import type { AiTurnContextInput } from "@/lib/aiBuilder";
import { ID_RE, NAME_RE, type StudioRoute } from "@/lib/studioRoutes";

const okName = (v?: string): v is string => !!v && v.length <= 64 && NAME_RE.test(v);

/**
 * Whether the docked assistant may mount on this page (specs/076 ai3): a project page that builds inside the session
 * (`phien`, never an operator-only `nha` page) AND a token that belongs to the project (`tokenMatches` = the gate's
 * projectTokenMatches). Anything else mounts nothing and makes no call.
 */
export function assistantEligible(route: StudioRoute | null, tokenMatches: boolean): boolean {
  return tokenMatches && route !== null && route.ctx === "phien" && route.aiPanel !== null;
}

export function studioAiContext(
  route: StudioRoute | null,
  params: { bang?: string; id?: string },
  query: Record<string, string | undefined>,
  recordId?: string,
  /** App found by `resolveAppForEntity` when the URL carries no ?app. */
  resolvedApp?: string,
): AiTurnContextInput {
  const out: AiTurnContextInput = {};
  if (route?.aiPanel) out.panel = route.aiPanel;
  const entity = params.bang ?? query.bang;
  if (okName(entity)) out.entity = entity;
  if (okName(query.app)) out.app = query.app;
  else if (out.entity && okName(resolvedApp)) out.app = resolvedApp;
  if (out.entity && recordId && ID_RE.test(recordId)) out.recordId = recordId;
  return out;
}

export interface AppLookup {
  apps: () => Promise<{ id: string; name: string }[]>;
  entityId: (name: string) => Promise<string | undefined>;
  menu: (appId: string) => Promise<{ itemType: string; refId: string }[]>;
}

/**
 * Which app owns the open table (specs/076 T104): one app ⇒ that app; several ⇒ the single app whose menu lists
 * the table; none or more than one ⇒ undefined (the server then falls back to the table alone). Any failed
 * lookup ⇒ undefined: a context the page could not verify is not sent. Menus are read only when >1 app exists.
 */
export async function resolveAppForEntity(entity: string, l: AppLookup): Promise<string | undefined> {
  try {
    const apps = await l.apps();
    if (apps.length === 1) return apps[0].name;
    if (apps.length < 2) return undefined;
    const id = await l.entityId(entity);
    if (!id) return undefined;
    const menus = await Promise.all(apps.map((a) => l.menu(a.id)));
    const hits = apps.filter((_, i) => menus[i].some((m) => m.itemType === "entity" && m.refId === id));
    return hits.length === 1 ? hits[0].name : undefined;
  } catch {
    return undefined;
  }
}
