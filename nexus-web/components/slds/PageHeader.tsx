// SLDS page header (blueprint `page-headers`, SLDS 2.264.1): base, object-home, record-home,
// related-list. It is the top of the three page templates (list view, record home, form), so
// every blueprint region is a slot:
//   row 1  col-title  [breadcrumbs] media(icon · name(label + title · titleMenu) · [name-meta])
//          col-actions controls > control × actions[]
//   row 2  object-home / related-list: col-meta(meta-text) · col-controls > control × controls[]
//          record-home: row_gutters > col-details > detail-row > detail-block × details[]
// The icon svg needs `slds-page-header__icon` (2.25rem), which the shared Icon wrapper has no
// glyph-class slot for, so the tile is drawn here with the same classes Icon uses.
// Small screens: SLDS has no page-header rules for them (Salesforce ships a separate mobile app), so
// on a phone the action group pushed the name out of view and the highlights truncated to "Cập n…".
// Below its medium breakpoint (48em = Tailwind `md`) layout utilities stack title and actions and
// wrap the highlights two per line; from 48em up the blueprint layout is untouched.
"use client";

import clsx from "clsx";
import type { LucideIcon } from "lucide-react";
import type { ReactNode } from "react";
import { Breadcrumbs, type BreadcrumbItem } from "./Breadcrumbs";
import { useEmbedded } from "./EmbeddedSurface";

export type PageHeaderVariant = "base" | "object-home" | "record-home" | "related-list";

export interface PageHeaderDetail {
  label: string;
  value: ReactNode;
}

export interface PageHeaderProps {
  variant?: PageHeaderVariant;
  icon?: {
    glyph: LucideIcon;
    /** Standard object colour, e.g. "account" → slds-icon-standard-account. */
    name: string;
    /** Object type for screen readers when nothing else on the header names it (base). */
    label?: string;
  };
  /** Small text above the title: object plural (object-home) or record type (record-home). */
  label?: string;
  title: string;
  /** List-view switcher trigger (object-home), rendered in `slds-page-header__name-switcher`. */
  titleMenu?: ReactNode;
  /** Base: name-meta under the title. Object-home / related-list: meta text of the second row. */
  meta?: ReactNode;
  /** Page-level actions; each entry becomes one `slds-page-header__control`. */
  actions?: ReactNode[];
  /** List controls of the second row (object-home / related-list); one control each. */
  controls?: ReactNode[];
  /** Record-home highlight fields (compact layout). */
  details?: PageHeaderDetail[];
  /** Related-list: parent trail above the title. */
  breadcrumbs?: BreadcrumbItem[];
  /** Let the title wrap onto at most two lines (full text in `title`) instead of `slds-truncate`'s ellipsis. */
  wrapTitle?: boolean;
  className?: string;
}

const VARIANT_CLASS: Record<PageHeaderVariant, string | undefined> = {
  base: undefined,
  "object-home": undefined, // the blueprint uses no modifier here
  "record-home": "slds-page-header_record-home",
  "related-list": "slds-page-header_related-list",
};

// Two lines then an ellipsis: a long customer name must stay readable, and two similar names must not look alike.
const CLAMP_TWO_LINES = { display: "-webkit-box", WebkitLineClamp: 2, WebkitBoxOrient: "vertical", overflow: "hidden" } as const;

function Controls({ items }: { items: ReactNode[] }) {
  return (
    <div className="slds-page-header__controls">
      {items.map((item, i) => (
        <div key={i} className="slds-page-header__control">
          {item}
        </div>
      ))}
    </div>
  );
}

