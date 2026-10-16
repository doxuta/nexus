// SLDS vertical navigation (blueprint `vertical-navigation/list`, SLDS 2.264.1: default, compact,
// items-with-icons, shade):
//   nav.slds-nav-vertical[_compact][_shade][aria-label]
//     > div.slds-nav-vertical__section > h2.slds-nav-vertical__title#id + ul[aria-describedby=id]
//         > li.slds-nav-vertical__item[.slds-is-active] > a.slds-nav-vertical__action[aria-current]
// Items are real links (Next Link), so the browser keeps middle-click / open-in-new-tab.
// Additions, all from SLDS classes: a section may have no title (then its list is not described);
// `actionText` gives the label the link colour of the overflow "Show More" action
// (`slds-nav-vertical__action-text`); `external` items leave the SPA through a plain link in the SAME tab
// (a new tab would lose a build session, specs/073 kh6 FR-023); a section `note` (e.g. why the list is empty) is weak text above its items, in
// the action's padding, on a plain li so it takes no hover highlight.
// Deviation: the active link says aria-current="page" (the blueprint writes "true") — it is a link
// to the page being shown.
import clsx from "clsx";
import Link from "next/link";
import type { LucideIcon } from "lucide-react";
import { useId, type ReactNode } from "react";
import { Icon } from "./Icon";

export interface VerticalNavItem {
  key: string;
  label: string;
  href: string;
  /** Decorative utility glyph before the label (blueprint "items-with-icons"). */
  icon?: LucideIcon;
  active?: boolean;
  /** Label in link colour — an action ("Thêm …") rather than a place. */
  actionText?: boolean;
  /** Opens another screen in a new tab. */
  external?: boolean;
  /** Trailing status text on the row (e.g. a Studio draft label). */
  status?: ReactNode;
  /** An assistant proposal draws this row (`.nx-proposed`, data-proposal-key); with `ghostOnly` it is a row that does not exist yet: text, not a link. */
  ghostKey?: string;
  ghostOnly?: boolean;
}

export interface VerticalNavSection {
  id: string;
  title?: string;
  items: VerticalNavItem[];
  /** Weak text above the items (e.g. why the list is empty). */
  note?: ReactNode;
}

export interface VerticalNavigationProps {
  /** Accessible name of the nav landmark. */
  label: string;
  sections: VerticalNavSection[];
  compact?: boolean;
  /** For a shaded (grey) column background. */
  shade?: boolean;
  className?: string;
}

function ItemLink({ item }: { item: VerticalNavItem }) {
  if (item.ghostOnly) {
    return (
      <span className="slds-nav-vertical__action">
        {item.icon && <Icon icon={item.icon} size="x-small" className="slds-line-height_reset slds-m-right_x-small" />}
        {item.label}
        {item.status && <> {item.status}</>}
      </span>
    );
  }
  const body = (
    <>
      {item.icon && <Icon icon={item.icon} size="x-small" className="slds-line-height_reset slds-m-right_x-small" />}
      {item.actionText ? <span className="slds-nav-vertical__action-text">{item.label}</span> : item.label}
      {item.status && <> {item.status}</>}
    </>
  );
  if (item.external) {
    return (
      <a href={item.href} className="slds-nav-vertical__action">
        {body}
      </a>
    );
  }
  return (
    <Link href={item.href} className="slds-nav-vertical__action" aria-current={item.active ? "page" : undefined}>
      {body}
    </Link>
  );
}

export function VerticalNavigation({ label, sections, compact, shade, className }: VerticalNavigationProps) {
  const uid = useId();
  return (
    <nav
      className={clsx("slds-nav-vertical", compact && "slds-nav-vertical_compact", shade && "slds-nav-vertical_shade", className)}
      aria-label={label}
    >
      {sections.map((s) => {
        const titleId = `${uid}-${s.id}`;
        return (
          <div key={s.id} className="slds-nav-vertical__section">
            {s.title && (
              <h2 id={titleId} className="slds-nav-vertical__title">
                {s.title}
              </h2>
            )}
            <ul aria-describedby={s.title ? titleId : undefined}>
              {s.note != null && (
                <li>
                  <p className="slds-p-vertical_xx-small slds-p-left_x-large slds-p-right_large slds-text-body_small slds-text-color_weak">
                    {s.note}
                  </p>
                </li>
              )}
              {s.items.map((item) => (
                <li key={item.key} className={clsx("slds-nav-vertical__item", item.active && "slds-is-active", item.ghostKey && "nx-proposed")} data-proposal-key={item.ghostKey}>
                  <ItemLink item={item} />
                </li>
              ))}
            </ul>
          </div>
        );
      })}
    </nav>
  );
}
