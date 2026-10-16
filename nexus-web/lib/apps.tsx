"use client";

// lib/apps.tsx — active-app context (App container Phase 2, ../specs/010-app-container/spec.md §7).
// The active app is SIDEBAR CONTEXT ONLY (spec §0 locked decision #1): localStorage
// `nexus.activeApp` holds the app slug (special "*" = All Items); a `?app=<slug>` query
// param wins ONCE on load and is then cleaned from the URL (deep-link support without
// polluting history). URLs stay flat — never an /a/:app prefix.
//
// Resolution (pure, unit-tested):
//   stored "*"                    → All Items
//   stored slug that still exists → that app
//   stored slug DELETED           → All Items (never silently jump to another app)
//   nothing stored                → the app the caller's ROLE is default for (defaultForCaller),
//                                   else the tenant's isDefault app,
//                                   else the tenant's ONLY app (exactly 1 — F7-3: a curated
//                                   single-app tenant must land in it, not "Tất cả dữ liệu"),
//                                   else All Items
//
// INV-1: 0 apps ⇒ resolution always lands on All Items ⇒ the sidebar keeps today's exact
// behavior. INV-2: All Items stays reachable from every state (switcher menu + nav footer).

import {
  createContext, useCallback, useContext, useEffect, useMemo, useState,
  type ReactNode,
} from "react";
import { useQuery } from "@tanstack/react-query";
import {
  LayoutGrid, Briefcase, Users, Ticket, GraduationCap, Stethoscope, Building2,
  ShoppingCart, BarChart3, Package, Phone, CalendarDays, HeartHandshake,
  type LucideIcon,
} from "lucide-react";
import { appsCatalog, appNav, getStorageScope, type AppSummary, type AppNavEntry } from "@/lib/api";
import { readForScope } from "@/lib/scopedStorage";
import { inProjectPath } from "@/lib/studioRoutes";
import { qk } from "@/lib/query/keys";
import { useAuth } from "@/lib/auth/AuthProvider";

/** Sentinel slug for the implicit "Tất cả dữ liệu" (All Items) lens. */
export const ALL_ITEMS = "*";

// Scoped `${tenantId}:${userId}` (specs/062 L2b); the bare pre-L2b key is adopted once at home.
const LEGACY_KEY = "nexus.activeApp";
const storageKey = (scope: string) => `${LEGACY_KEY}:${scope}`;

// ── Pure resolution logic (unit-tested in apps.test.ts) ────────────────────────

export function resolveActiveApp(stored: string | null, apps: AppSummary[]): string {
  if (stored === ALL_ITEMS) return ALL_ITEMS;
  if (stored) {
    // A stored slug survives only while its app exists; a deleted app falls back to
    // All Items — NOT to the default app (jumping lenses silently would be confusing).
    return apps.some((a) => a.name === stored) ? stored : ALL_ITEMS;
  }
  // No default flagged: a tenant with EXACTLY ONE app still lands in it (F7-3) — the curated
  // lens is the whole point of a single-app tenant. 2+ apps without a default stays All Items
  // (never guess between lenses).
  return apps.find((a) => a.defaultForCaller)?.name
    ?? apps.find((a) => a.isDefault)?.name
    ?? (apps.length === 1 ? apps[0].name : ALL_ITEMS);
}

export { slugifyAppName } from "@/lib/slug";

export const APP_SLUG_RE = /^[a-z][a-z0-9_]{0,59}$/;
export function isValidAppSlug(slug: string): boolean {
  return APP_SLUG_RE.test(slug);
}

