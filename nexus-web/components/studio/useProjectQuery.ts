"use client";

// useProjectQuery — the ONLY way the project frame reads anything that depends on the tenant of the token in hand
// (health, publish state, installs, current project…; specs/073 FR-022, FR-025).
//   • closed: `enabled` is projectTokenMatches(S); a fetcher is never called for a project the token does not
//     belong to, not even by refetch()/focus (the guard inside queryFn) — those numbers would be Nexus's shown as the customer's.
//   • isolated: the key carries the project code (and the tenant id on `nha` pages) and `placeholderData` is forced to
//     nothing, because the app default (keepPreviousData) would paint project A's data while B loads.
// test/studio-token-scoped-queries.test.ts fails when a listed fetcher is called outside this hook.
import { useQuery } from "@tanstack/react-query";
import { useStudioIdentity } from "@/lib/studioContext";

export function useProjectQuery<T>(
  slug: string,
  key: readonly unknown[],
  fetcher: () => Promise<T>,
  opts: { tenantId?: string; refetchOnWindowFocus?: boolean; /** never reuse a cached answer (a plan that must be recomputed on every visit) */ fresh?: boolean; /** a read whose silence means "unknown" must not be retried into a 17 s wait */ retry?: boolean; /** ms a cached answer counts as fresh */ staleTime?: number } = {},
) {
  const identity = useStudioIdentity();
  const allowed = identity.matches(slug);
  return useQuery<T>({
    queryKey: ["project", slug, ...(opts.tenantId ? [opts.tenantId] : []), ...key],
    queryFn: () => {
      if (!allowed) throw new Error("useProjectQuery: the token in this tab does not belong to this project");
      return fetcher();
    },
    enabled: allowed,
    refetchOnWindowFocus: opts.refetchOnWindowFocus,
    ...(opts.staleTime != null ? { staleTime: opts.staleTime } : {}),
    ...(opts.retry === false ? { retry: false } : {}),
    placeholderData: () => undefined,
    ...(opts.fresh ? { staleTime: 0, gcTime: 0 } : {}),
  });
}
