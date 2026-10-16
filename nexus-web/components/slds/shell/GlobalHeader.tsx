"use client";

// SLDS global header (blueprint `global-header/base`, SLDS 2.264.1) — top bar of the customer face.
//   header.slds-global-header_container > a.slds-assistive-text_focus ×2
//     + div.slds-global-header.slds-grid.slds-grid_align-spread
//       > div.slds-global-header__item (logo) · div.slds-global-header__item_search · div.slds-global-header__item > ul.slds-global-actions
// Mapping of the old Topbar/Sidebar capabilities (specs/038 contracts/shell.md):
//   search      → a faux input that opens the existing cmdk CommandPalette (⌘K / "/"); cmdk keeps
//                 the results, so the blueprint's object switcher + typeahead listbox are not copied.
//   favorites   → pins (lib/pins): the star toggles the current entity's pin, ▾ lists pinned entities.
//   help        → menu: shortcut sheet, plus /about for admins.
//   setup gear  → admins only: menu of the /admin/* pages (+ "Tạo trong Studio" for platform operators).
//   bell        → the showcase's notifications popover (global-header/base example: header outside the body,
//                 `.slds-global-header__notification` rows) with the latest notifications.
//   avatar      → account menu: name · role · tenant header, profile, preferences, sign out.
// Below md (#343): SLDS fixes the logo at 12.5rem and pads each item 1rem, which made the bar 440px at 390px —
// the logo box shrinks to its content (brand name truncates) and the items pad 0.5rem.
// Deviations: the container is `static z-auto` — in flow (AppShell stacks header, nav and main in a
// flex column instead of fixing the header and padding the page) and without SLDS's z-index 100,
// which still applies to a flex item and would float the header over the app's own overlays
// (shortcut sheet, ⌘K palette); no global-actions "+" button (nothing behind it); `.slds-global-header__logo` paints the Salesforce logo (CC BY-ND) as a background, so
// an inline style blanks it and the tenant logo / Nexus glyph is rendered inside instead.
import clsx from "clsx";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { Bell, ChevronDown, CircleHelp, Search, Settings, Sparkles, Star } from "lucide-react";
import { useMemo, useState, useSyncExternalStore, type CSSProperties, type ReactNode } from "react";
import { NexusMark } from "@/components/brand/NexusLogo";
import { useBranding } from "@/components/brand/BrandingBoot";
import { useFacePaths } from "@/components/shell/ModeSwitch";
import { CommandPalette, useCommandPalette } from "@/components/ui/CommandPalette";
import { TYPE_ICON, TYPE_META, useNotificationTray } from "@/components/ui/NotificationBell";
import { ShortcutSheet, openShortcutSheet } from "@/components/ui/ShortcutSheet";
import { toast } from "@/components/ui/Toaster";
import { tenantSlug, type ApiNotification } from "@/lib/api";
import { useAuth } from "@/lib/auth/AuthProvider";
import { t, tf } from "@/lib/i18n";
import { ADMIN_LINKS, OWNER_USERS_LINK } from "./adminLinks";
import { useEntities } from "@/lib/metadata/hooks";
import { usePermissions } from "@/lib/metadata/permissions";
import { useGuardedRouter } from "@/lib/navigation-blocker";
import { usePins } from "@/lib/pins";
import { useWorkspaceView } from "@/lib/studioContext";
import { relTime } from "@/lib/workflowRuns";
import { Button } from "../Button";
import { Icon } from "../Icon";
import { Menu, MenuDivider, MenuGroup, MenuItem } from "../Menu";
import { Popover } from "../Popover";
import { WorkspaceBanner } from "./WorkspaceBanner";

export interface HeaderLink {
  href: string;
  label: string;
}

export interface HeaderNotifications {
  unread: number;
  /** Latest notifications; fetched by the caller while the tray is open. */
  items: ApiNotification[];
  onOpenChange?: (open: boolean) => void;
  /** A row was clicked (mark it read; the row's link navigates). */
  onRead: (n: ApiNotification) => void;
  onMarkAll: () => void;
  markingAll?: boolean;
}

