"use client";

// lib/entityOptions.ts — table pickers that tell two same-named tables apart (queue #301).
// "Khách hàng" exists in CRM and in Nha khoa; the picker used to show both as the bare label, and
// picking the wrong one wires a relation/report/widget to another app's table.
// Rule: only a label that appears more than once gets a second line (`code · app`); a unique label
// stays exactly as before, so a tenant with no clash sees no change.

import { useEffect, useMemo, useState } from "react";
import { appNav, appsCatalog, type EntityMeta } from "@/lib/api";
import { tf } from "@/lib/i18n";
import type { ComboboxOption } from "@/components/slds/Combobox";

/** table name → the apps whose menu lists it. */
export type EntityAppMap = ReadonlyMap<string, ReadonlyArray<{ name: string; label: string }>>;
type Opts = { preferApp?: string | null; preferAppOf?: string };

export function entityOptions(
  entities: ReadonlyArray<Pick<EntityMeta, "name" | "label">>,
  appMap: EntityAppMap,
  opts: Opts = {},
): ComboboxOption[] {
  // preferAppOf: "the app(s) this table sits in" — for editors that know a table but not the active app.
  const prefer = new Set<string>(opts.preferApp ? [opts.preferApp] : (appMap.get(opts.preferAppOf ?? "") ?? []).map((a) => a.name));
  const count = new Map<string, number>();
  for (const e of entities) count.set(e.label, (count.get(e.label) ?? 0) + 1);
  const opt = (e: Pick<EntityMeta, "name" | "label">): ComboboxOption => {
    if ((count.get(e.label) ?? 0) < 2) return { value: e.name, label: e.label };
    const apps = (appMap.get(e.name) ?? []).map((a) => a.label).join(", ");
    return { value: e.name, label: e.label, meta: apps ? tf("studio.build.data.pickerMeta", { name: e.name, app: apps }) : e.name };
  };
  const inApp = (e: { name: string }) => !!appMap.get(e.name)?.some((a) => prefer.has(a.name));
  // Stable partition: the app being built first, everything else in the given order.
  return [...entities.filter(inApp), ...entities.filter((e) => !inApp(e))].map(opt);
}

/** One line for a native <select> option, which cannot draw a second line. */
export const entityOptionText = (o: ComboboxOption): string => (o.meta ? `${o.label} · ${o.meta}` : o.label);

const NO_APPS: EntityAppMap = new Map();

/**
 * Table → apps map from every app's nav. Fetches only when `enabled` (a clash exists or an app is
 * preferred), so a screen with nothing to disambiguate makes no extra request.
 * ponytail: plain effect, not react-query — WorkflowBuilder/ApprovalBuilder load with effects and
 * render without a QueryClient in their suites; one small fetch per mount, upgrade to useQueries
 * if a screen mounts this in a loop.
 */
export function useEntityAppMap(enabled: boolean): EntityAppMap {
  const [map, setMap] = useState<EntityAppMap>(NO_APPS);
  useEffect(() => {
    if (!enabled) return;
    let live = true;
    (async () => {
      const apps = await appsCatalog();
      const navs = await Promise.all(apps.map((a) => appNav(a.name)));
      const m = new Map<string, Array<{ name: string; label: string }>>();
      apps.forEach((a, i) => {
        for (const it of navs[i]?.items ?? []) {
          if (it.type !== "entity") continue;
          const k = it.href.replace(/^\//, "");
          m.set(k, [...(m.get(k) ?? []), { name: a.name, label: a.label }]);
        }
      });
      if (live) setMap(m);
    })().catch(() => { /* picker degrades to code-only meta; never blocks the form */ });
    return () => { live = false; };
  }, [enabled]);
  return map;
}

/** Hook form for components: options for the given tables, app map fetched only when it matters. */
export function useEntityOptions(
  entities: ReadonlyArray<Pick<EntityMeta, "name" | "label">>,
  opts: Opts = {},
): ComboboxOption[] {
  const clash = new Set(entities.map((e) => e.label)).size < entities.length;
  const appMap = useEntityAppMap(clash || !!opts.preferApp || !!opts.preferAppOf);
  return useMemo(() => entityOptions(entities, appMap, opts), [entities, appMap, opts.preferApp, opts.preferAppOf]); // eslint-disable-line react-hooks/exhaustive-deps
}
