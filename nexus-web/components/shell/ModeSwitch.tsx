"use client";

// Builder (Nexus) ↔ App (khách) face switching.
// Nexus serves two personas in two separated faces (see docs/ux-spec/USAGE-FLOW-AND-GUIDE.md):
//   - "builder": the internal Nexus factory console (/studio/*) — assemble the client app from metadata.
//   - "app":     the generated end-user app (/home, /:entity, /dashboards…) — operate the built app.
// Only Nexus/admin users get the Builder side; end-users never see it (callers gate on the role).
// The switch itself now lives in each face's SLDS header: the Studio's builder header has the
// "App khách" utility (project strip), the customer face's Setup menu the Studio entry
// (GlobalHeader). The old two-segment ModeSwitch control is gone.
//
// EASE Nước 2 — STATEFUL round-trips: switching faces used to hard-land on the face ROOT
// (/studio, /home), losing your place on every hop — the single biggest "I got teleported"
// mechanic. Now each face remembers its last-visited path (2 localStorage keys) and the
// switcher returns you THERE. Fallback = face root.
import { usePathname } from "next/navigation";
import { useEffect, useSyncExternalStore } from "react";
import { getStorageScope } from "@/lib/api";
import { readForScope } from "@/lib/scopedStorage";
import { isLiveBuilderPath } from "@/lib/studioRoutes";

// Keys carry the scope `${tenantId}:${userId}` (specs/062 L2b): Nexus and each customer workspace remember
// their own last path per face. The bare pre-L2b keys are adopted once at home (readForScope).
const KEY_BUILDER = "nexus.lastPath.builder";
const KEY_APP = "nexus.lastPath.app";
const scoped = (base: string) => (scope: string) => `${base}:${scope}`;

// Guard the remembered path so a bad value (auth screen, cross-face leak) can't hijack the switch.
function sanitize(path: string | null, fallback: string, prefixOk: (p: string) => boolean): string {
  if (!path || !prefixOk(path)) return fallback;
  return path;
}

function remembered(key: string, fallback: string, prefixOk: (p: string) => boolean): string {
  try {
    return sanitize(readForScope(scoped(key), key), fallback, prefixOk);
  } catch {
    return fallback; // private mode — fall back to roots
  }
}

const isBuilderPath = (p: string) => p.startsWith("/studio");
const isAppPath = (p: string) => p.startsWith("/") && !p.startsWith("/studio") && !p.startsWith("/login");
const noSubscribe = () => () => {};

/**
 * Records the current face's path and returns where each face was last left. Used by the Studio's
 * builder header ("App khách") and by the SLDS global header's Studio entry, so each face keeps
 * feeding the other's return path.
 */
export function useFacePaths(current: "builder" | "app") {
  const pathname = usePathname();

  // Record where we are for the CURRENT face.
  useEffect(() => {
    try {
      if (current === "builder" && isBuilderPath(pathname)) {
        localStorage.setItem(scoped(KEY_BUILDER)(getStorageScope().key), pathname);
      } else if (current === "app" && !isBuilderPath(pathname)) {
        localStorage.setItem(scoped(KEY_APP)(getStorageScope().key), pathname);
      }
    } catch { /* private mode — nothing remembered */ }
  }, [current, pathname]);

  // Read the remembered spots during render (roots on the server, so hydration matches). Every
  // navigation re-renders, which re-reads what the effect above stored.
  const builderHref = useSyncExternalStore(noSubscribe, () => remembered(KEY_BUILDER, "/studio", isLiveBuilderPath), () => "/studio");
  const appHref = useSyncExternalStore(noSubscribe, () => remembered(KEY_APP, "/home", isAppPath), () => "/home");
  return { builderHref, appHref };
}