// ── Curated lucide icon set (create dialog picker + name→component render) ─────
// Stored value = the kebab-case name; unknown/null renders the LayoutGrid fallback.
export const APP_ICONS: ReadonlyArray<{ name: string; icon: LucideIcon }> = [
  { name: "layout-grid", icon: LayoutGrid },
  { name: "briefcase", icon: Briefcase },
  { name: "users", icon: Users },
  { name: "heart-handshake", icon: HeartHandshake },
  { name: "ticket", icon: Ticket },
  { name: "graduation-cap", icon: GraduationCap },
  { name: "stethoscope", icon: Stethoscope },
  { name: "building-2", icon: Building2 },
  { name: "shopping-cart", icon: ShoppingCart },
  { name: "bar-chart-3", icon: BarChart3 },
  { name: "package", icon: Package },
  { name: "phone", icon: Phone },
  { name: "calendar-days", icon: CalendarDays },
];

export function appIconByName(name?: string | null): LucideIcon {
  return APP_ICONS.find((i) => i.name === name)?.icon ?? LayoutGrid;
}

// ── App color = EXISTING theme tone token key (no new colors — design-anti-slop) ──
// Full class strings (no interpolation) so Tailwind JIT picks them up; mirrors the
// tone→token mapping of components/ui/bits.tsx.
// `label` là tên NGƯỜI ĐỌC ĐƯỢC. Trước đây nút chọn màu có aria-label `Màu ${key}` → "Màu ac",
// "Màu cy", "Màu wn" — tên biến design-token đọc thẳng ra cho người dùng và cho trình đọc màn hình.
// Phát hiện khi đóng vai tay-mơ tạo app (2026-07-29).
export const APP_TONES: ReadonlyArray<{ key: string; label: string; chip: string; dot: string }> = [
  { key: "ac", label: "Xanh ngọc", chip: "bg-acd text-ac-hi", dot: "bg-ac" },
  { key: "cy", label: "Xanh dương", chip: "bg-cyd text-cy", dot: "bg-cy" },
  { key: "ok", label: "Xanh lá", chip: "bg-okd text-ok", dot: "bg-ok" },
  { key: "wn", label: "Vàng cam", chip: "bg-wnd text-wnf", dot: "bg-wn" },
  { key: "dg", label: "Đỏ", chip: "bg-dgd text-dgf", dot: "bg-dg" },
  { key: "neu", label: "Xám", chip: "bg-neud text-mut", dot: "bg-neu" },
];

export function appToneChip(color?: string | null): string {
  return APP_TONES.find((t) => t.key === color)?.chip ?? "bg-acd text-ac-hi";
}

// SLDS face: app tone key → SLDS custom icon colour (`slds-icon-custom-<N>`), the closest hue of the
// old tone — no hand-written colours. Feed it to <Icon category="custom" name={…}>.
const TONE_ICON: Record<string, string> = { ac: "27", cy: "9", ok: "6", wn: "16", dg: "13", neu: "92" };
export function appToneIcon(color?: string | null): string {
  return TONE_ICON[color ?? ""] ?? TONE_ICON.ac;
}

// ── localStorage read/write (fail-silent like lib/pins.ts) ─────────────────────

export function loadStored(): string | null {
  if (typeof window === "undefined") return null;
  try {
    return readForScope(storageKey, LEGACY_KEY);
  } catch {
    return null;
  }
}

export function saveStored(slug: string): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(storageKey(getStorageScope().key), slug);
  } catch {
    /* quota/private mode — active app is a convenience, fail silent */
  }
}

// ── Context ────────────────────────────────────────────────────────────────────

export interface ActiveAppState {
  /** All apps of the tenant, sortOrder order (empty until loaded / when tenant has none). */
  apps: AppSummary[];
  /** Resolved active slug — ALL_ITEMS ("*") or a live app name. */
  activeApp: string;
  /** Summary row of the active app; null when All Items. */
  activeMeta: AppSummary | null;
  /** ACL-filtered nav of the active app, in order; null when All Items or still loading. */
  activeNav: AppNavEntry[] | null;
  setActiveApp: (slug: string) => void;
  /** True while the apps list (or the active app's nav) is still loading. */
  isLoading: boolean;
  /** True when the active app's nav failed to load (queue #455): scope is unknown, not "unfiltered". */
  navError: boolean;
}

const Ctx = createContext<ActiveAppState | null>(null);

