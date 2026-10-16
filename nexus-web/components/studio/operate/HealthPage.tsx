"use client";

// Vận hành › Sức khoẻ (specs/078 lát u7-suc-khoe, FR-103/FR-104): the lede, then only the signals that have a problem.
// No KPI row, no overall badge: the sentence says it and each row has one button. The read goes through
// useProjectQuery (token-scoped), refetches when the tab regains focus and never polls.
import { AlertOctagon, AlertTriangle, CheckCircle2, HelpCircle } from "lucide-react";
import Link from "next/link";
import { Button, buttonClassName } from "@/components/slds/Button";
import { Icon } from "@/components/slds/Icon";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { PendingPage } from "@/components/studio/PendingPage";
import { StatusText } from "@/components/studio/StatusText";
import { UsageSection } from "@/components/studio/operate/UsageSection";
import { useProjectQuery } from "@/components/studio/useProjectQuery";
import { getTenantHealth } from "@/lib/admin";
import { ApiError } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { healthLede, healthSignals, type SignalLevel } from "@/lib/operateHealth";

const GLYPH = { critical: AlertOctagon, warn: AlertTriangle, unknown: HelpCircle } as const;
const TONE = { critical: "error", warn: "warning", unknown: "default" } as const;

/** The server answers 404/405 on a Nexus build that has no GET /admin/health yet. */
const notBuilt = (e: unknown) => e instanceof ApiError && (e.status === 404 || e.status === 405 || e.status === 501);

export function HealthPage({ slug, khoang }: { slug: string; khoang?: string }) {
  const q = useProjectQuery(slug, ["operate", "health"], getTenantHealth, { refetchOnWindowFocus: true, retry: false });

  if (q.isPending) {
    return (
      <div className="slds-is-relative slds-p-around_xx-large">
        <Spinner size="medium" />
      </div>
    );
  }
  if (q.isError && notBuilt(q.error)) return <PendingPage feature={t("studio.operate.health.pending")} />;
  if (q.isError) {
    return (
      <div className="slds-p-around_medium">
        <ScopedNotification theme="error">{t("studio.operate.health.loadError")}</ScopedNotification>
        <div className="slds-m-top_small">
          <Button variant="neutral" onClick={() => void q.refetch()}>
            {t("studio.query.retry")}
          </Button>
        </div>
      </div>
    );
  }

  const lede = healthLede(q.data, slug);
  const signals = healthSignals(q.data, slug);
  return (
    <div className="slds-p-around_medium">
      <p className="nx-lede">
        {lede.unknown.length > 0 && <>{tf("studio.operate.health.lede.unknown", { sources: lede.unknown.join(", ") })} </>}
        {lede.clean && (
          <>
            <Link href={lede.href}>
              <b>{t("studio.operate.health.lede.clean")}</b>
            </Link>
            {t("studio.operate.health.lede.in24")}
          </>
        )}
        {lede.n > 0 && (
          <>
            <Link href={lede.href}>
              <b className="tabular-nums">{tf("studio.operate.health.lede.fail", { n: lede.n })}</b>
            </Link>
            {t("studio.operate.health.lede.in24")}
          </>
        )}
        {lede.m > 0 && (
          <>
            {(lede.clean || lede.n > 0) && " · "}
            <Link href={lede.href}>
              <b className="tabular-nums">{tf("studio.operate.health.lede.jobs", { m: lede.m })}</b>
            </Link>
            {t("studio.operate.health.lede.in7")}
          </>
        )}
      </p>

      {signals.length === 0 ? (
        <p className="slds-m-top_large slds-grid slds-grid_vertical-align-center">
          <Icon icon={CheckCircle2} tone="success" size="x-small" className="slds-m-right_x-small" />
          <StatusText tone="success">{t("studio.operate.health.allGood")}</StatusText>
        </p>
      ) : (
        <ul className="slds-has-dividers_bottom slds-m-top_large" aria-label={t("studio.operate.health.listLabel")}>
          {signals.map((s) => (
            <li key={s.id} className="slds-item slds-grid slds-grid_vertical-align-center">
              <Icon icon={GLYPH[s.level]} tone={TONE[s.level]} size="x-small" className="slds-m-right_small" />
              <div className="slds-col slds-has-flexi-truncate">
                <span className="slds-assistive-text">{t(`studio.operate.health.level.${s.level}` as I18nKey)}: </span>
                {s.text}
              </div>
              <div className="slds-m-left_medium">
                {s.action.retry ? (
                  <Button variant="neutral" onClick={() => void q.refetch()}>
                    {s.action.label}
                  </Button>
                ) : (
                  <Link className={buttonClassName("neutral")} href={s.action.href ?? lede.href}>
                    {s.action.label}
                  </Link>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}
      <UsageSection slug={slug} khoang={khoang} />
    </div>
  );
}

export type { SignalLevel };
