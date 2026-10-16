"use client";

// Card — panel surface primitive (Phase 5b): --panel · --bd · --r-card · --sh-card, with
// optional header (title + actions) and footer. Recipe = the /dev/kit Section + the list
// cards in AppsManager. One card level only — no card-in-card (design-anti-slop).

import { clsx } from "clsx";

export function Card({
  title,
  actions,
  footer,
  className,
  bodyClassName,
  children,
}: {
  /** string → standard 14px display heading; ReactNode → rendered as-is. */
  title?: React.ReactNode;
  /** Right-aligned header cluster (buttons, badge…). */
  actions?: React.ReactNode;
  footer?: React.ReactNode;
  className?: string;
  /** Replaces the default body padding (p-4) — avoids same-property class conflicts. */
  bodyClassName?: string;
  children: React.ReactNode;
}) {
  return (
    <section className={clsx("rounded-card border border-bd bg-panel shadow-card", className)}>
      {(title || actions) && (
        <header className="flex items-center justify-between gap-3 border-b border-bd2 px-4 py-2.5">
          {typeof title === "string" ? (
            <h2 className="font-display text-14px font-semibold text-fg">{title}</h2>
          ) : (
            title
          )}
          {actions}
        </header>
      )}
      <div className={bodyClassName ?? "p-4"}>{children}</div>
      {footer && <footer className="border-t border-bd2 px-4 py-2.5">{footer}</footer>}
    </section>
  );
}
