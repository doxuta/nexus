"use client";

// Scope gate for every multi-call write loop in the Studio (specs/078 FR-131). Read live (refs), not at the time
// the loop started: between `clearWorkspace` and the page leaving, getToken() falls back to the home token while an
// async loop is still running, so the next chunk would write into the wrong tenant with the customer's ids.
import { usePathname } from "next/navigation";
import { useEffect, useMemo, useRef } from "react";
import type { MassOpGuard } from "@/lib/api";
import { activeProjectSlug, projectSlugAt, transitionPhase, useStudioIdentity } from "@/lib/studioContext";

/** `enabled` false (customer face): no /auth/me call and the gate is always open. The guard is stable; `signal` is read at call time. */
export function useScopeGate(enabled: boolean): MassOpGuard {
  const identity = useStudioIdentity({ enabled });
  const slug = projectSlugAt(usePathname());
  const live = useRef({ identity, slug, enabled });
  useEffect(() => {
    live.current = { identity, slug, enabled };
  });
  const ctl = useRef<AbortController | null>(null);
  useEffect(() => {
    ctl.current = new AbortController();
    return () => ctl.current?.abort();
  }, []);
  return useMemo<MassOpGuard>(
    () => ({
      assertScope: () => {
        const { identity: id, slug: S, enabled: on } = live.current;
        return !on || (transitionPhase() === "idle" && id.matches(S) && activeProjectSlug() === S);
      },
      get signal() {
        return ctl.current?.signal;
      },
    }),
    [],
  );
}
