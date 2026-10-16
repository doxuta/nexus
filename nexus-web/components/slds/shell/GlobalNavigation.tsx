"use client";

// SLDS global navigation (blueprint `global-navigation/navigation-bar`, SLDS 2.264.1).
//   div.slds-context-bar
//     > div.slds-context-bar__primary > div.slds-context-bar__item.slds-context-bar__dropdown-trigger….slds-no-hover
//         > div.slds-context-bar__icon-action > (app launcher waffle)  +  span.slds-context-bar__label-action.slds-context-bar__app-name
//     > nav.slds-context-bar__secondary[role=navigation] > ul.slds-grid
//         > li.slds-context-bar__item(.slds-is-active) > a.slds-context-bar__label-action (+ "Trang hiện tại:" assistive text)
// Overflow: tabs that do not fit go into a "Thêm" item (the "More" item of the navigation-tab-bar
// blueprint, on components/slds/Menu); an overflowed active tab takes the last slot in the bar.
// Widths come from a hidden copy of the whole row (ResizeObserver), so long Vietnamese labels are
// handled without a guessed tab count. Below 768px the row gives way to one menu button with every tab.
// Deviation: the "Thêm" li holds Menu's own div.slds-dropdown-trigger wrapper instead of carrying
// the dropdown-trigger classes itself.
import clsx from "clsx";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { useQuery } from "@tanstack/react-query";
import { ChevronDown, Menu as MenuGlyph } from "lucide-react";
import { useMemo, useRef, type ReactNode } from "react";
import { getNotificationUnreadCount, listPendingApprovals } from "@/lib/api";
import { ALL_ITEMS, useActiveApp } from "@/lib/apps";
import { useAuth } from "@/lib/auth/AuthProvider";
import { t } from "@/lib/i18n";
import { useEntities } from "@/lib/metadata/hooks";
import { usePermissions } from "@/lib/metadata/permissions";
import { useGuardedRouter } from "@/lib/navigation-blocker";
import { usePins } from "@/lib/pins";
import { qk } from "@/lib/query/keys";
import { Menu, MenuItem } from "../Menu";
import { useFit } from "../useFit";
import { ShellAppLauncher } from "./AppLauncher";

export interface NavTab {
  href: string;
  label: string;
  /** Badge number; shown only when > 0. */
  count?: number;
  /** Assistive text read with the badge, e.g. "3 yêu cầu đang chờ duyệt". */
  countLabel?: string;
}

export interface GlobalNavigationProps {
  /** Current app (or lens) name in the primary zone. */
  appName: string;
  items: NavTab[];
  /** The tab whose href is the longest prefix of this path is active. */
  pathname: string;
  /** App launcher trigger (waffle), rendered before the app name. */
  launcher?: ReactNode;
  /** Upper bound on tabs kept in the bar (on top of what fits). */
  maxVisible?: number;
  /** Navigation for menu entries (overflow / mobile); tabs themselves are links. */
  onNavigate: (href: string) => void;
  id?: string;
  className?: string;
}

/** Index of the tab that owns `pathname` (longest matching href), -1 when none. */
export function activeTabIndex(items: NavTab[], pathname: string): number {
  let best = -1;
  items.forEach((item, i) => {
    const hit = pathname === item.href || pathname.startsWith(`${item.href}/`);
    if (hit && (best < 0 || item.href.length > items[best].href.length)) best = i;
  });
  return best;
}

/** Tabs kept in the bar + overflow for `fit` slots; an overflowed active tab takes the last slot. */
export function splitTabs(items: NavTab[], active: number, fit: number): [NavTab[], NavTab[]] {
  if (fit >= items.length) return [items, []];
  if (active < fit || fit === 0) return [items.slice(0, fit), items.slice(fit)];
  return [[...items.slice(0, fit - 1), items[active]], items.filter((_, i) => i >= fit - 1 && i !== active)];
}

const badgeText = (n: number) => (n > 99 ? "99+" : String(n));
const menuLabel = (item: NavTab) => (item.count ? `${item.label} (${badgeText(item.count)})` : item.label);

