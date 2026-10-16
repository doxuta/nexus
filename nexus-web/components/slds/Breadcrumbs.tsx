"use client";

// SLDS breadcrumbs (blueprint `breadcrumbs/base`, SLDS 2.264.1). `.slds-breadcrumb` draws the
// separators itself. The overflow-menu variant is not wrapped (no caller yet).
import Link from "next/link";
import { t } from "@/lib/i18n";

export interface BreadcrumbItem {
  label: string;
  href: string;
}

export interface BreadcrumbsProps {
  items: BreadcrumbItem[];
  /** Landmark name; blueprint default is "Breadcrumbs". */
  label?: string;
}

export function Breadcrumbs({ items, label }: BreadcrumbsProps) {
  return (
    <nav role="navigation" aria-label={label ?? t("slds.breadcrumbs.label")}>
      <ol className="slds-breadcrumb slds-list_horizontal slds-wrap">
        {items.map((item) => (
          <li key={item.href} className="slds-breadcrumb__item">
            <Link href={item.href}>{item.label}</Link>
          </li>
        ))}
      </ol>
    </nav>
  );
}