export interface GlobalHeaderProps {
  /** Tenant brand: logo image (else the Nexus glyph) + name. */
  brandName: string;
  logoUrl?: string;
  searchPlaceholder?: string;
  /** Opens the command palette. */
  onSearch: () => void;
  /** The pinnable entity of the current page; omitted ⇒ the star is disabled (blueprint "Favorites - Disabled"). */
  pinTarget?: { label: string; pinned: boolean; onToggle: () => void };
  favorites: HeaderLink[];
  onHelp: () => void;
  /** Admins get /about in the help menu and the Setup gear. */
  isAdmin: boolean;
  /** Setup gear entries (already filtered to what this person can open: an admin's list, or a customer owner's single "Người dùng"). */
  setupLinks: HeaderLink[];
  /** Studio bridge in the Setup menu; omit when the user cannot enter Studio. */
  studioHref?: string;
  notifications: HeaderNotifications;
  user: { name: string; subtitle: string } | null;
  /** specs/064 FR-017: this tab previews a role — the account menu shrinks to "Xem như <vai>" + "Thoát xem thử". */
  previewRole?: string;
  onNavigate: (href: string) => void;
  /** Sign out — or, while previewing, leave the preview (lib/api `logout` does that itself). */
  onLogout: () => void;
  /** Studio slots (specs/073 FR-052): project switcher beside the brand … */
  switcher?: ReactNode;
  /** … the assistant on/off toggle (stateful; icon-only below 768px) … */
  assistantToggle?: { pressed: boolean; onToggle: () => void };
  /** … no Setup gear in the Studio … */
  hideSetup?: boolean;
  /** … "Nexus" + "Studio": the suffix drops below 768px … */
  brandSuffix?: string;
  /** … the brand block is a link there ("Nexus Studio" → /studio) … */
  brandHref?: string;
  /** … the account menu lists these instead of profile/preferences … */
  accountLinks?: HeaderLink[];
  /** … (the Studio has no pinnable pages, so the same flag also drops the favorites star) … and ⌘K moves into the account menu below 768px (no room for six controls at 375px). */
  searchInMenu?: boolean;
  className?: string;
}

const ACTION_BUTTON =
  "slds-button slds-button_icon slds-button_icon slds-button_icon-container slds-button_icon-small slds-global-actions__item-action";

/** Two-letter initials of a person's name ("Nguyễn Thị Lan" → "NT"). */
export function initialsOf(name: string): string {
  const words = name.split(/\s+/).filter(Boolean);
  return (words.length ? words.slice(0, 2).map((w) => w[0]).join("") : "?").toUpperCase();
}

