"use client";

// lib/workshopScope.tsx — "this screen is mounted INSIDE an app's workshop" (queue #302).
// The Roles / Users / Field-ACL / Reports / Dashboards / Calendars screens used to be reachable only as
// tenant-wide pages (PANEL_BRIDGE opened them in a new tab, listing every app's roles and tables). The
// workshop now mounts the SAME components under this provider; outside it `useWorkshopScope()` is null
// and every screen behaves exactly as before. Scope is a SOFT display filter (INV-2): ACL stays server-side
// and every filtered list offers "Xem tất cả" one click away.

import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { getRole, type RoleGrant, type RoleSummary } from "@/lib/api";

export interface WorkshopScope {
  slug: string;
  appLabel: string;
  /** Entity slugs of the app's menu; null while the app's nav is still loading (render unscoped, no flash). */
  entityNames: Set<string> | null;
  /** URL of a workshop panel, optionally with extra query params (e.g. { report: id }). */
  panelHref: (panel: string, extra?: Record<string, string>) => string;
}

const Ctx = createContext<WorkshopScope | null>(null);

export function WorkshopScopeProvider({ value, children }: { value: WorkshopScope; children: ReactNode }) {
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

/** null = not inside a workshop (the tenant-wide standalone screens). */
export function useWorkshopScope(): WorkshopScope | null {
  return useContext(Ctx);
}

/**
 * A role belongs to the app's workshop when it grants anything on one of the app's tables or on the
 * wildcard row, or grants nothing yet (a role just created here must not vanish). Pure — unit-tested.
 */
export function roleTouchesScope(grants: readonly RoleGrant[], entityNames: ReadonlySet<string>): boolean {
  const live = grants.filter((g) => g.accessLevel !== "none");
  if (live.length === 0) return true;
  return live.some((g) => g.entityName === "*" || entityNames.has(g.entityName));
}

/** Soft-filter a row list by an entity name; rows with no entity stay (never hide what we can't attribute). */
export function inEntityScope<T>(items: T[], names: ReadonlySet<string> | null, nameOf: (i: T) => string | null | undefined): T[] {
  if (!names) return items;
  return items.filter((i) => {
    const n = nameOf(i);
    return !n || names.has(n);
  });
}

/**
 * Which of `roles` the scope keeps. `relevant` null = no filtering (outside a workshop, nav still loading,
 * or grants not read yet). Plain fetch + state, not react-query: RolesList / UserList also render standalone.
 */
export function useScopedRoleIds(
  roles: RoleSummary[],
  scope: WorkshopScope | null,
): { relevant: Set<string> | null; pending: boolean } {
  const names = scope?.entityNames ?? null;
  const key = roles.map((r) => r.id).join(",");
  const [grants, setGrants] = useState<{ key: string; byRole: Map<string, RoleGrant[]> } | null>(null);
  useEffect(() => {
    if (!names || roles.length === 0) return;
    let alive = true;
    void Promise.all(roles.map((r) => getRole(r.id).then((d) => [r.id, d.actions ?? []] as const).catch(() => null))).then((rows) => {
      if (alive) setGrants({ key, byRole: new Map(rows.filter((x): x is readonly [string, RoleGrant[]] => !!x)) });
    });
    return () => { alive = false; };
    // `roles` is covered by `key`; re-read when the role list changes (a role was created or deleted).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [!!names, key]);
  const ready = !!names && (roles.length === 0 || grants?.key === key);
  const relevant = useMemo(() => {
    if (!names || !ready) return null;
    const s = new Set<string>();
    for (const r of roles) {
      const g = grants?.byRole.get(r.id);
      // A role whose grants failed to read stays visible: unknown is not foreign.
      if (!g || roleTouchesScope(g, names)) s.add(r.id);
    }
    return s;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [names, ready, key, grants]);
  return { relevant, pending: !!names && !ready };
}
