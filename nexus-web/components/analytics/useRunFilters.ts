"use client";

// useRunFilters — URL-synced W4 run-overlay filter state (?dateFrom/?dateTo/?ownerId).
// The URL is the single source of truth: parse on read, router.replace on write —
// links share filter state, back/forward navigates it, refresh keeps it.
// NOTE: pages using this hook must render under <Suspense> (useSearchParams — Next 16).

import { useCallback, useMemo } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { filtersToParams, parseFilters, type RunFilters } from "@/lib/runFilters";

const OVERLAY_KEYS = ["dateFrom", "dateTo", "ownerId"] as const;

export function useRunFilters(): [RunFilters, (next: RunFilters) => void] {
  const sp = useSearchParams();
  const pathname = usePathname();
  const router = useRouter();

  const filters = useMemo(() => parseFilters(sp), [sp]);

  const setFilters = useCallback(
    (next: RunFilters) => {
      const params = new URLSearchParams(sp.toString());
      for (const k of OVERLAY_KEYS) params.delete(k);
      for (const [k, v] of Object.entries(filtersToParams(next))) params.set(k, v);
      const qs = params.toString();
      router.replace(qs ? `${pathname}?${qs}` : pathname, { scroll: false });
    },
    [sp, pathname, router],
  );

  return [filters, setFilters];
}
