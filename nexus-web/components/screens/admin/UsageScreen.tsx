"use client";

// Mức dùng — the customer admin's per-day count of what their automation did (specs/066 L4): rule runs,
// mail, webhooks out and webhooks in. Answers "how much is this customer's app doing, and how much of
// it failed?". Today is counted live by the server; earlier days come from its hourly roll-up, so the
// old figures do not shrink when the source logs are cleaned. Route is adminGated (the customer's own
// admin, not an operator).
//
// SLDS (specs/038): ObjectHomePage + Combobox range (7 / 30 / 90 / 365 days — 365 is why the roll-up
// keeps two years), four Cards of totals each saying how many failed, the kit DataTable per day.

import { useEffect, useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { BarChart3, RefreshCw } from "lucide-react";
import { getUsage, ensureAuth, ApiError, type UsageCounts, type UsageDay, type UsageReport } from "@/lib/admin";
import { formatBytes, formatTime } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { Alert } from "@/components/slds/Alert";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Card } from "@/components/slds/Card";
import { Combobox } from "@/components/slds/Combobox";
import { DataTable } from "@/components/slds/DataTable";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { ObjectHomePage } from "@/components/slds/templates/ObjectHomePage";

const RANGES = [7, 30, 90, 365] as const;
type RangeDays = (typeof RANGES)[number];

/** YYYY-MM-DD → DD/MM/YYYY without going through a Date (a date-only string has no zone to shift). */
const showDay = (d: string) => d.split("-").reverse().join("/");

/** The server's own "today" is not known before the first answer, so `from` is cut from the browser's
 *  calendar day in the same fixed zone the app shows times in. Only the length of the range matters. */
function fromFor(days: RangeDays): string {
  const vn = new Date(Date.now() + 7 * 3600_000); // UTC+7 wall clock, read through the UTC getters
  vn.setUTCDate(vn.getUTCDate() - (days - 1));
  return vn.toISOString().slice(0, 10);
}

interface Loaded {
  key: string;
  report: UsageReport | null;
  err: string | null;
}

const COLS: { key: keyof UsageCounts; label: I18nKey }[] = [
  { key: "workflowRuns", label: "page.usage.col.runs" },
  { key: "workflowRunsFailed", label: "page.usage.col.runsFailed" },
  { key: "emailsSent", label: "page.usage.col.emailsSent" },
  { key: "emailsFailed", label: "page.usage.col.emailsFailed" },
  { key: "webhooksDelivered", label: "page.usage.col.webhooksDelivered" },
  { key: "webhooksFailed", label: "page.usage.col.webhooksFailed" },
  { key: "hooksReceived", label: "page.usage.col.hooksReceived" },
  { key: "hooksFailed", label: "page.usage.col.hooksFailed" },
  { key: "usersSeen", label: "page.usage.col.usersSeen" },
];

type RecordRow = NonNullable<UsageReport["current"]>["records"][number];

const RECORD_COLUMNS: ColumnDef<RecordRow>[] = [
  { id: "label", header: t("page.usage.current.col.entity"), accessorFn: (r) => r.label },
  { id: "count", header: t("page.usage.current.col.count"), size: 140, accessorFn: (r) => r.count.toLocaleString("vi-VN"), meta: { numeric: true } },
];

/** "Hiện tại": who is using the app, how much data it holds and how much room the attachments take. */
function CurrentBlock({ current: c }: { current: NonNullable<UsageReport["current"]> }) {
  const cards: { id: string; title: I18nKey; value: string; note?: string }[] = [
    { id: "usersActive", title: "page.usage.current.usersActive", value: c.usersActive.toLocaleString("vi-VN") },
    { id: "usersSeen30d", title: "page.usage.current.usersSeen30d", value: c.usersSeen30d.toLocaleString("vi-VN") },
    { id: "records", title: "page.usage.current.records", value: c.recordsTotal.toLocaleString("vi-VN") },
    { id: "files", title: "page.usage.current.files", value: formatBytes(c.fileBytes), note: tf("page.usage.current.filesNote", { n: c.fileCount.toLocaleString("vi-VN") }) },
  ];
  return (
    <>
      <h2 className="slds-text-heading_small slds-p-horizontal_medium slds-p-bottom_x-small">
        {t("page.usage.current.title")} <span className="slds-text-body_small slds-text-color_weak">{tf("page.usage.current.at", { at: formatTime(c.computedAt) })}</span>
      </h2>
      <div className="grid grid-cols-1 gap-3 slds-p-horizontal_medium slds-p-bottom_small sm:grid-cols-2 lg:grid-cols-4">
        {cards.map((k) => (
          <Card key={k.id} title={t(k.title)}>
            <p className="slds-text-heading_large" data-testid={`current-${k.id}`}>{k.value}</p>
            {k.note && <p className="slds-text-color_weak">{k.note}</p>}
          </Card>
        ))}
      </div>
      {c.records.length > 0 && (
        <div className="slds-p-horizontal_medium slds-p-bottom_small">
          <DataTable label={t("page.usage.current.table")} columns={RECORD_COLUMNS} data={c.records} />
        </div>
      )}
    </>
  );
}

