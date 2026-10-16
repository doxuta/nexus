"use client";

// SLDS app launcher (blueprint `app-launcher/base`, SLDS 2.264.1) on @radix-ui/react-dialog.
//   button.slds-icon-waffle_container.slds-context-bar__button (waffle, in GlobalNavigation's primary zone)
//   section.slds-modal.slds-fade-in-open.slds-modal_large.slds-app-launcher[role=dialog]
//     > div.slds-modal__container > close
//       · div.slds-modal__header.slds-grid.slds-grid_align-spread.slds-grid_vertical-align-center (h2 · search · action)
//       · div.slds-modal__content.slds-app-launcher__content > div.slds-section (app tiles) · hr · div.slds-section (all items)
//   + div.slds-backdrop.slds-backdrop_open
// Radix owns the focus trap, Esc and focus return to the waffle. Modal.tsx is not reused: its
// header is h1.slds-modal__title, the launcher's is an h2 + search + button grid.
// Nexus mapping (spec 010 app container): a tile is one app of the tenant; choosing it sets the
// active app (a lens — URLs stay flat) and opens its home. The extra "Tất cả dữ liệu" tile is the
// All Items lens → /entities. "Tất cả mục" lists dashboards, reports and every entity the user can view.
// ponytail: no drag-to-reorder / Reorder button / "More" description tooltip — app order is set in
// Studio and AppSummary has no description. Add them with a per-user order if one ever exists.
import * as Dialog from "@radix-ui/react-dialog";
import clsx from "clsx";
import Link from "next/link";
import { ChevronDown, Database, Search, X } from "lucide-react";
import { useId, useMemo, useRef, useState, type ReactNode } from "react";
import { CreateAppDialog } from "@/components/apps/CreateAppDialog";
import { useAuth } from "@/lib/auth/AuthProvider";
import { studioHref } from "@/lib/studioRoutes";
import { activeProjectSlug } from "@/lib/studioContext";
import { ALL_ITEMS, appIconByName, appToneIcon, useActiveApp } from "@/lib/apps";
import { t } from "@/lib/i18n";
import { useEntities } from "@/lib/metadata/hooks";
import { usePermissions } from "@/lib/metadata/permissions";
import { Icon } from "../Icon";

export interface LauncherApp {
  name: string;
  label: string;
  /** Lucide icon name from lib/apps APP_ICONS. */
  icon?: string | null;
  /** App tone key ("ac" | "cy" | "ok" | "wn" | "dg" | "neu"). */
  color?: string | null;
  description?: string;
}

export interface LauncherItem {
  href: string;
  label: string;
}

export interface AppLauncherProps {
  apps: LauncherApp[];
  /** Active app slug, or ALL_ITEMS. */
  activeApp?: string;
  items: LauncherItem[];
  /** An app tile (its slug) or the "Tất cả dữ liệu" tile (ALL_ITEMS) was chosen. */
  onSelectApp: (name: string) => void;
  /** Header button slot (the blueprint's "App Exchange"). */
  actions?: ReactNode;
  open?: boolean;
  defaultOpen?: boolean;
  onOpenChange?: (open: boolean) => void;
}

/** Case- and diacritic-insensitive match ("khach" finds "Khách hàng"). */
function fold(s: string): string {
  return s.normalize("NFD").replace(/[\u0300-\u036f]/g, "").replace(/đ/g, "d").replace(/Đ/g, "D").toLowerCase();
}

