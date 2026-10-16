"use client";

// useBackupTenant — which tenant a Bảo trì › Sao lưu page talks to (specs/079 f7-sao-luu-bang FR-101).
// The id is NEVER read from the URL: for a customer it is the row of GET /admin/projects ["projects"] whose code is S;
// for the workshop itself (S = home) it is `tenantId` of the very /auth/me answer that confirmed the token. Until the
// state is "ok" nothing per-tenant is requested, and every per-tenant query key carries the tenant id (useHomeQuery),
// so a changed S can never show — or write to — the previous project's backups.
import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { listProjects } from "@/lib/api";
import { useStudioIdentity } from "@/lib/studioContext";

export interface BackupTenant {
  state: "loading" | "error" | "missing" | "ok";
  slug: string;
  tenantId?: string;
  /** Display name for sentences ("Ghi vào dự án …"); the code when the registry has none. */
  name?: string;
  isHome: boolean;
}

export function useBackupTenant(slug: string): BackupTenant {
  const identity = useStudioIdentity();
  const me = identity.me;
  const isHome = slug === identity.home;
  // A customer's rows are read only with the home token (no session in this tab): a session means the gate is in the way.
  const askRegistry = identity.status === "ready" && !isHome && !me?.workspace && me?.isPlatformOperator === true;
  const projects = useQuery({ queryKey: ["projects"], queryFn: listProjects, enabled: askRegistry, placeholderData: () => undefined });

  const out = (state: BackupTenant["state"], extra: Partial<BackupTenant> = {}): BackupTenant => ({ state, slug, isHome, ...extra });
  if (identity.status === "loading") return out("loading");
  if (identity.status === "error") return out("error");
  if (isHome) {
    if (!identity.matches(slug) || !me?.tenantId) return out("error");
    return out("ok", { tenantId: me.tenantId, name: slug });
  }
  if (!askRegistry) return out("error");
  if (projects.isPending) return out("loading");
  if (projects.isError) return out("error");
  const row = (projects.data ?? []).find((p) => p.slug === slug);
  return row ? out("ok", { tenantId: row.tenantId, name: row.name || row.slug }) : out("missing");
}

/** A read of the tenant `bt` (state "ok" only); the key carries slug AND tenantId, no previous data is shown while a new key loads. */
export function useHomeQuery<T>(
  bt: BackupTenant,
  key: readonly unknown[],
  fetcher: (tenantId: string) => Promise<T>,
  opts: { refetchInterval?: number | false | ((q: { state: { data: T | undefined } }) => number | false) } = {},
): UseQueryResult<T> {
  const tenantId = bt.tenantId;
  return useQuery<T>({
    queryKey: ["home", bt.slug, tenantId ?? "", ...key],
    queryFn: () => fetcher(tenantId as string),
    enabled: bt.state === "ok" && !!tenantId,
    placeholderData: () => undefined,
    refetchInterval: opts.refetchInterval as never,
    refetchIntervalInBackground: false,
  });
}
