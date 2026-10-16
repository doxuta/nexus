"use client";

// Chạy thử › Dữ liệu mẫu: ONE GET /admin/sample-data (whole project, never narrowed by ?app=) joined with the
// project's table list, so a table with no sample rows still gets a row of zeros (specs/077 f5-trial-sample-view).
// The key prefix ["project", S, "studio", "sampleData"] is a contract: F4's invalidateAfterApply must not exclude it (T036a).
import type { QueryClient } from "@tanstack/react-query";
import { getSampleDataSummary, listEntities, type SampleEntitySummary } from "@/lib/api";
import { useProjectQuery } from "../useProjectQuery";

export interface SampleRow extends SampleEntitySummary { ambiguous: boolean }

export function joinSampleRows(
  entities: { name: string; label?: string }[],
  summary: { entities: SampleEntitySummary[] },
): SampleRow[] {
  const by = new Map(summary.entities.map((e) => [e.entity, e]));
  const rows = entities.map((e) => {
    const s = by.get(e.name);
    return {
      entity: e.name,
      label: e.label || e.name,
      count: s?.count ?? 0,
      live: s?.live ?? 0,
      inBin: s?.inBin ?? 0,
      bySource: { generator: s?.bySource?.generator ?? 0, ai: s?.bySource?.ai ?? 0, pack: s?.bySource?.pack ?? 0 },
    };
  });
  const seen = new Map<string, number>();
  for (const r of rows) seen.set(r.label, (seen.get(r.label) ?? 0) + 1);
  return rows.map((r) => ({ ...r, ambiguous: (seen.get(r.label) ?? 0) > 1 }));
}

export function useSampleSummary(slug: string) {
  const entities = useProjectQuery(slug, ["entities"], listEntities);
  const summary = useProjectQuery(slug, ["studio", "sampleData"], () => getSampleDataSummary(), { refetchOnWindowFocus: true });
  const rows = entities.data && summary.data ? joinSampleRows(entities.data, summary.data) : [];
  return {
    rows,
    total: summary.data?.total ?? 0,
    isPending: entities.isPending || summary.isPending,
    error: summary.error ?? entities.error,
    refetch: () => { void entities.refetch(); void summary.refetch(); },
  };
}

/** Refresh the sample tally of one project. Only the F5 sample actions (generate / wipe) call this. */
export function invalidateSampleData(qc: QueryClient, slug: string) {
  return qc.invalidateQueries({ queryKey: ["project", slug, "studio", "sampleData"] });
}
