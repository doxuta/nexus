// lib/studioRecentProjects.ts — "Gần đây" of the Studio project switcher (specs/073 FR-083, T422).
// Inside a build session GET /admin/projects answers 403, so the switcher cannot list customers; it lists the ones this
// operator built for lately. Only a code and a name are kept (nothing a token or tenant id could ride in), per home
// user, newest first, 8 at most. Storage that throws or holds junk reads as an empty list: this is a convenience and
// must never break the switcher.
import { SLUG_RE } from "@/lib/studioRoutes";

export const MAX_RECENT_PROJECTS = 8;
const NAME_MAX = 120;

export interface RecentProject {
  slug: string;
  name: string;
}

export const recentProjectsKey = (homeUserId: string) => `nexus.studio.recentProjects:${homeUserId}`;

function clean(p: unknown): RecentProject | null {
  if (!p || typeof p !== "object") return null;
  const { slug, name } = p as Record<string, unknown>;
  if (typeof slug !== "string" || !SLUG_RE.test(slug) || typeof name !== "string" || !name.trim()) return null;
  return { slug, name: name.trim().slice(0, NAME_MAX) };
}

export function readRecentProjects(homeUserId: string): RecentProject[] {
  if (!homeUserId) return [];
  try {
    const raw = window.localStorage.getItem(recentProjectsKey(homeUserId));
    const list: unknown = raw ? JSON.parse(raw) : [];
    return Array.isArray(list) ? list.map(clean).filter((p): p is RecentProject => p !== null).slice(0, MAX_RECENT_PROJECTS) : [];
  } catch {
    return [];
  }
}

/** Newest first; a repeat moves to the front (and takes the latest name). */
export function rememberProject(homeUserId: string, project: RecentProject): void {
  const p = clean(project);
  if (!homeUserId || !p) return;
  try {
    const next = [p, ...readRecentProjects(homeUserId).filter((x) => x.slug !== p.slug)].slice(0, MAX_RECENT_PROJECTS);
    window.localStorage.setItem(recentProjectsKey(homeUserId), JSON.stringify(next));
  } catch {
    // private mode / quota: nothing remembered
  }
}