function Count({ item }: { item: NavTab }) {
  if (!item.count) return null;
  return (
    <>
      <span className="slds-badge slds-m-left_x-small" aria-hidden="true">
        {badgeText(item.count)}
      </span>
      {item.countLabel && <span className="slds-assistive-text">{item.countLabel}</span>}
    </>
  );
}

export function GlobalNavigation({ appName, items, pathname, launcher, maxVisible, onNavigate, id, className }: GlobalNavigationProps) {
  const navRef = useRef<HTMLElement>(null);
  const ghostRef = useRef<HTMLUListElement>(null);
  const fit = Math.min(useFit(navRef, ghostRef), maxVisible ?? Infinity);
  const active = activeTabIndex(items, pathname);
  const [shown, overflow] = splitTabs(items, active, fit);
  const more = t("slds.globalNav.more");
  const moreAssistive = t("slds.globalNav.moreAssistive");

  return (
    <div id={id} className={clsx("slds-context-bar", className)}>
      <div className="slds-context-bar__primary">
        <div className="slds-context-bar__item slds-context-bar__dropdown-trigger slds-dropdown-trigger slds-dropdown-trigger_click slds-no-hover">
          {launcher && <div className="slds-context-bar__icon-action">{launcher}</div>}
          <span className="slds-context-bar__label-action slds-context-bar__app-name">
            <span className="slds-truncate" title={appName}>
              {appName}
            </span>
          </span>
        </div>
      </div>
      <nav ref={navRef} className="slds-context-bar__secondary max-md:hidden" role="navigation" aria-label={t("slds.globalNav.label")}>
        <ul className="slds-grid">
          {shown.map((item) => {
            const isActive = item === items[active];
            return (
              <li key={item.href} className={clsx("slds-context-bar__item", isActive && "slds-is-active")}>
                <Link href={item.href} className="slds-context-bar__label-action" title={item.label}>
                  {isActive && <span className="slds-assistive-text">{t("slds.globalNav.currentPage")}</span>}
                  <span className="slds-truncate" title={item.label}>
                    {item.label}
                  </span>
                  <Count item={item} />
                </Link>
              </li>
            );
          })}
          {overflow.length > 0 && (
            <li className="slds-context-bar__item slds-context-bar__dropdown-trigger">
              <Menu
                className="flex"
                align="right"
                length={10}
                trigger={
                  <button type="button" className="slds-button slds-context-bar__label-action" title={`${more} ${moreAssistive}`}>
                    <span className="slds-truncate" title={more}>
                      {more} <span className="slds-assistive-text">{moreAssistive}</span>
                    </span>
                    <ChevronDown className="slds-button__icon slds-button__icon_small slds-button__icon_right nx-lucide" aria-hidden="true" />
                  </button>
                }
              >
                {overflow.map((item) => (
                  <MenuItem key={item.href} label={menuLabel(item)} onSelect={() => onNavigate(item.href)} />
                ))}
              </Menu>
            </li>
          )}
        </ul>
        {/* Measuring copy of the whole row (never seen, never focused). */}
        <div aria-hidden="true" className="pointer-events-none invisible absolute h-0 w-0 overflow-hidden">
          <ul ref={ghostRef} className="slds-grid w-max">
            {items.map((item) => (
              <li key={item.href} className="slds-context-bar__item">
                <span className="slds-context-bar__label-action">
                  <span className="slds-truncate">{item.label}</span>
                  <Count item={item} />
                </span>
              </li>
            ))}
            <li className="slds-context-bar__item">
              <span className="slds-context-bar__label-action">
                <span className="slds-truncate">{more}</span>
                <ChevronDown className="slds-button__icon slds-button__icon_small slds-button__icon_right nx-lucide" />
              </span>
            </li>
          </ul>
        </div>
      </nav>
      <div className="slds-context-bar__tertiary ml-auto md:hidden">
        <Menu
          className="flex items-center"
          align="right"
          length={10}
          trigger={
            <button type="button" className="slds-button slds-button_icon slds-context-bar__button slds-p-horizontal_small" title={t("slds.globalNav.menu")}>
              <MenuGlyph className="slds-button__icon nx-lucide" aria-hidden="true" />
              <span className="slds-assistive-text">{t("slds.globalNav.menu")}</span>
            </button>
          }
        >
          {items.map((item) => (
            <MenuItem key={item.href} label={menuLabel(item)} onSelect={() => onNavigate(item.href)} />
          ))}
        </Menu>
      </div>
    </div>
  );
}

