"use client";

// One titled part of an operate page (specs/078 FR-017): heading, actions at its right, the body, "Xem tất cả" below.
// `level` is the heading level so the page keeps one h1 and the parts sit at h2 (or h3 inside a part).
import Link from "next/link";
import type { ReactNode } from "react";
import { t } from "@/lib/i18n";

export function OperateSection({
  level = 2,
  title,
  actions,
  seeAll,
  children,
}: {
  level?: 2 | 3;
  title: string;
  actions?: ReactNode;
  seeAll?: { href: string; label?: string };
  children: ReactNode;
}) {
  const H = level === 2 ? "h2" : "h3";
  return (
    <section className="slds-m-top_large">
      <div className="slds-grid slds-grid_vertical-align-center slds-wrap">
        <H className="slds-text-heading_small slds-col" style={{ fontSize: "1.125rem", fontWeight: 600 }}>{title}</H>
        {actions && <div className="slds-col_bump-left">{actions}</div>}
      </div>
      <div className="slds-m-top_small">{children}</div>
      {seeAll && (
        <p className="slds-m-top_small">
          <Link href={seeAll.href}>{seeAll.label ?? t("studio.operate.auto.feed.all")}</Link>
        </p>
      )}
    </section>
  );
}