export function GlobalHeader({
  brandName,
  logoUrl,
  searchPlaceholder,
  onSearch,
  pinTarget,
  favorites,
  onHelp,
  isAdmin,
  setupLinks,
  studioHref,
  notifications,
  user,
  previewRole,
  onNavigate,
  onLogout,
  switcher,
  assistantToggle,
  hideSetup,
  brandSuffix,
  brandHref,
  accountLinks,
  searchInMenu,
  className,
}: GlobalHeaderProps) {
  const placeholder = searchPlaceholder ?? t("slds.globalHeader.searchPlaceholder");
  const brandInner = (
    <>
      {logoUrl ? (
        // eslint-disable-next-line @next/next/no-img-element -- tenant-supplied URL/data-URI, no next/image loader
        <img src={logoUrl} alt="" className="h-full w-auto max-w-[50%] object-contain" />
      ) : (
        // The Nexus mark on a tile: the SLDS icon container painted through its background hook
        // with the brand colour (which follows tenant branding).
        <span className="slds-icon_container" style={{ "--slds-c-icon-color-background": "var(--slds-g-color-brand-base-50)" } as CSSProperties}>
          <NexusMark size={32} className="slds-icon nx-lucide nx-lucide_tile" />
        </span>
      )}
      <span className={clsx("slds-text-heading_small slds-truncate", switcher && "max-md:hidden")} title={brandName}>
        {brandName}
      </span>
      {brandSuffix && <span className="slds-text-heading_small max-md:hidden">{brandSuffix}</span>}
    </>
  );
  const brandClass = "slds-global-header__logo flex min-w-0 items-center gap-2 max-md:w-auto";
  return (
    <header className={clsx("slds-global-header_container static z-auto", className)}>
      <a href="#nx-global-nav" className="slds-assistive-text slds-assistive-text_focus">
        {t("slds.globalHeader.skipNav")}
      </a>
      <a href="#nx-main" className="slds-assistive-text slds-assistive-text_focus">
        {t("slds.globalHeader.skipMain")}
      </a>
      <div className="slds-global-header slds-grid slds-grid_align-spread">
        <div className={clsx("slds-global-header__item min-w-0 max-md:px-2", switcher && "flex items-center gap-2")}>
          {/* SLDS paints its own logo asset (CC BY-ND, campaign D3) as this box's background. */}
          {brandHref ? (
            <Link href={brandHref} className={brandClass} style={{ backgroundImage: "none" }}>
              {brandInner}
            </Link>
          ) : (
            <div className={brandClass} style={{ backgroundImage: "none" }}>
              {brandInner}
            </div>
          )}
          {switcher && <div className="min-w-0 shrink overflow-hidden">{switcher}</div>}
        </div>

        <div className="slds-global-header__item slds-global-header__item_search max-lg:min-w-0 max-md:hidden">
          <div className="slds-form-element">
            <div className="slds-form-element__control">
              <div className="slds-combobox_container">
                <div className="slds-combobox slds-dropdown-trigger slds-dropdown-trigger_click">
                  <div
                    className="slds-combobox__form-element slds-input-has-icon slds-input-has-icon_left slds-global-search__form-element"
                    role="none"
                  >
                    <span className="slds-icon_container slds-icon-utility-search slds-input__icon slds-input__icon_left">
                      <Search className="slds-icon slds-icon_xx-small slds-icon-text-default nx-lucide" aria-hidden="true" />
                    </span>
                    {/* A button, not an input: typing happens in the cmdk dialog it opens. */}
                    <button type="button" className="slds-input slds-combobox__input slds-text-align_left" aria-haspopup="dialog" onClick={onSearch}>
                      <span className="slds-truncate slds-text-color_weak block">{placeholder}</span>
                    </button>
                  </div>
                </div>
              </div>
            </div>
          </div>
        </div>

        <div className="slds-global-header__item shrink-0 max-md:px-2">
          <ul className="slds-global-actions">
            {assistantToggle && (
              <li className="slds-global-actions__item">
                <Button
                  variant="neutral"
                  stateful
                  selected={assistantToggle.pressed}
                  iconLeft={Sparkles}
                  className="min-h-6 min-w-6 max-md:px-1"
                  aria-label={t("slds.globalHeader.assistant")}
                  onClick={assistantToggle.onToggle}
                >
                  <span className="max-md:sr-only">{t("slds.globalHeader.assistant")}</span>
                </Button>
              </li>
            )}
            {!searchInMenu && (
              <li className="slds-global-actions__item max-md:hidden">
                <Favorites pinTarget={pinTarget} favorites={favorites} onNavigate={onNavigate} />
              </li>
            )}
            {!searchInMenu && (
              <li className="slds-global-actions__item md:hidden">
                <div className="slds-dropdown-trigger slds-dropdown-trigger_click">
                  <button type="button" className={ACTION_BUTTON} aria-haspopup="dialog" title={placeholder} onClick={onSearch}>
                    <Search className="slds-button__icon slds-global-header__icon nx-lucide" aria-hidden="true" />
                    <span className="slds-assistive-text">{t("slds.globalHeader.search")}</span>
                  </button>
                </div>
              </li>
            )}
            <li className="slds-global-actions__item">
              <Menu
                align="right"
                trigger={
                  <button type="button" className={clsx(ACTION_BUTTON, "slds-global-actions__help")} title={t("slds.globalHeader.help")}>
                    <CircleHelp className="slds-button__icon slds-global-header__icon nx-lucide" aria-hidden="true" />
                    <span className="slds-assistive-text">{t("slds.globalHeader.help")}</span>
                  </button>
                }
              >
                <MenuItem label={t("slds.globalHeader.shortcuts")} onSelect={onHelp} />
                {isAdmin && <MenuItem label={t("slds.globalHeader.about")} onSelect={() => onNavigate("/about")} />}
              </Menu>
            </li>
            {/* The gear opens for an admin AND for a customer owner (specs/065), whose menu is the one
                "Người dùng" link the connected header hands in: no links, no gear. */}
            {(isAdmin || setupLinks.length > 0) && !hideSetup && (
              <li className="slds-global-actions__item">
                <Menu
                  align="right"
                  trigger={
                    <button type="button" className={clsx(ACTION_BUTTON, "slds-global-actions__setup")} title={t("slds.globalHeader.setup")}>
                      <Settings className="slds-button__icon slds-global-header__icon nx-lucide" aria-hidden="true" />
                      <span className="slds-assistive-text">{t("slds.globalHeader.setup")}</span>
                    </button>
                  }
                >
                  <MenuGroup label={t("slds.globalHeader.admin")}>
                    {setupLinks.map((l) => (
                      <MenuItem key={l.href} label={l.label} onSelect={() => onNavigate(l.href)} />
                    ))}
                  </MenuGroup>
                  {studioHref && (
                    <>
                      <MenuDivider />
                      <MenuItem label={t("slds.globalHeader.studio")} onSelect={() => onNavigate(studioHref)} />
                    </>
                  )}
                </Menu>
              </li>
            )}
            <li className="slds-global-actions__item">
              <NotificationTray {...notifications} />
            </li>
            <li className="slds-global-actions__item">
              <Menu
                align="right"
                trigger={
                  <button type="button" className="slds-button slds-global-actions__avatar slds-global-actions__item-action" title={user?.name}>
                    <span className="slds-avatar slds-avatar_circle slds-avatar_medium" aria-hidden="true">
                      <abbr className="slds-avatar__initials slds-icon-standard-user" title={user?.name}>
                        {initialsOf(user?.name ?? "")}
                      </abbr>
                    </span>
                    <span className="slds-assistive-text">{t("slds.globalHeader.account")}</span>
                  </button>
                }
              >
                {previewRole !== undefined ? (
                  <>
                    <li className="slds-dropdown__header" role="presentation">
                      <span className="slds-truncate" title={tf("preview.menu.viewingAs", { role: previewRole })}>
                        {tf("preview.menu.viewingAs", { role: previewRole })}
                      </span>
                    </li>
                    <MenuItem label={t("preview.menu.exit")} onSelect={onLogout} />
                  </>
                ) : (
                  <>
                    {user && (
                      <>
                        <li className="slds-dropdown__header slds-p-bottom_none" role="presentation">
                          <span className="slds-truncate" title={user.name}>
                            {user.name}
                          </span>
                        </li>
                        {/* Own row: the header row is bold, the role · tenant line is not. */}
                        <li
                          className="slds-p-horizontal_small slds-p-bottom_x-small slds-text-body_small slds-text-color_weak slds-truncate"
                          role="presentation"
                          title={user.subtitle}
                        >
                          {user.subtitle}
                        </li>
                      </>
                    )}
                    {searchInMenu && (
                      <MenuItem className="md:hidden" icon={Search} label={t("slds.globalHeader.searchMenu")} onSelect={onSearch} />
                    )}
                    {(accountLinks ?? [
                      { href: "/profile", label: t("slds.globalHeader.profile") },
                      { href: "/settings", label: t("slds.globalHeader.settings") },
                    ]).map((l) => (
                      <MenuItem key={l.href} label={l.label} onSelect={() => onNavigate(l.href)} />
                    ))}
                    <MenuDivider />
                    <MenuItem label={t("slds.globalHeader.logout")} onSelect={onLogout} />
                  </>
                )}
              </Menu>
            </li>
          </ul>
        </div>
      </div>
    </header>
  );
}

