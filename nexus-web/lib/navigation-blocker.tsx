"use client";

// NavigationBlocker — a global "unsaved changes" guard for client-side navigation (S10 / G1).
//
// WHY this exists (and why it's not just the Link `onNavigate` prop):
//   The generated CRM shell navigates through PLAIN <a href> tags (Sidebar entity links, "Về Nexus
//   Studio", brand, profile avatar) — NOT next/link <Link>. A plain <a> triggers a full document
//   navigation; the only browser hook that fires there is `beforeunload`, whose native prompt is
//   inconsistent (auto-suppressed in automation, no custom Vietnamese copy) and does NOT cover the
//   Topbar back button, which calls useRouter().back() (a pure client-side transition `beforeunload`
//   never sees). So a Link-only guard would miss every real exit path in this app.
//
//   The robust fix that covers EVERY exit while the form is dirty:
//     1. A capture-phase document click interceptor — catches ANY <a> click (sidebar, breadcrumb,
//        "Về danh sách", profile) AND any future next/link <Link>, synchronously, before the browser
//        starts navigating, so window.confirm can preventDefault and keep the user on the page.
//     2. A guarded router (useGuardedRouter) — wraps back/push/replace/forward so programmatic
//        client-side navigation (Topbar back button, save/cancel flows) runs the same confirm.
//     3. The existing window.beforeunload in LayoutForm stays untouched (hard reload / close tab).
//
//   window.confirm is intentional, not lazy: a capture-phase handler must decide preventDefault
//   SYNCHRONOUSLY. An async React modal cannot cancel a full-page <a> navigation in time. The
//   blocker is armed only while a form reports isDirty (setBlocked(true)), so there are no
//   false-positives on clean navigation.

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
} from "react";
import { useRouter } from "next/navigation";

const CONFIRM_MESSAGE = "Bạn có thay đổi chưa lưu. Rời trang và bỏ thay đổi?";

type NavBlockerCtx = {
  /** Arm/disarm the guard. LayoutForm calls setBlocked(isDirty && !saving). */
  setBlocked: (blocked: boolean) => void;
  /** Synchronous confirm — returns true if the user agrees to leave (or guard is disarmed). */
  confirmLeave: () => boolean;
  /** Hold the guard on regardless of setBlocked (a one-time secret on screen); returns the release function. */
  hold: () => () => void;
};

const Ctx = createContext<NavBlockerCtx | null>(null);

export function NavigationBlockerProvider({ children }: { children: React.ReactNode }) {
  // A ref (not state): the capture-phase listener reads the latest value without re-binding, and
  // setBlocked never triggers a re-render of the whole tree.
  const blockedRef = useRef(false);
  // Holds are counted apart from the flag so another form's setBlocked(false) cannot lift them.
  const holdsRef = useRef(0);

  const setBlocked = useCallback((blocked: boolean) => {
    blockedRef.current = blocked;
  }, []);

  const hold = useCallback(() => {
    holdsRef.current += 1;
    let released = false;
    return () => {
      if (!released) {
        released = true;
        holdsRef.current -= 1;
      }
    };
  }, []);

  const confirmLeave = useCallback(() => {
    if (!blockedRef.current && holdsRef.current === 0) return true;
    return window.confirm(CONFIRM_MESSAGE);
  }, []);

  // ── Capture-phase click interceptor for every in-app <a> (plain anchor OR next/link) ──
  useEffect(() => {
    const onClick = (e: MouseEvent) => {
      if (!blockedRef.current && holdsRef.current === 0) return;
      // Respect modifier-clicks / non-left buttons (new tab, middle click) — those open elsewhere
      // and never destroy the current form, so don't nag.
      if (e.defaultPrevented) return;
      if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;

      const anchor = (e.target as HTMLElement | null)?.closest("a");
      if (!anchor) return;

      const href = anchor.getAttribute("href");
      if (!href || href.startsWith("#")) return;            // in-page anchor, not a route change
      if (anchor.target && anchor.target !== "_self") return; // opens a new tab/window
      if (anchor.hasAttribute("download")) return;

      // External / cross-origin links → let beforeunload handle them (this is a route-leave guard).
      try {
        const url = new URL(href, window.location.href);
        if (url.origin !== window.location.origin) return;
        // Same URL (no real navigation) → nothing to guard.
        if (url.pathname === window.location.pathname && url.search === window.location.search) return;
      } catch {
        return;
      }

      if (!confirmLeave()) {
        // Cancel both the browser's default navigation and any framework click handler (next/link
        // attaches its own listener; stopImmediatePropagation prevents it from firing).
        e.preventDefault();
        e.stopImmediatePropagation();
      }
    };

    // capture:true → we run BEFORE next/link's bubble-phase handler and before the default action.
    document.addEventListener("click", onClick, true);
    return () => document.removeEventListener("click", onClick, true);
  }, [confirmLeave]);

  return <Ctx.Provider value={{ setBlocked, confirmLeave, hold }}>{children}</Ctx.Provider>;
}

export function useNavigationBlocker(): NavBlockerCtx {
  const ctx = useContext(Ctx);
  if (!ctx) {
    throw new Error("useNavigationBlocker must be used inside <NavigationBlockerProvider>");
  }
  return ctx;
}

/** Arms/disarms the guard; a no-op without the provider (a builder rendered outside a guarded subtree). */
export function useSetBlocked(): (blocked: boolean) => void {
  return useContext(Ctx)?.setBlocked ?? (() => {});
}

/** Keeps the guard armed while `active`; a no-op without the provider. Immune to other forms' setBlocked(false). */
export function useHoldNavigation(active: boolean): void {
  const hold = useContext(Ctx)?.hold;
  useEffect(() => {
    if (!active || !hold) return;
    return hold();
  }, [active, hold]);
}

/** The unsaved-changes confirm for imperative exits; without the provider nothing is armed, so it always agrees. */
export function useConfirmLeave(): () => boolean {
  return useContext(Ctx)?.confirmLeave ?? (() => true);
}

/**
 * A drop-in replacement for next/navigation's useRouter that runs the unsaved-changes confirm
 * before any programmatic client-side navigation (back/forward/push/replace). Use this in any
 * component that navigates imperatively while a guarded form may be mounted — e.g. the Topbar
 * back button and LayoutFormScreen's cancel handler.
 *
 * If the provider isn't mounted (router used outside a guarded subtree) it degrades gracefully to
 * the plain router so nothing breaks.
 */
export function useGuardedRouter() {
  const router = useRouter();
  const ctx = useContext(Ctx);
  const confirmLeave = ctx?.confirmLeave ?? (() => true);

  return {
    back: () => {
      if (confirmLeave()) router.back();
    },
    forward: () => {
      if (confirmLeave()) router.forward();
    },
    push: (href: string, opts?: Parameters<ReturnType<typeof useRouter>["push"]>[1]) => {
      if (confirmLeave()) router.push(href, opts);
    },
    replace: (href: string, opts?: Parameters<ReturnType<typeof useRouter>["replace"]>[1]) => {
      if (confirmLeave()) router.replace(href, opts);
    },
    refresh: () => router.refresh(),
    prefetch: (href: string) => router.prefetch(href),
  };
}
