"use client";

// Sức khoẻ — one screen telling the customer's admin whether their app is running well (specs/066 L6):
// failed/stuck background jobs, failing rules, failed mail, failing inbound webhooks and the last Doctor
// sweep, under one overall label. Route is adminGated; the Doctor link only shows for an operator.
//
// SLDS (specs/038): ObjectHomePage + a Badge for the overall level + one Card per signal.

import { useEffect, useState } from "react";
import Link from "next/link";
import { HeartPulse, RefreshCw } from "lucide-react";
import { getTenantHealth, ensureAuth, ApiError, type HealthLevel, type TenantHealth } from "@/lib/admin";
import { useAuth } from "@/lib/auth/AuthProvider";
import { formatTime } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { Badge } from "@/components/slds/Badge";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Card } from "@/components/slds/Card";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { ObjectHomePage } from "@/components/slds/templates/ObjectHomePage";

const THEME = { ok: "success", warn: "warning", critical: "error" } as const;

function LevelBadge({ level }: { level: HealthLevel | "unknown" }) {
  return (
    <Badge theme={level === "unknown" ? undefined : THEME[level]}>{t(`page.health.status.${level}` as I18nKey)}</Badge>
  );
}

interface Loaded {
  tick: number;
  data: TenantHealth | null;
  err: string | null;
}

export function HealthScreen() {
  const { user } = useAuth();
  const isOperator = !!user?.isPlatformOperator;
  const [tick, setTick] = useState(0);
  const [loaded, setLoaded] = useState<Loaded | null>(null);
  const loading = loaded?.tick !== tick;

  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        await ensureAuth();
        const data = await getTenantHealth();
        if (alive) setLoaded({ tick, data, err: null });
      } catch (e) {
        if (alive) setLoaded({ tick, data: null, err: e instanceof ApiError ? e.message : t("page.health.loadError") });
      }
    })();
    return () => { alive = false; };
  }, [tick]);

  const h = loaded?.data ?? null;
  let body: React.ReactNode;
  if (loading) {
    body = (
      <div className="relative min-h-24 slds-border_top">
        <Spinner size="medium" label={t("page.health.loading")} />
      </div>
    );
  } else if (!h) {
    body = (
      <div className="slds-p-around_medium slds-border_top">
        <ScopedNotification theme="error">{loaded?.err ?? t("page.health.loadError")}</ScopedNotification>
      </div>
    );
  } else {
    body = (
      <div className="grid grid-cols-1 gap-3 slds-p-horizontal_medium slds-p-bottom_small md:grid-cols-2">
        <Card title={t("page.health.card.jobs")}>
          {h.jobs.failed7d.length === 0 ? (
            <p className="slds-text-color_weak">{t("page.health.card.jobsNone")}</p>
          ) : (
            <ul>
              {h.jobs.failed7d.map((j) => (
                <li key={j.jobType}>
                  {tf("page.health.rules.row", { name: t(`page.jobs.type.${j.jobType}` as I18nKey) || j.jobType, n: j.count })}
                </li>
              ))}
            </ul>
          )}
          <p className={h.jobs.stuck > 0 ? "slds-text-color_error" : "slds-text-color_weak"} data-testid="stuck">
            {!h.queueEnabled ? t("page.health.stuckOff") : tf("page.health.stuck", { n: h.jobs.stuck })}
          </p>
          <Link href="/admin/jobs">{t("page.health.jobsLink")}</Link>
        </Card>

        <Card title={t("page.health.card.rules")}>
          <p>{tf("page.health.rules.counts", { d: h.workflowRuns.failed24h, w: h.workflowRuns.failed7d })}</p>
          <ul>
            {h.workflowRuns.topFailing.map((r) => (
              <li key={r.workflowId}>{tf("page.health.rules.row", { name: r.name, n: r.failed })}</li>
            ))}
          </ul>
        </Card>

        <Card title={t("page.health.card.emails")}>
          <p>{tf("page.health.emails.counts", { d: h.emails.failed24h, w: h.emails.failed7d })}</p>
          <p className={h.emails.unresentFailed > 0 ? "slds-text-color_error" : "slds-text-color_weak"}>
            {tf("page.health.emails.unresent", { n: h.emails.unresentFailed })}
          </p>
          <Link href="/admin/email-log?status=failed">{t("page.health.emailsLink")}</Link>
        </Card>

        <Card title={t("page.health.card.hooks")}>
          {h.inboundHooks.byHook.length === 0 ? (
            <p className="slds-text-color_weak">{t("page.health.hooksNone")}</p>
          ) : (
            <ul>
              {h.inboundHooks.byHook.map((k) => (
                <li key={k.hookId}>{tf("page.health.hooks.row", { name: k.name, n: k.failed })}</li>
              ))}
            </ul>
          )}
        </Card>

        <Card title={t("page.health.card.doctor")}>
          <p><LevelBadge level={h.doctor.status} /></p>
          <p className="slds-text-color_weak">
            {h.doctor.checkedAt
              ? tf("page.health.doctor.counts", { c: h.doctor.critical, w: h.doctor.warn, at: formatTime(h.doctor.checkedAt) })
              : t("page.health.doctor.never")}
          </p>
          {isOperator && <Link href="/admin/doctor">{t("page.health.doctorLink")}</Link>}
        </Card>
      </div>
    );
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
      <div className="p-4">
        <ObjectHomePage
          icon={{ glyph: HeartPulse, name: "health" }}
          label={t("page.health.label")}
          title={t("page.health.title")}
          meta={h && !loading ? <LevelBadge level={h.status} /> : "…"}
          controls={[
            <ButtonIcon key="refresh" icon={RefreshCw} variant="border-filled" label={t("action.refresh")} onClick={() => setTick((n) => n + 1)} />,
          ]}
        >
          {body}
        </ObjectHomePage>
      </div>
    </div>
  );
}