const APPS_STALE = 5 * 60 * 1000; // apps/nav change rarely (admin edits) — same spirit as metadata TTL

// specs/073 FR-032: in a Studio project `?app=` belongs to the project (StudioScopeOverride "create into"), not to this lens.
const inStudioProject = () => inProjectPath(window.location.pathname);

export function ActiveAppProvider({ children }: { children: ReactNode }) {
  const { user } = useAuth();
  // Boot: ?app=<slug> wins once, else localStorage. Read in the initializer (a pure read, null on the
  // server): hydration-safe because `stored` is never rendered itself, and the apps list is still
  // empty on the first client render (the query waits for the user) — so activeApp resolves to
  // ALL_ITEMS on both sides, exactly as the old null-then-effect boot did.
  const [stored, setStored] = useState<string | null>(() =>
    typeof window === "undefined"
      ? null
      : (inStudioProject() ? null : new URL(window.location.href).searchParams.get("app")) || loadStored(),
  );

  // …then the query param is persisted and cleaned from the URL (external sync only, no setState).
  useEffect(() => {
    if (inStudioProject()) return;
    const url = new URL(window.location.href);
    const fromQuery = url.searchParams.get("app");
    if (!fromQuery) return;
    saveStored(fromQuery);
    url.searchParams.delete("app");
    window.history.replaceState(null, "", url.toString());
  }, []);

  // specs/064 FR-014: the server filters apps + nav by the previewed ROLE, which the builder edits in
  // another tab — a preview tab re-reads both whenever it comes back to the foreground.
  const previewing = !!user?.preview;
  const appsQ = useQuery({
    queryKey: qk.apps(),
    queryFn: appsCatalog,
    staleTime: previewing ? 0 : APPS_STALE,
    refetchOnWindowFocus: previewing,
    enabled: !!user,
  });
  const apps = useMemo(() => appsQ.data ?? [], [appsQ.data]);

  // While apps are loading, resolution sees [] and lands on ALL_ITEMS — harmless because
  // consumers must render a skeleton while isLoading (no bogus nav fetch fires either).
  const activeApp = useMemo(() => resolveActiveApp(stored, apps), [stored, apps]);
  const activeMeta = useMemo(
    () => apps.find((a) => a.name === activeApp) ?? null,
    [apps, activeApp],
  );

  const navQ = useQuery({
    queryKey: qk.appNav(activeApp),
    queryFn: () => appNav(activeApp),
    staleTime: previewing ? 0 : APPS_STALE,
    refetchOnWindowFocus: previewing,
    enabled: !!user && activeApp !== ALL_ITEMS,
    // Opt out of the client-wide keepPreviousData (D17): one app's nav must never stand in for
    // another's. With it, a switch Ticketing → Clinic reported "Clinic, nav loaded" with TICKETING's
    // tables, and every Studio builder soft-filtered on that — Clinic's automation panel opened
    // Ticketing's SLA rule, Xoá/Cập nhật live, until Clinic's nav landed (queue #297). Without it
    // the switch reads as loading (activeNav null), which every consumer already handles.
    placeholderData: undefined,
  });

  const setActiveApp = useCallback((slug: string) => {
    saveStored(slug);
    setStored(slug);
  }, []);

  const value = useMemo<ActiveAppState>(() => ({
    apps,
    activeApp,
    activeMeta,
    activeNav: activeApp !== ALL_ITEMS ? navQ.data?.items ?? null : null,
    setActiveApp,
    isLoading: appsQ.isLoading || (activeApp !== ALL_ITEMS && navQ.isLoading),
    navError: activeApp !== ALL_ITEMS && navQ.isError && !navQ.data,
  }), [apps, activeApp, activeMeta, navQ.data, navQ.isLoading, navQ.isError, appsQ.isLoading, setActiveApp]);

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useActiveApp(): ActiveAppState {
  const ctx = useContext(Ctx);
  if (!ctx) throw new Error("useActiveApp must be used within <ActiveAppProvider>");
  return ctx;
}
