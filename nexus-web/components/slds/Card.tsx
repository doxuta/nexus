"use client";

// SLDS card (blueprint `cards`, SLDS 2.264.1): default, body-has-padding / body-has-no-padding,
// visibly-hidden-card-header, loading, empty, nested-with-boundary; RelatedListCard is the
// `related-list-table` example. The header icon is decorative (docs: redundant with the title),
// so it carries no assistive text.
// Deviation in RelatedListCard: the blueprint table says role="grid"/"gridcell" and puts
// tabindex="-1" on the record link, which assumes data-table arrow-key navigation. Without that
// model the link would be unreachable by keyboard, so the table keeps native table semantics
// and the link stays in the tab order. Classes are the blueprint's.
import clsx from "clsx";
import Link from "next/link";
import type { LucideIcon } from "lucide-react";
import type { ReactNode } from "react";
import { t } from "@/lib/i18n";
import { useEmbedded } from "./EmbeddedSurface";
import { Icon } from "./Icon";

export interface CardIcon {
  glyph: LucideIcon;
  /** Standard object colour, e.g. "account", "contact" → slds-icon-standard-<name>. */
  name: string;
}

export interface CardProps {
  title: string;
  /** Makes the title the blueprint's header link. */
  href?: string;
  icon?: CardIcon;
  /** Appended to the title as "Title (n)". */
  count?: number;
  /** Header actions (blueprint: one neutral button, or an overflow menu). */
  actions?: ReactNode;
  /** Footer "Xem tất cả" link. */
  viewAllHref?: string;
  /** Custom footer content; replaces the "Xem tất cả" link. */
  footer?: ReactNode;
  /** `slds-card__body_inner` padding (default true; false for flush tables). */
  padded?: boolean;
  loading?: boolean;
  /** Visually hidden header, still read by screen readers. */
  hideHeader?: boolean;
  /** `slds-card_boundary`: gives a nested card its border back. */
  boundary?: boolean;
  className?: string;
  children?: ReactNode;
}

export function Card({
  title,
  href,
  icon,
  count,
  actions,
  viewAllHref,
  footer,
  padded = true,
  loading,
  hideHeader,
  boundary,
  className,
  children,
}: CardProps) {
  const embedded = useEmbedded();
  const heading = count === undefined ? title : `${title} (${count})`;
  const headingText = <span>{heading}</span>;
  const footerContent =
    footer ??
    (viewAllHref && (
      <Link className="slds-card__footer-action" href={viewAllHref}>
        {t("slds.card.viewAll")} <span className="slds-assistive-text">{title}</span>
      </Link>
    ));
  if (embedded) {
    // Embedded (specs/073 FR-005): a flat section under a rule, never a card inside the work surface
    // (nested cards included, so `boundary` is moot). Keeps title, link, actions, loading and footer.
    return (
      <section className={clsx("slds-border_top slds-p-vertical_medium", className)}>
        <div className={clsx("slds-grid slds-grid_vertical-align-center slds-m-bottom_small", hideHeader && "slds-assistive-text")}>
          <h3 className="slds-text-heading_small min-w-0 flex-1 slds-truncate" title={heading}>
            {href ? <Link href={href}>{heading}</Link> : heading}
          </h3>
          {actions && <div className="slds-no-flex">{actions}</div>}
        </div>
        {loading ? (
          <div role="status" className="slds-spinner slds-spinner_small">
            <span className="slds-assistive-text">{t("slds.card.loading")}</span>
            <div className="slds-spinner__dot-a" />
            <div className="slds-spinner__dot-b" />
          </div>
        ) : (
          <>
            <div>{children}</div>
            {footerContent && <footer className="slds-m-top_small">{footerContent}</footer>}
          </>
        )}
      </section>
    );
  }
  return (
    <article className={clsx("slds-card", boundary && "slds-card_boundary", className)}>
      <div className={clsx("slds-card__header slds-grid", hideHeader && "slds-assistive-text")}>
        <header className="slds-media slds-media_center slds-has-flexi-truncate">
          {icon && (
            <div className="slds-media__figure">
              <Icon icon={icon.glyph} category="standard" name={icon.name} size="small" />
            </div>
          )}
          <div className="slds-media__body">
            <h2 className="slds-card__header-title">
              {href ? (
                <Link href={href} className="slds-card__header-link slds-truncate" title={heading}>
                  {headingText}
                </Link>
              ) : (
                <span className="slds-card__header-link slds-truncate" title={heading}>
                  {headingText}
                </span>
              )}
            </h2>
          </div>
          {actions && <div className="slds-no-flex">{actions}</div>}
        </header>
      </div>
      {loading ? (
        <div className="slds-card__body slds-card__body_inner">
          <div role="status" className="slds-spinner slds-spinner_small">
            <span className="slds-assistive-text">{t("slds.card.loading")}</span>
            <div className="slds-spinner__dot-a" />
            <div className="slds-spinner__dot-b" />
          </div>
        </div>
      ) : (
        <>
          <div className={clsx("slds-card__body", padded && "slds-card__body_inner")}>{children}</div>
          {footerContent && <footer className="slds-card__footer">{footerContent}</footer>}
        </>
      )}
    </article>
  );
}

export interface RelatedListColumn<Row> {
  key: string;
  label: string;
  /** Cell content; default is the row value at `key` as text. */
  render?: (row: Row) => ReactNode;
}

export interface RelatedListCardProps<Row extends Record<string, unknown>>
  extends Omit<CardProps, "children" | "padded" | "footer"> {
  columns: RelatedListColumn<Row>[];
  rows: Row[];
  rowKey: (row: Row) => string;
  /** Links the first column to the record. */
  rowHref?: (row: Row) => string;
}

function cellText(value: unknown): string {
  return value === null || value === undefined ? "" : String(value);
}

export function RelatedListCard<Row extends Record<string, unknown>>({
  columns,
  rows,
  rowKey,
  rowHref,
  count,
  viewAllHref,
  ...card
}: RelatedListCardProps<Row>) {
  const empty = rows.length === 0;
  return (
    // Empty state (blueprint `states/empty`): header only, body and footer collapsed.
    <Card {...card} count={count ?? rows.length} padded={false} viewAllHref={empty ? undefined : viewAllHref}>
      {!empty && (
        <table
          className="slds-table slds-table_cell-buffer slds-no-row-hover slds-table_bordered slds-table_fixed-layout"
          aria-label={card.title}
        >
          <thead>
            <tr className="slds-line-height_reset">
              {columns.map((col) => (
                <th key={col.key} scope="col">
                  <div className="slds-truncate" title={col.label}>
                    {col.label}
                  </div>
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map((row) => (
              <tr key={rowKey(row)} className="slds-hint-parent">
                {columns.map((col, i) => {
                  const text = cellText(row[col.key]);
                  const content = col.render ? col.render(row) : text;
                  const href = i === 0 && rowHref ? rowHref(row) : undefined;
                  const cell = (
                    <div className="slds-truncate" title={col.render ? undefined : text}>
                      {href ? <Link href={href}>{content}</Link> : content}
                    </div>
                  );
                  return i === 0 ? (
                    <th key={col.key} scope="row">
                      {cell}
                    </th>
                  ) : (
                    <td key={col.key}>{cell}</td>
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </Card>
  );
}
