"use client";

// useEntityOverview (specs/074 FR-010): one row per table for the Studio table list — field count, tables it links to,
// rule count, apps that hold it. Built from four sources that already exist and stored nowhere:
// GET /metadata/fields/:entity per table, GET /metadata/relationships (1 call), GET /admin/workflows (1 call),
// GET /apps/:name/nav per app. Records come from useRecordCounts (one GET /admin/usage), never GET /:entity?limit=1 per table.

import { useMemo } from "react";
import { useQuery, useQueries } from "@tanstack/react-query";
import {
  appNav, listAllRelationships, listFields, listWorkflows,
  type AdminRelationship, type AppSummary, type EntityMeta, type WorkflowSummary,
} from "@/lib/api";
import { entityNamesFromNav } from "@/lib/appScope";
import { qk } from "@/lib/query/keys";
import { useRecordCounts } from "@/lib/operateData";

/** ponytail: one GET /metadata/fields per table, so only the first 50 are asked; the rest show "…" for Trường. E-F2-3 (a count on GET /metadata/entities) removes the cap. */
export const ENTITY_OVERVIEW_CAP = 50;

export interface EntityOverviewRow {
  name: string;
  label: string;
  /** null = not known (beyond the cap, or still loading / failed). */
  fieldCount: number | null;
  /** Codes of the tables this one points to through a relation field (N-N: both ways). */
  linksTo: string[];
  /** null = the rule list failed to load (never a fake 0). */
  ruleCount: number | null;
  apps: AppSummary[];
  /** Live rows (GET /admin/usage via useRecordCounts). null = no figure for this table (no usage block, failed, or table not in it). */
  records: number | null;
}

export interface OverviewInput {
  entities: EntityMeta[];
  fieldCounts: Record<string, number | undefined>;
  relationships: AdminRelationship[];
  workflows: WorkflowSummary[] | null;
  navByApp: { app: AppSummary; names: Set<string> }[];
  recordCounts?: Map<string, number> | null;
}

export function buildEntityOverview({ entities, fieldCounts, relationships, workflows, navByApp, recordCounts }: OverviewInput): EntityOverviewRow[] {
  const links = new Map<string, Set<string>>();
  const add = (from: string, to: string) => {
    if (from === to) return;
    (links.get(from) ?? links.set(from, new Set()).get(from)!).add(to);
  };
  for (const r of relationships) {
    add(r.childEntityName, r.parentEntityName); // the child holds the relation field
    if (r.relationshipType === "N-N") add(r.parentEntityName, r.childEntityName);
  }
  const rules = new Map<string, number>();
  for (const w of workflows ?? []) if (w.entityName) rules.set(w.entityName, (rules.get(w.entityName) ?? 0) + 1);
  return entities.map((e) => ({
    name: e.name,
    label: e.label || e.name,
    fieldCount: fieldCounts[e.name] ?? null,
    linksTo: [...(links.get(e.name) ?? [])].sort(),
    ruleCount: workflows === null ? null : rules.get(e.name) ?? 0,
    apps: navByApp.filter((n) => n.names.has(e.name)).map((n) => n.app),
    records: recordCounts?.get(e.name) ?? null,
  }));
}

export function useEntityOverview(entities: EntityMeta[], apps: AppSummary[], slug: string) {
  const counts = useRecordCounts(slug);
  const asked = useMemo(() => entities.slice(0, ENTITY_OVERVIEW_CAP), [entities]);
  const fieldQs = useQueries({
    queries: asked.map((e) => ({ queryKey: qk.fields(e.name), queryFn: () => listFields(e.name), staleTime: 60_000 })),
  });
  const relQ = useQuery({ queryKey: qk.entityLinks(), queryFn: () => listAllRelationships(), staleTime: 60_000 });
  const wfQ = useQuery({ queryKey: qk.adminWorkflows(), queryFn: listWorkflows, staleTime: 60_000 });
  const navQs = useQueries({
    queries: apps.map((a) => ({ queryKey: qk.appNav(a.name), queryFn: () => appNav(a.name), staleTime: 60_000 })),
  });

  // useQueries returns a fresh array each render: key the memo on the data it carries.
  const fieldSig = fieldQs.map((q) => q.data?.length ?? "-").join(",");
  const navSig = navQs.map((q) => q.dataUpdatedAt).join(",");
  const rows = useMemo(() => {
    const fieldCounts: Record<string, number | undefined> = {};
    asked.forEach((e, i) => {
      const d = fieldQs[i]?.data;
      if (d) fieldCounts[e.name] = d.length;
    });
    const navByApp = apps.flatMap((app, i) => {
      const nav = navQs[i]?.data;
      return nav ? [{ app, names: entityNamesFromNav(nav.items) ?? new Set<string>() }] : [];
    });
    return buildEntityOverview({ entities, fieldCounts, relationships: relQ.data ?? [], workflows: wfQ.data ?? null, navByApp, recordCounts: counts.data });
    // eslint-disable-next-line react-hooks/exhaustive-deps -- fieldQs/navQs are fresh arrays every render; fieldSig/navSig stand for their data
  }, [entities, asked, apps, fieldSig, navSig, relQ.data, wfQ.data, counts.data]);

  return {
    rows,
    loading: {
      fields: fieldQs.some((q) => q.isPending),
      links: relQ.isPending,
      rules: wfQ.isPending,
      apps: navQs.some((q) => q.isPending),
    },
    /** true only when usage answered with a records block: the "Bản ghi" column is drawn iff this holds. */
    hasRecords: counts.data !== null,
    error: { rules: wfQ.isError, links: relQ.isError },
  };
}
