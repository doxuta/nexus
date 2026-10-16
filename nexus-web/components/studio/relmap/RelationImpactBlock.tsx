"use client";

// RelationImpactBlock (specs/075 T150): the "Đang dùng" group of the right column — what a link is holding right now.
// Reads RelationshipImpact directly. Not a card: a group head + rule lines. Dependent roll-up fields BLOCK removal and
// are listed as links straight to the field on its table page.
import Link from "next/link";
import { t, tf } from "@/lib/i18n";
import { impactDependents } from "@/lib/relCanvas";
import { studioHref } from "@/lib/studioRoutes";
import type { RelationshipImpact } from "@/lib/api";
import { Button } from "@/components/slds/Button";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";

export interface RelationImpactBlockProps {
  slug: string;
  status: "loading" | "error" | "ok";
  impact?: RelationshipImpact;
  labelOf: (entity: string) => string;
  onRetry: () => void;
}

export function RelationImpactBlock({ slug, status, impact, labelOf, onRetry }: RelationImpactBlockProps) {
  const dep = status === "ok" && impact ? impactDependents(impact) : null;
  const pairs = impact?.relationship.relationshipType === "N-N";
  const row = (kind: "blocking" | "inert", d: { entityName: string; fieldName: string }) => (
    <li key={`${kind}:${d.entityName}.${d.fieldName}`} className="slds-p-vertical_xx-small slds-border_top">
      <Link href={studioHref(slug, "dung/du-lieu/:bang", { bang: d.entityName }, { truong: d.fieldName })}>
        {labelOf(d.entityName)} · {d.fieldName}
      </Link>
    </li>
  );
  return (
    <section aria-label={t("studio.build.relMap.panel.usage")} className="slds-border_top slds-p-top_small">
      <h3 className="slds-text-title_caps slds-m-bottom_x-small">{t("studio.build.relMap.panel.usage")}</h3>
      {status === "loading" && (
        <div className="slds-text-body_small slds-text-color_weak" style={{ position: "relative", minHeight: "1.5rem", paddingLeft: "2rem" }}>
          <Spinner size="x-small" inline label={t("studio.build.relMap.state.impactLoading")} />
          {t("studio.build.relMap.state.impactLoading")}
        </div>
      )}
      {status === "error" && (
        <>
          <ScopedNotification theme="error">{t("studio.build.relMap.state.impactError")}</ScopedNotification>
          <div className="slds-m-top_x-small">
            <Button variant="neutral" onClick={onRetry}>{t("studio.build.relMap.state.retry")}</Button>
          </div>
        </>
      )}
      {dep && (
        <>
          <p className="slds-text-body_regular">
            {dep.records === 0
              ? t("studio.build.relMap.panel.usageNone")
              : tf(pairs ? "studio.build.relMap.panel.usagePairs" : "studio.build.relMap.panel.usageRecords", { n: dep.records })}
          </p>
          {dep.blocking.length > 0 && (
            <div className="slds-m-top_x-small">
              <ScopedNotification theme="error">{t("studio.build.relMap.panel.blockedHead")}</ScopedNotification>
              <ul>{dep.blocking.map((d) => row("blocking", d))}</ul>
            </div>
          )}
          {dep.inert.length > 0 && (
            <div className="slds-m-top_x-small">
              <p className="slds-text-body_small slds-text-color_weak">{t("studio.build.relMap.panel.inertHead")}</p>
              <ul>{dep.inert.map((d) => row("inert", d))}</ul>
            </div>
          )}
        </>
      )}
    </section>
  );
}
