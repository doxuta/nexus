// SLDS builder header (blueprint `builder-header/base` + `toolbar`, SLDS 2.264.1) — the dark bar of
// a builder (Studio), and the white toolbar under it:
//   div.slds-builder-header_container > header.slds-builder-header
//     > div.slds-builder-header__item > a.slds-builder-header__item-action (back)
//     + div.slds-builder-header__item > …item-label.slds-media (app icon + name)
//     + nav.slds-builder-header__item.slds-builder-header__nav > ul.slds-builder-header__nav-list > li.slds-builder-header__nav-item > a
//     + div.slds-builder-header__item.slds-has-flexi-truncate > h1.slds-builder-header__item-label (page type)
//     + div.slds-builder-header__item.slds-builder-header__utilities > div.slds-builder-header__utilities-item > a
//   div.slds-builder-toolbar[role=toolbar] > div.slds-builder-toolbar__item-group + div.slds-builder-toolbar__actions
// Deviations:
// - The container is `static z-auto`: SLDS fixes it to the top at z-index 100; Nexus stacks header,
//   builder header and the workspace in one flex column instead (same call as GlobalHeader).
// - Nav entries are links, not the blueprint's dropdown triggers (Studio's destinations are pages);
//   the current one carries aria-current — "page" only when it IS the open page, "true" when the
//   open page sits inside its section (WCAG 4.1.2) — SLDS has no visual state for it.
// - The app name is a link (to the builder's home) using the item-action class the utilities use.
// - No back item when there is no parent to go to (the blueprint always shows one).
// - BuilderToolbar is exported on its own so a screen can mount it right under the header.
// - Below the md breakpoint the app name and the nav/utility labels become screen-reader-only, so
//   every item still fits as an icon (the blueprint has no narrow layout; its labels overflow).
// Glyphs are Lucide inside `slds-icon_container slds-current-color` (sprite is CC BY-ND).
import clsx from "clsx";
import Link from "next/link";
import { ArrowLeft, type LucideIcon } from "lucide-react";
import type { ReactNode } from "react";

export interface BuilderHeaderLink {
  label: string;
  href: string;
  icon: LucideIcon;
  /** "page": this link is the open page; "true": the open page sits inside this entry's section. */
  current?: "page" | "true";
}

export interface BuilderHeaderProps {
  /** Up one level; omitted ⇒ no back item. */
  back?: { href: string; label: string };
  appName: string;
  appIcon: LucideIcon;
  appHref?: string;
  /** Accessible name of the nav item. */
  navLabel: string;
  nav: BuilderHeaderLink[];
  /** "Page type" — what is being built right now. */
  title: string;
  /** Inline after the title (scope note, status). */
  titleSuffix?: ReactNode;
  utilities?: BuilderHeaderLink[];
  /** id of the nav landmark (skip-link target). */
  navId?: string;
  className?: string;
}

function Glyph({ icon: G }: { icon: LucideIcon }) {
  return (
    <span className="slds-icon_container slds-current-color">
      <G aria-hidden="true" className="slds-icon slds-icon_x-small nx-lucide" />
    </span>
  );
}

function MediaLink({ link }: { link: BuilderHeaderLink }) {
  return (
    <Link
      href={link.href}
      className="slds-builder-header__item-action slds-media slds-media_center"
      aria-current={link.current}
    >
      <span className="slds-media__figure">
        <Glyph icon={link.icon} />
      </span>
      <span className="slds-media__body max-md:sr-only">
        <span className="slds-truncate" title={link.label}>
          {link.label}
        </span>
      </span>
    </Link>
  );
}

export function BuilderHeader({
  back,
  appName,
  appIcon,
  appHref,
  navLabel,
  nav,
  title,
  titleSuffix,
  utilities = [],
  navId,
  className,
}: BuilderHeaderProps) {
  const appLabel = (
    <>
      <span className="slds-media__figure">
        <Glyph icon={appIcon} />
      </span>
      <span className="slds-media__body max-md:sr-only">{appName}</span>
    </>
  );
  return (
    <div className={clsx("slds-builder-header_container static z-auto", className)}>
      <header className="slds-builder-header">
        {back && (
          <div className="slds-builder-header__item">
            <Link href={back.href} className="slds-builder-header__item-action" title={back.label}>
              <span className="slds-icon_container slds-current-color">
                <ArrowLeft aria-hidden="true" className="slds-icon slds-icon_x-small nx-lucide" />
                <span className="slds-assistive-text">{back.label}</span>
              </span>
            </Link>
          </div>
        )}
        <div className="slds-builder-header__item">
          {appHref ? (
            <Link href={appHref} className="slds-builder-header__item-action slds-media slds-media_center">
              {appLabel}
            </Link>
          ) : (
            <div className="slds-builder-header__item-label slds-media slds-media_center">{appLabel}</div>
          )}
        </div>
        <nav id={navId} className="slds-builder-header__item slds-builder-header__nav" aria-label={navLabel}>
          <ul className="slds-builder-header__nav-list">
            {nav.map((n) => (
              <li key={n.href} className="slds-builder-header__nav-item">
                <MediaLink link={n} />
              </li>
            ))}
          </ul>
        </nav>
        <div className="slds-builder-header__item slds-has-flexi-truncate">
          <h1 className="slds-builder-header__item-label">
            <span className="slds-truncate" title={title}>
              {title}
            </span>
            {titleSuffix}
          </h1>
        </div>
        <div className="slds-builder-header__item slds-builder-header__utilities">
          {utilities.map((u) => (
            <div key={u.href} className="slds-builder-header__utilities-item">
              <MediaLink link={u} />
            </div>
          ))}
        </div>
      </header>
    </div>
  );
}

export interface BuilderToolbarProps {
  /** Accessible name of the toolbar. */
  label: string;
  /** Left item group. */
  children?: ReactNode;
  /** Right-aligned document actions (buttons). */
  actions?: ReactNode;
  className?: string;
}

export function BuilderToolbar({ label, children, actions, className }: BuilderToolbarProps) {
  return (
    <div className={clsx("slds-builder-toolbar", className)} role="toolbar" aria-label={label}>
      <div className="slds-builder-toolbar__item-group min-w-0">{children}</div>
      {actions && <div className="slds-builder-toolbar__actions shrink-0">{actions}</div>}
    </div>
  );
}