// ── Connected navigation (AppShell) ─────────────────────────────────────────────────────────────

/**
 * Tabs of the customer face: Trang chủ · (Bảng điều khiển · Báo cáo in the All Items lens) ·
 * Phê duyệt (pending badge) · Thông báo (unread badge), then the user's pinned entities, then the
 * active app's nav in the admin's order — or, in the All Items lens, every entity the user can view.
 */
export function useShellNav(): { appName: string; items: NavTab[] } {
  const { user } = useAuth();
  const { activeApp, activeMeta, activeNav } = useActiveApp();
  const { data: entities = [] } = useEntities();
  const { can } = usePermissions();
  const { pins } = usePins();
  // The nav owns the 60s pending-approvals poll the old sidebar ran; ApprovalsInbox/Home share the key.
  const { data: pending = [] } = useQuery({
    queryKey: qk.pendingApprovals(),
    queryFn: listPendingApprovals,
    staleTime: 60_000,
    refetchInterval: 60_000,
    enabled: !!user,
  });
  // Same key as the header bell, which runs the 30s poll — this only reads the cache.
  const { data: unread = 0 } = useQuery({
    queryKey: qk.notificationCount(),
    queryFn: getNotificationUnreadCount,
    staleTime: 30_000,
    enabled: !!user,
  });

  const inApp = activeApp !== ALL_ITEMS;
  const pendingCount = pending.length;
  return useMemo(() => {
    const fixed: NavTab[] = [
      { href: "/home", label: t("shell.nav.home") },
      // App-mode diet (Founder 2026-07-10): tenant-wide catalogs stay in All Items, the launcher and ⌘K.
      ...(inApp
        ? []
        : [
            { href: "/dashboards", label: t("shell.nav.dashboards") },
            { href: "/reports", label: t("shell.nav.reports") },
          ]),
      {
        href: "/approvals",
        label: t("shell.nav.approvals"),
        count: pendingCount,
        countLabel: t("slds.globalNav.pending").replace("{n}", String(pendingCount)),
      },
      {
        href: "/notifications",
        label: t("slds.globalNav.notifications"),
        count: unread,
        countLabel: t("slds.globalNav.unread").replace("{n}", String(unread)),
      },
    ];
    const viewable = entities.filter((e) => can("view", e.name));
    // Pins survive in localStorage after access is revoked: only entities still viewable become tabs.
    const pinned = pins.flatMap((p) => {
      const e = viewable.find((x) => x.name === p);
      return e ? [{ href: `/${e.name}`, label: e.label }] : [];
    });
    const taken = new Set([...fixed, ...pinned].map((i) => i.href));
    const rest = (inApp ? (activeNav ?? []) : viewable.map((e) => ({ href: `/${e.name}`, label: e.label })))
      .map(({ href, label }) => ({ href, label }))
      .filter((i) => !taken.has(i.href));
    return {
      appName: inApp ? (activeMeta?.label ?? "") : t("slds.globalNav.allItems"),
      items: [...fixed, ...pinned, ...rest],
    };
  }, [inApp, activeMeta, activeNav, entities, can, pins, pendingCount, unread]);
}

/** GlobalNavigation on the session: active app, pins, badges, launcher, current route. */
export function ShellGlobalNavigation({ id, className }: { id?: string; className?: string }) {
  const { appName, items } = useShellNav();
  const pathname = usePathname();
  const router = useGuardedRouter();
  return (
    <GlobalNavigation
      id={id}
      className={className}
      appName={appName}
      items={items}
      pathname={pathname}
      launcher={<ShellAppLauncher />}
      onNavigate={(href) => router.push(href)}
    />
  );
}
