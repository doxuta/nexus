"use client";

// Reads for the Giao cho khách checklist: every app with its readiness (1 + n calls, in parallel), the table
// labels, and the sample-data total. All go through useProjectQuery (closed to a token of another project) and are
// read again on every visit and on window focus, because the fixes happen on other pages.
import { adminApp, adminApps, getSampleDataSummary, listAdminUsers, listEntities, listRoles } from "@/lib/api";
import type { HandoffAppInput } from "@/lib/handoffChecklist";
import { useProjectQuery } from "../useProjectQuery";

const LIVE = { fresh: true, refetchOnWindowFocus: true } as const;

export function useHandoffApps(slug: string) {
  return useProjectQuery<HandoffAppInput[]>(
    slug,
    ["studio", "handoff", "apps"],
    async () => {
      const apps = await adminApps();
      // ponytail: ≈10 apps, one detail call each in parallel; the engine should return readiness in the list beyond that.
      return Promise.all(
        apps.map(async (a): Promise<HandoffAppInput> => {
          try {
            return { id: a.id, name: a.name, label: a.label, readiness: (await adminApp(a.id)).readiness ?? "error" };
          } catch {
            return { id: a.id, name: a.name, label: a.label, readiness: "error" };
          }
        }),
      );
    },
    LIVE,
  );
}

export function useEntityLabels(slug: string) {
  return useProjectQuery(slug, ["studio", "handoff", "entityLabels"], async () => Object.fromEntries((await listEntities()).map((e) => [e.name, e.label || e.name])), LIVE);
}

export function useSampleSummary(slug: string) {
  return useProjectQuery(slug, ["studio", "sampleData"], () => getSampleDataSummary(), LIVE);
}

/** The customer-owner row: who can log in and manage users, and the roles the form offers (all of them). */
export function useHandoffOwner(slug: string) {
  return useProjectQuery(slug, ["studio", "handoff", "owner"], async () => {
    const [users, roles] = await Promise.all([listAdminUsers(), listRoles()]);
    return { users, roles };
  }, LIVE);
}