/**
 * Favorites = pins. The blueprint puts the dropdown trigger class on the outer div and the two
 * buttons in one button-group: Menu's `leading` slot puts the ☆ before the ▾ trigger in that group.
 * The pinned (`slds-is-selected`) star needs the app/globals.css glue rule
 * `.slds-global-actions__favorites-action.slds-is-selected .nx-lucide`: SLDS fills its sprite star
 * white, but `.slds-global-actions__item .slds-button_icon` greys `color` and `.nx-lucide` forces
 * `fill:none`, so the Lucide star would stay a grey outline on the dark-blue button.
 */
function Favorites({
  pinTarget,
  favorites,
  onNavigate,
}: Pick<GlobalHeaderProps, "pinTarget" | "favorites" | "onNavigate">) {
  const pinLabel = pinTarget
    ? t(pinTarget.pinned ? "slds.globalHeader.unpin" : "slds.globalHeader.pin").replace("{label}", pinTarget.label)
    : t("slds.globalHeader.pinDisabled");
  const moreLabel = t("slds.globalHeader.favorites");
  return (
    <Menu
      className="slds-global-actions__favorites"
      align="right"
      length={10}
      leading={
        <button
          type="button"
          className={clsx(
            "slds-button slds-button_icon slds-global-actions__favorites-action slds-button_icon slds-button_icon-border slds-button_icon-small",
            pinTarget?.pinned && "slds-is-selected",
            !pinTarget && "slds-is-disabled",
          )}
          disabled={!pinTarget}
          aria-pressed={pinTarget?.pinned ?? false}
          title={pinLabel}
          onClick={pinTarget?.onToggle}
        >
          <Star className="slds-button__icon nx-lucide" aria-hidden="true" />
          <span className="slds-assistive-text">{pinLabel}</span>
        </button>
      }
      trigger={
        <button
          type="button"
          className="slds-button slds-button_icon slds-global-actions__favorites-more slds-button_icon slds-button_icon-border slds-button_icon-small"
          title={moreLabel}
        >
          <ChevronDown className="slds-button__icon slds-button__icon_small nx-lucide" aria-hidden="true" />
          <span className="slds-assistive-text">{moreLabel}</span>
        </button>
      }
    >
      {favorites.length > 0 ? (
        favorites.map((f) => <MenuItem key={f.href} label={f.label} onSelect={() => onNavigate(f.href)} />)
      ) : (
        <MenuItem label={t("slds.globalHeader.noFavorites")} disabled />
      )}
    </Menu>
  );
}

