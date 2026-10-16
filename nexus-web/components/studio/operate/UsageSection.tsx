"use client";

// Vận hành › Sức khoẻ & mức dùng, nửa mức dùng (specs/078 lát u7-muc-dung-bieu-do, FR-100..FR-103):
// "câu dẫn rồi một biểu đồ" — who is using the app and how much room it takes, then ONE daily chart with a range
// and a measure.
import { useRouter } from "next/navigation";
import { useMemo, useState } from "react";
import { DailyChart, type DailyChartSeries } from "@/components/charts/DailyChart";
import { Select } from "@/components/slds/Select";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { PendingPage } from "@/components/studio/PendingPage";
import { QueryState } from "@/components/studio/QueryState";
import { useProjectQuery } from "@/components/studio/useProjectQuery";
import { OperateSection } from "@/components/studio/operate/OperateSection";
import { getUsage, type UsageDay } from "@/lib/admin";
import { ApiError } from "@/lib/api";
import { formatBytes, formatTime } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { usageRange } from "@/lib/operateData";
import { studioHref } from "@/lib/studioRoutes";

const RANGES = [7, 30, 90, 365] as const;
const DEFAULT_RANGE = 30;
// The server's zone before its first answer; the answer's own `timezone` replaces it (one extra read at most).
const GUESS_TZ = "Asia/Ho_Chi_Minh";

type Measure = "users" | "runs" | "email" | "out" | "in";
const MEASURES: { id: Measure; ok: keyof UsageDay; failed?: keyof UsageDay }[] = [
  { id: "users", ok: "usersSeen" },
  { id: "runs", ok: "workflowRuns", failed: "workflowRunsFailed" },
  { id: "email", ok: "emailsSent", failed: "emailsFailed" },
  { id: "out", ok: "webhooksDelivered", failed: "webhooksFailed" },
  { id: "in", ok: "hooksReceived", failed: "hooksFailed" },
];

const showDay = (d: string) => d.split("-").reverse().join("/");

export function UsageSection({ slug, khoang }: { slug: string; khoang?: string }) {
  const router = useRouter();
  const days = RANGES.find((n) => String(n) === khoang) ?? DEFAULT_RANGE;
  const [measure, setMeasure] = useState<Measure>("users");
  const [tz, setTz] = useState(GUESS_TZ);
  const { from, to } = usageRange(days, new Date(), tz);
  // `to` is left out of the request: the server then ends on ITS today and can never refuse it as "in the future".
  const q = useProjectQuery(slug, ["operate", "usage", from, to], () => getUsage(from), { staleTime: 60_000 });
  const data = q.data;
  // Adjusting state while rendering (React's documented pattern): the zone the server answered with replaces the guess.
  if (data?.timezone && data.timezone !== tz) setTz(data.timezone);

  const m = MEASURES.find((x) => x.id === measure) ?? MEASURES[0];
  const series: DailyChartSeries[] = useMemo(
    () =>
      m.failed
        ? [
            { key: "ok", label: t("studio.operate.usage.series.ok"), tone: "normal" },
            { key: "failed", label: t("studio.operate.usage.series.failed"), tone: "error" },
          ]
        : [{ key: "ok", label: t("studio.operate.usage.series.users"), tone: "normal" }],
    [m],
  );
  const points = useMemo(
    () => (data?.days ?? []).map((d) => ({ day: d.day, values: { ok: d[m.ok] as number, failed: m.failed ? (d[m.failed] as number) : 0 } })),
    [data, m],
  );

  // #412 is not on this server: say so, draw nothing (never a chart of invented zeros).
  if (q.isError && q.error instanceof ApiError && q.error.status === 404) return <PendingPage feature={t("studio.route.operHealth")} />;

  const title = tf("studio.operate.usage.title", { metric: t(`studio.operate.usage.metric.${measure}` as I18nKey), n: days });
  const c = data?.current;
  const num = (n: number) => n.toLocaleString("vi-VN");
  return (
    <div className="slds-m-top_x-large">
      <QueryState query={q}>
        {data && (
          <>
            {c && (
              <p className="nx-lede">
                <b className="tabular-nums">{c.usersSeen30d}/{c.usersActive}</b> {t("studio.operate.usage.lede.people")} · <b className="tabular-nums">{num(c.recordsTotal)}</b>{" "}
                {t("studio.operate.usage.lede.records")} · <b className="tabular-nums">{formatBytes(c.fileBytes)}</b> {t("studio.operate.usage.lede.files")}
              </p>
            )}
            {!data.rollupEnabled && (
              <div className="slds-m-top_small">
                <ScopedNotification theme="warning">{t("studio.operate.usage.rollupOff")}</ScopedNotification>
              </div>
            )}
            <OperateSection
              title={title}
              actions={
                <div className="flex gap-3">
                  <Select
                    label={t("studio.operate.usage.rangeLabel")}
                    placeholder={false}
                    value={String(days)}
                    onChange={(e) => router.replace(studioHref(slug, "van-hanh/suc-khoe", {}, { khoang: e.target.value }))}
                    options={RANGES.map((n) => ({ value: String(n), label: tf("studio.operate.usage.range", { n }) }))}
                  />
                  <Select
                    label={t("studio.operate.usage.metricLabel")}
                    placeholder={false}
                    value={measure}
                    onChange={(e) => setMeasure(e.target.value as Measure)}
                    options={MEASURES.map((x) => ({ value: x.id, label: t(`studio.operate.usage.metric.${x.id}` as I18nKey) }))}
                  />
                </div>
              }
            >
              <DailyChart series={series} points={points} title={title} formatDay={showDay} />
              <p className="slds-m-top_small slds-text-body_small slds-text-color_weak">
                {data.lastRollupAt
                  ? tf("studio.operate.usage.note", { time: formatTime(data.lastRollupAt) })
                  : t("studio.operate.usage.noteBare")}
              </p>
            </OperateSection>
          </>
        )}
      </QueryState>
    </div>
  );
}