export function AppLauncher({ apps, activeApp, items, onSelectApp, actions, open, defaultOpen, onOpenChange }: AppLauncherProps) {
  const [innerOpen, setInnerOpen] = useState(defaultOpen ?? false);
  const [query, setQuery] = useState("");
  const isOpen = open ?? innerOpen;
  const setOpen = (next: boolean) => {
    setInnerOpen(next);
    onOpenChange?.(next);
    if (!next) setQuery("");
  };
  const id = useId();
  const searchRef = useRef<HTMLInputElement>(null);
  const q = fold(query.trim());

  const tiles: (LauncherApp & { href: string })[] = [
    ...apps.map((a) => ({ ...a, href: "/home" })),
    { name: ALL_ITEMS, label: t("slds.appLauncher.allData"), description: t("slds.appLauncher.allDataDesc"), href: "/entities" },
  ].filter((a) => !q || fold(a.label).includes(q));
  const links = items.filter((i) => !q || fold(i.label).includes(q));
  const close = t("slds.modal.close");

  return (
    <Dialog.Root open={isOpen} onOpenChange={setOpen}>
      <Dialog.Trigger asChild>
        <button type="button" className="slds-button slds-icon-waffle_container slds-context-bar__button" title={t("slds.appLauncher.open")}>
          <span className="slds-icon-waffle">
            <span className="slds-r1" />
            <span className="slds-r2" />
            <span className="slds-r3" />
            <span className="slds-r4" />
            <span className="slds-r5" />
            <span className="slds-r6" />
            <span className="slds-r7" />
            <span className="slds-r8" />
            <span className="slds-r9" />
          </span>
          <span className="slds-assistive-text">{t("slds.appLauncher.open")}</span>
        </button>
      </Dialog.Trigger>
      <Dialog.Portal>
        <Dialog.Content
          asChild
          aria-describedby={undefined}
          onInteractOutside={(e) => e.preventDefault()}
          onOpenAutoFocus={(e) => {
            // Start in the search box (type-to-find), not on the close button Radix would pick.
            e.preventDefault();
            searchRef.current?.focus();
          }}
        >
          <section
            role="dialog"
            tabIndex={-1}
            aria-modal="true"
            className="slds-modal slds-fade-in-open slds-modal_large slds-app-launcher"
          >
            <div className="slds-modal__container">
              <Dialog.Close asChild>
                <button type="button" className="slds-button slds-button_icon slds-modal__close">
                  <X className="slds-button__icon slds-button__icon_large nx-lucide" aria-hidden="true" />
                  <span className="slds-assistive-text">{close}</span>
                </button>
              </Dialog.Close>
              <div className="slds-modal__header slds-grid slds-grid_align-spread slds-grid_vertical-align-center">
                <Dialog.Title asChild>
                  <h2 className="slds-text-heading_medium">{t("slds.appLauncher.title")}</h2>
                </Dialog.Title>
                <div className="slds-app-launcher__header-search">
                  <div className="slds-form-element">
                    <label htmlFor={`${id}-search`} className="slds-form-element__label slds-assistive-text">
                      {t("slds.appLauncher.search")}
                    </label>
                    <div className="slds-form-element__control slds-input-has-icon slds-input-has-icon_left">
                      <span className="slds-icon_container slds-icon-utility-search slds-input__icon slds-input__icon_left">
                        <Search className="slds-icon slds-icon_xx-small slds-icon-text-default nx-lucide" aria-hidden="true" />
                      </span>
                      <input
                        type="search"
                        className="slds-input"
                        ref={searchRef}
                        id={`${id}-search`}
                        placeholder={t("slds.appLauncher.search")}
                        value={query}
                        onChange={(e) => setQuery(e.target.value)}
                      />
                    </div>
                  </div>
                </div>
                {actions ? <div className="slds-grid slds-gutters_xx-small">{actions}</div> : <span />}
              </div>
              <div className="slds-modal__content slds-app-launcher__content slds-p-around_medium">
                <Section id={`${id}-apps`} title={t("slds.appLauncher.apps")}>
                  {tiles.length === 0 ? (
                    <p className="slds-p-horizontal_small slds-text-color_weak">{t("slds.appLauncher.noMatch")}</p>
                  ) : (
                    <ul className="slds-grid slds-grid_pull-padded slds-wrap">
                      {tiles.map((a) => (
                        <AppTile
                          key={a.name}
                          app={a}
                          href={a.href}
                          current={a.name === activeApp}
                          onChoose={() => {
                            onSelectApp(a.name);
                            setOpen(false);
                          }}
                        />
                      ))}
                    </ul>
                  )}
                </Section>
                <hr />
                <Section id={`${id}-items`} title={t("slds.appLauncher.items")}>
                  {links.length === 0 ? (
                    <p className="slds-text-color_weak">{t("slds.appLauncher.noMatch")}</p>
                  ) : (
                    <ul className="slds-grid slds-wrap">
                      {links.map((i) => (
                        <li key={i.href} className="slds-col_padded slds-p-vertical_xx-small slds-size_1-of-2 slds-medium-size_1-of-5">
                          <Link href={i.href} className="slds-truncate" title={i.label} onClick={() => setOpen(false)}>
                            {i.label}
                          </Link>
                        </li>
                      ))}
                    </ul>
                  )}
                </Section>
              </div>
            </div>
          </section>
        </Dialog.Content>
        <Dialog.Overlay asChild>
          <div className="slds-backdrop slds-backdrop_open" role="presentation" />
        </Dialog.Overlay>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

/** Expandable section (blueprint expandable-section, as the launcher uses it). */
function Section({ id, title, children }: { id: string; title: string; children: ReactNode }) {
  const [open, setOpen] = useState(true);
  return (
    <div className={clsx("slds-section", open && "slds-is-open")}>
      <h3 className="slds-section__title">
        <button type="button" aria-controls={id} aria-expanded={open} className="slds-button slds-section__title-action" onClick={() => setOpen(!open)}>
          <ChevronDown className="slds-section__title-action-icon slds-button__icon slds-button__icon_left nx-lucide" aria-hidden="true" />
          <span className="slds-truncate" title={title}>
            {title}
          </span>
        </button>
      </h3>
      <div className="slds-section__content" id={id} hidden={!open}>
        {children}
      </div>
    </div>
  );
}

function AppTile({ app, href, current, onChoose }: { app: LauncherApp; href: string; current: boolean; onChoose: () => void }) {
  const glyph = app.name === ALL_ITEMS ? Database : appIconByName(app.icon);
  return (
    <li className="slds-p-horizontal_small slds-size_1-of-1 slds-medium-size_1-of-3">
      <div className="slds-app-launcher__tile slds-text-link_reset">
        <div className="slds-app-launcher__tile-figure">
          <span className="slds-avatar slds-avatar_large">
            <Icon icon={glyph} category="custom" name={app.name === ALL_ITEMS ? "92" : appToneIcon(app.color)} size="large" />
          </span>
        </div>
        <div className="slds-app-launcher__tile-body">
          <Link href={href} onClick={onChoose} aria-current={current || undefined}>
            {app.label}
          </Link>
          {current && <span className="slds-badge slds-badge_lightest slds-m-left_x-small">{t("slds.appLauncher.current")}</span>}
          {app.description && <p>{app.description}</p>}
        </div>
      </div>
    </li>
  );
}

// ── Connected launcher (AppShell) ───────────────────────────────────────────────────────────────

/** AppLauncher on the tenant's apps (ActiveAppProvider) and the entities this user can view. */
export function ShellAppLauncher() {
  const { user } = useAuth();
  const { apps, activeApp, setActiveApp } = useActiveApp();
  const { data: entities = [] } = useEntities();
  const { can } = usePermissions();
  const [open, setOpen] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);
  // App CRUD (/admin/apps) and Studio are mounted behind RequirePlatformOperator: only operators
  // get the create/manage buttons, otherwise they would be doors that answer 403.
  const canBuild = !!user?.isAdmin && !!user?.isPlatformOperator;

  const items = useMemo(
    () => [
      { href: "/dashboards", label: t("shell.nav.dashboards") },
      { href: "/reports", label: t("shell.nav.reports") },
      ...entities.filter((e) => can("view", e.name)).map((e) => ({ href: `/${e.name}`, label: e.label })),
    ],
    [entities, can],
  );

  return (
    <>
      <AppLauncher
        open={open}
        onOpenChange={setOpen}
        apps={apps}
        activeApp={activeApp}
        items={items}
        onSelectApp={setActiveApp}
        actions={
          canBuild && (
            <>
              <div className="slds-col">
                <button
                  type="button"
                  className="slds-button slds-button_neutral"
                  onClick={() => {
                    setOpen(false); // one modal at a time
                    setCreateOpen(true);
                  }}
                >
                  {t("slds.appLauncher.create")}
                </button>
              </div>
              <div className="slds-col">
                <Link href={studioHref(activeProjectSlug(), "dung/man-hinh/app")} className="slds-button slds-button_neutral" onClick={() => setOpen(false)}>
                  {t("slds.appLauncher.manage")}
                </Link>
              </div>
            </>
          )
        }
      />
      {canBuild && <CreateAppDialog open={createOpen} onOpenChange={setCreateOpen} />}
    </>
  );
}