/**
 * Bell + badge + tray. The tray is the global-header/base showcase popover
 * (`slds-popover_large slds-nubbin_top-right`, headerTitle outside the body, body `slds-p-around_none`,
 * `ul` of `.slds-global-header__notification` rows); the footer (view all · mark all read) is the
 * popovers "With Footer" pattern. Unread rows carry the showcase's `abbr ●` plus assistive text, so
 * unread is not colour alone (WCAG 1.4.1).
 */
function NotificationTray({ unread, items, onOpenChange, onRead, onMarkAll, markingAll }: HeaderNotifications) {
  const [open, setOpen] = useState(false);
  const change = (next: boolean) => {
    setOpen(next);
    onOpenChange?.(next);
  };
  const unreadLabel = t("slds.globalHeader.unread");
  const bellLabel =
    unread > 0 ? t("slds.globalHeader.newNotifications").replace("{n}", String(unread)) : t("slds.globalHeader.noNotifications");
  return (
    <div className="slds-dropdown-trigger slds-dropdown-trigger_click">
      <Popover
        open={open}
        onOpenChange={change}
        side="bottom"
        align="end"
        size="large"
        scrollable
        headerOutside
        bodyClassName="slds-p-around_none"
        title={t("slds.globalHeader.notifications")}
        trigger={
          <button
            type="button"
            className={clsx(ACTION_BUTTON, "slds-global-actions__notifications", unread > 0 && "slds-incoming-notification")}
            title={bellLabel}
            aria-live="assertive"
            aria-atomic="true"
          >
            <Bell className="slds-button__icon slds-global-header__icon nx-lucide" aria-hidden="true" />
            <span className="slds-assistive-text">{bellLabel}</span>
          </button>
        }
        footer={
          <div className="slds-grid slds-grid_vertical-align-center">
            <Link href="/notifications" onClick={() => change(false)}>
              {t("slds.globalHeader.viewAllNotifications")}
            </Link>
            <button type="button" className="slds-button slds-col_bump-left" disabled={unread === 0 || markingAll} onClick={onMarkAll}>
              {t("slds.globalHeader.markAllRead")}
            </button>
          </div>
        }
      >
        {items.length === 0 ? (
          <p className="slds-p-around_small slds-text-color_weak">{t("slds.globalHeader.noNotifications")}</p>
        ) : (
          <ul>
            {items.slice(0, 8).map((n) => {
              const type = n.type ?? "system";
              const meta = TYPE_META[type] ?? TYPE_META.system;
              return (
                <li key={n.id} className={clsx("slds-global-header__notification", !n.isRead && "slds-global-header__notification_unread")}>
                  <div className="slds-media slds-has-flexi-truncate slds-p-around_x-small">
                    <div className="slds-media__figure">
                      <Icon icon={TYPE_ICON[type] ?? TYPE_ICON.system} category="standard" name="custom-notification" size="small" />
                    </div>
                    <div className="slds-media__body">
                      <div className="slds-grid slds-grid_align-spread">
                        <Link
                          href={n.href ?? "/notifications"}
                          className="slds-text-link_reset slds-has-flexi-truncate"
                          onClick={() => {
                            onRead(n);
                            change(false);
                          }}
                        >
                          {n.title && (
                            <h3 className="slds-truncate" title={n.title}>
                              <strong>{n.title}</strong>
                            </h3>
                          )}
                          <p className="slds-truncate" title={n.message}>
                            {n.message}
                          </p>
                          <p className="slds-m-top_x-small slds-text-color_weak">
                            {meta.label} · {relTime(n.createdAt)}
                            {!n.isRead && (
                              <>
                                {/* title is not read out reliably, hence the assistive text. */}
                                <abbr className="slds-text-link slds-m-horizontal_xxx-small" title={unreadLabel} aria-hidden="true">
                                  ●
                                </abbr>
                                <span className="slds-assistive-text">{unreadLabel}</span>
                              </>
                            )}
                          </p>
                        </Link>
                      </div>
                    </div>
                  </div>
                </li>
              );
            })}
          </ul>
        )}
      </Popover>
      <span aria-hidden="true" className={clsx("slds-notification-badge", unread > 0 && "slds-show-notification")}>
        {unread > 9 ? "9+" : unread}
      </span>
    </div>
  );
}