export function UsageScreen() {
  const [days, setDays] = useState<RangeDays>(30);
  const [tick, setTick] = useState(0);
  const [loaded, setLoaded] = useState<Loaded | null>(null);

  // `loading` is derived from the key, so a slower answer for an older range never overwrites the newer one.
  const requestKey = `${days}#${tick}`;
  const loading = loaded?.key !== requestKey;
  const report = loaded?.report ?? null;
  const err = loaded?.err ?? null;

  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        await ensureAuth();
        const res = await getUsage(fromFor(days));
        if (alive) setLoaded({ key: requestKey, report: res, err: null });
      } catch (e) {
        if (alive) setLoaded({ key: requestKey, report: null, err: e instanceof ApiError ? e.message : t("page.usage.loadError") });
      }
    })();
    return () => { alive = false; };
  }, [days, requestKey]);

  const columns = useMemo<ColumnDef<UsageDay>[]>(
    () => [
      { id: "day", header: t("page.usage.col.day"), size: 112, accessorFn: (d) => showDay(d.day) },
      ...COLS.map(({ key, label }): ColumnDef<UsageDay> => ({
        id: key,
        header: t(label),
        size: 120,
        accessorFn: (d) => d[key].toLocaleString("vi-VN"),
        meta: { numeric: true },
      })),
    ],
    [],
  );

  const rangeOptions = RANGES.map((n) => ({ value: String(n), label: t(`page.usage.range.${n}` as I18nKey) }));

  let body: React.ReactNode;
  if (loading) {
    body = (
      <div className="relative min-h-24 slds-border_top">
        <Spinner size="medium" label={t("page.usage.loading")} />
      </div>
    );
  } else if (err || !report) {
    body = (
      <div className="slds-p-around_medium slds-border_top">
        <ScopedNotification theme="error">{err ?? t("page.usage.loadError")}</ScopedNotification>
      </div>
    );
  } else {
    const tt = report.totals;
    const cards: { title: I18nKey; total: number; failed: number }[] = [
      { title: "page.usage.card.runs", total: tt.workflowRuns, failed: tt.workflowRunsFailed },
      { title: "page.usage.card.emails", total: tt.emailsSent + tt.emailsFailed, failed: tt.emailsFailed },
      { title: "page.usage.card.webhooksOut", total: tt.webhooksDelivered + tt.webhooksFailed, failed: tt.webhooksFailed },
      { title: "page.usage.card.webhooksIn", total: tt.hooksReceived + tt.hooksFailed, failed: tt.hooksFailed },
    ];
    body = (
      <>
        {!report.rollupEnabled && (
          <div className="slds-p-horizontal_medium slds-p-bottom_small">
            <Alert variant="warning">{t("page.usage.noRollup")}</Alert>
          </div>
        )}
        <div className="grid grid-cols-1 gap-3 slds-p-horizontal_medium slds-p-bottom_small sm:grid-cols-2 lg:grid-cols-4">
          {cards.map((c) => (
            <Card key={c.title} title={t(c.title)}>
              <p className="slds-text-heading_large" data-testid={`total-${c.title}`}>{c.total.toLocaleString("vi-VN")}</p>
              <p className={c.failed > 0 ? "slds-text-color_error" : "slds-text-color_weak"}>
                {tf("page.usage.card.failed", { n: c.failed.toLocaleString("vi-VN") })}
              </p>
            </Card>
          ))}
        </div>
        {report.current && <CurrentBlock current={report.current} />}
        <p className="slds-text-body_small slds-text-color_weak slds-p-horizontal_medium slds-p-bottom_small">
          {report.lastRollupAt ? tf("page.usage.note", { at: formatTime(report.lastRollupAt) }) : t("page.usage.noteNever")}{" "}
          {t("page.usage.caveat")}
        </p>
        <div className="slds-scrollable_x slds-border_top">
          <DataTable label={t("page.usage.table")} columns={columns} data={[...report.days].reverse()} className="min-w-[72rem]" />
        </div>
      </>
    );
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
      <div className="p-4">
        <ObjectHomePage
          icon={{ glyph: BarChart3, name: "usage" }}
          label={t("page.usage.label")}
          title={t("page.usage.title")}
          meta={report && !loading ? tf("page.usage.meta", { from: showDay(report.from), to: showDay(report.to), tz: report.timezone }) : "…"}
          controls={[
            <ButtonIcon
              key="refresh"
              icon={RefreshCw}
              variant="border-filled"
              label={t("action.refresh")}
              onClick={() => setTick((n) => n + 1)}
            />,
          ]}
        >
          <div className="flex flex-wrap items-center gap-2 slds-p-horizontal_medium slds-p-bottom_small">
            <Combobox
              className="w-48"
              label={t("page.usage.range")}
              labelHidden
              options={rangeOptions}
              value={String(days)}
              onChange={(v) => setDays((Number(v) || 30) as RangeDays)}
            />
          </div>
          {body}
        </ObjectHomePage>
      </div>
    </div>
  );
}