export function PageHeader({
  variant = "base",
  icon,
  label,
  title,
  titleMenu,
  meta,
  actions,
  controls,
  details,
  breadcrumbs,
  wrapTitle,
  className,
}: PageHeaderProps) {
  const embedded = useEmbedded();
  const metaInName = variant === "base";
  const Glyph = icon?.glyph;
  if (embedded) {
    // Embedded (specs/073 FR-005): one flat H2 on the work surface, meta + actions to the right under a rule.
    // No icon tile, no H1 (the frame owns it), no breadcrumbs (the frame's tabs/steps are the trail).
    const right = [...(actions ?? []), ...(controls ?? [])];
    return (
      <header className={clsx("slds-m-bottom_medium", className)}>
        <div className="slds-grid slds-grid_vertical-align-center slds-border_bottom slds-p-bottom_small max-md:flex-wrap max-md:gap-y-2">
          <div className="min-w-0 flex-1 max-md:basis-full slds-grid slds-grid_vertical-align-center">
            <h2 className="slds-text-heading_medium slds-truncate" title={title}>
              {label && <span className="slds-text-color_weak slds-m-right_x-small">{label}</span>}
              {title}
            </h2>
            {titleMenu && <div className="slds-m-left_x-small">{titleMenu}</div>}
          </div>
          {(meta || right.length > 0) && (
            <div className="slds-no-flex slds-grid slds-grid_vertical-align-center max-md:max-w-full max-md:flex-wrap max-md:gap-y-2">
              {meta && <p className="slds-text-body_small slds-text-color_weak slds-m-right_small">{meta}</p>}
              {right.map((item, i) => (
                <div key={i} className="slds-m-left_x-small">
                  {item}
                </div>
              ))}
            </div>
          )}
        </div>
        {details && details.length > 0 && (
          <dl className="slds-grid slds-wrap slds-m-top_small max-md:gap-y-3">
            {details.map((d) => (
              <div key={d.label} className="slds-m-right_large">
                <dt className="slds-text-title">{d.label}</dt>
                <dd>{d.value}</dd>
              </div>
            ))}
          </dl>
        )}
      </header>
    );
  }
  return (
    <div className={clsx("slds-page-header", VARIANT_CLASS[variant], className)}>
      <div className="slds-page-header__row max-md:flex-wrap">
        <div className="slds-page-header__col-title max-md:basis-full">
          {breadcrumbs && <Breadcrumbs items={breadcrumbs} />}
          <div className="slds-media">
            {icon && Glyph && (
              <div className="slds-media__figure">
                <span className={`slds-icon_container slds-icon-standard-${icon.name}`} title={icon.label}>
                  <Glyph aria-hidden="true" className="slds-icon slds-page-header__icon nx-lucide nx-lucide_tile" />
                  {icon.label && <span className="slds-assistive-text">{icon.label}</span>}
                </span>
              </div>
            )}
            <div className="slds-media__body">
              <div className="slds-page-header__name">
                <div className="slds-page-header__name-title">
                  <h1>
                    {label && <span>{label}</span>}
                    <span
                      className={clsx("slds-page-header__title", !wrapTitle && "slds-truncate")}
                      title={title}
                      style={wrapTitle ? CLAMP_TWO_LINES : undefined}
                    >
                      {title}
                    </span>
                  </h1>
                </div>
                {titleMenu && <div className="slds-page-header__name-switcher">{titleMenu}</div>}
              </div>
              {metaInName && meta && <p className="slds-page-header__name-meta">{meta}</p>}
            </div>
          </div>
        </div>
        {actions && actions.length > 0 && (
          <div className="slds-page-header__col-actions max-md:basis-full max-md:pt-2">
            <Controls items={actions} />
          </div>
        )}
      </div>
      {!metaInName && (meta || (controls && controls.length > 0)) && (
        <div className="slds-page-header__row">
          <div className="slds-page-header__col-meta">{meta && <p className="slds-page-header__meta-text">{meta}</p>}</div>
          {controls && controls.length > 0 && (
            <div className="slds-page-header__col-controls">
              <Controls items={controls} />
            </div>
          )}
        </div>
      )}
      {details && details.length > 0 && (
        <div className="slds-page-header__row slds-page-header__row_gutters">
          <div className="slds-page-header__col-details">
            <ul className="slds-page-header__detail-row max-md:flex-wrap max-md:gap-y-3">
              {details.map((d) => (
                <li key={d.label} className="slds-page-header__detail-block max-md:basis-1/2 max-md:max-w-1/2 max-md:pr-4 max-md:pl-0">
                  <div className="slds-text-title slds-truncate" title={d.label}>
                    {d.label}
                  </div>
                  <div className="slds-truncate" title={typeof d.value === "string" ? d.value : undefined}>
                    {d.value}
                  </div>
                </li>
              ))}
            </ul>
          </div>
        </div>
      )}
    </div>
  );
}