// ── Connected header (AppShell) ─────────────────────────────────────────────────────────────────

const noSubscribe = () => () => {};

/**
 * GlobalHeader wired to the session: auth, tenant branding, pins, notifications, ⌘K palette and
 * the shortcut sheet (both mounted once, here — the customer face has no Topbar any more).
 * `studio`: the Studio face keeps Nexus's own name + glyph, never the tenant's (contracts/shell.md
 * "Studio KHÔNG nhuộm màu tenant"; the old StudioSidebar hard-coded the Nexus logo).
 */
export function ShellGlobalHeader({
  searchPlaceholder,
  className,
  studio = false,
  switcher,
  assistantToggle,
  hideSetup,
  brandSuffix,
}: {
  searchPlaceholder?: string;
  className?: string;
  studio?: boolean;
} & Pick<GlobalHeaderProps, "switcher" | "assistantToggle" | "hideSetup" | "brandSuffix">) {
  const { user, logout } = useAuth();
  const branding = useBranding();
  const router = useGuardedRouter();
  const pathname = usePathname();
  const palette = useCommandPalette();
  const { pins, toggle } = usePins();
  const { data: entities = [] } = useEntities();
  const { can } = usePermissions();
  const [trayOpen, setTrayOpen] = useState(false);
  const tray = useNotificationTray(trayOpen);
  const { builderHref } = useFacePaths("app");
  // The label "Mở app của xưởng Nexus" is false in a build session: /home then opens the CUSTOMER's app (same tab, session token).
  const inSession = useWorkspaceView() !== null;
  // localStorage read; "" on the server so hydration matches.
  const slug = useSyncExternalStore(noSubscribe, tenantSlug, () => "");

  const isAdmin = !!user?.isAdmin;
  // Studio sits behind RequireOperator: only platform operators get the bridge.
  const isOperator = !!user?.isPlatformOperator;

  const visible = useMemo(() => entities.filter((e) => can("view", e.name)), [entities, can]);
  const first = pathname.split("/").filter(Boolean)[0] ?? "";
  const current = visible.find((e) => e.name === first);
  const favorites = pins.flatMap((p) => {
    const e = visible.find((x) => x.name === p);
    return e ? [{ href: `/${e.name}`, label: e.label }] : [];
  });

  async function handleLogout() {
    try {
      await logout();
      // In a preview tab logout() already navigated back to the Studio page the preview started from.
      if (!user?.preview) window.location.href = "/login";
    } catch {
      toast.error(t("slds.globalHeader.logoutFailed"));
    }
  }

  const name = user?.fullName ?? user?.email ?? "";
  const role = user?.isAdmin ? t("slds.globalHeader.roleAdmin") : (user?.roleName ?? t("slds.globalHeader.roleUser"));
  return (
    <>
      {/* specs/062 L2: renders only while THIS tab builds inside a customer's workspace; above the header
          so it is never missed in the customer app. The Studio draws its own right UNDER the header
          (StudioShell → StudioSessionBanner, specs/073 FR-082), so it is not stacked twice here. */}
      {!studio && <WorkspaceBanner />}
      <CommandPalette open={palette.open} onClose={() => palette.setOpen(false)} />
      <ShortcutSheet />
      <GlobalHeader
        className={className}
        brandName={(!studio && branding.appName) || "Nexus"}
        logoUrl={studio ? undefined : branding.logoUrl}
        searchPlaceholder={searchPlaceholder}
        switcher={switcher}
        assistantToggle={assistantToggle}
        hideSetup={hideSetup}
        brandSuffix={brandSuffix ?? (studio ? t("studio.shell.brandSuffix") : undefined)}
        brandHref={studio ? "/studio" : undefined}
        accountLinks={
          studio
            ? [
                // ponytail: "Tài khoản của tôi" (/studio/tai-khoan) returns with the page in kh4 (T276); no link to a missing page.
                { href: "/home", label: t(inSession ? "studio.project.openApp" : "studio.project.openHomeApp") },
              ]
            : undefined
        }
        searchInMenu={studio}
        onSearch={() => palette.setOpen(true)}
        pinTarget={
          current && { label: current.label, pinned: pins.includes(current.name), onToggle: () => toggle(current.name) }
        }
        favorites={favorites}
        onHelp={openShortcutSheet}
        isAdmin={isAdmin}
        setupLinks={
          isAdmin
            ? ADMIN_LINKS.filter((l) => !l.operatorOnly || isOperator).map((l) => ({ href: l.href, label: t(l.label) }))
            : user?.canManageUsers
              ? [{ href: OWNER_USERS_LINK.href, label: t(OWNER_USERS_LINK.label) }]
              : []
        }
        studioHref={isAdmin && isOperator ? builderHref : undefined}
        notifications={{
          unread: tray.unreadCount,
          items: tray.notifications,
          onOpenChange: setTrayOpen,
          onRead: (n) => {
            if (!n.isRead) tray.markRead.mutate(n.id);
          },
          onMarkAll: () => tray.markAll.mutate(),
          markingAll: tray.markAll.isPending,
        }}
        user={user ? { name, subtitle: slug ? `${role} · ${slug}` : role } : null}
        previewRole={user?.preview ? user.preview.roleName : undefined}
        onNavigate={(href) => router.push(href)}
        onLogout={handleLogout}
      />
    </>
  );
}
