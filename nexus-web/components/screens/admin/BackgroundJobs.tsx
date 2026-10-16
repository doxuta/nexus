"use client";

// Việc nền — the customer admin's delivery log (DAILY-QUEUE #33; BE #31 list + #32 resend,
// specs/021-durable-job-queue §3). Answers "why didn't the partner system get our order?": every
// outbound webhook delivery is a sys_jobs row, and this page shows the tenant's own rows with the
// receiver's error VERBATIM (stored already masked: origin + HTTP status, never a secret, header or
// payload — the API never returns the payload). "Gửi lại" requeues the SAME row, so the receiver
// sees the same X-Nexus-Delivery-Id and can drop a duplicate.
// Route is adminGated, not operator: the customer's own tenant-admin must reach it (Setup menu
// entry is not operatorOnly).
//
// SLDS (specs/038): page-headers/object-home in a card (ObjectHomePage; refresh as a list control),
// a filter row of combobox (select) like /admin/audit, data-tables/base through the kit DataTable
// with the error column as the blueprint's "Cell content wrapped" (in full — no clamp, see DataTable),
// badges for the status, tooltips/base on "không gửi lại tay được" (a timer, or a webhook whose rule
// was deleted — its handler fails that delivery permanently and the BE Retry does not refuse it),
// prompt/base before a resend (shared ConfirmPrompt) and scoped-notifications/base for its outcome.
// Server-ordered keyset log: no client-side sort (one page of it would mislead).

import { useEffect, useMemo, useRef, useState } from "react";
import Link from "next/link";
import type { ColumnDef } from "@tanstack/react-table";
import { Info, RefreshCw, Send } from "lucide-react";
import {
  listBackgroundJobs, retryBackgroundJob, ensureAuth, ApiError,
  type BackgroundJob, type BackgroundJobStatus,
} from "@/lib/admin";
import { listWorkflows } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Combobox } from "@/components/slds/Combobox";
import { DataTable } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { Tooltip } from "@/components/slds/Tooltip";
import { ObjectHomePage } from "@/components/slds/templates/ObjectHomePage";
import { useEmbedded } from "@/components/slds/EmbeddedSurface";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { ConfirmPrompt } from "@/components/studio/workspaceShared";

const PAGE_LIMIT = 50;
const STATUSES: BackgroundJobStatus[] = ["failed", "queued", "running", "done"];
// The tenant-pinned job types. email_alert is listed too since specs/066 pinned a rule's mail to its
// tenant (FR-013); its row labels "Gửi email" and its history lives in Nhật ký email.
const TYPES = ["outbound_webhook", "workflow_timer", "email_alert"] as const;
// The one type whose resend is safe by construction (the receiver deduplicates on the delivery id);
// the server refuses the rest with 409 JOB_NOT_RETRYABLE (services.BackgroundJobService.Retry).
// It can be resent when it failed AND (#256) when it was delivered and the receiver lost it.
const RESENDABLE = "outbound_webhook";

const STATUS_THEME: Partial<Record<BackgroundJobStatus, "success" | "error">> = { failed: "error", done: "success" };

// In the Studio frame the maker reads "Gửi dữ liệu sang hệ thống khác", not "Giao webhook" (S13).
const typeLabel = (type: string, embedded = false) =>
  embedded && type === RESENDABLE
    ? t("studio.operate.jobs.type.outbound_webhook")
    : (TYPES as readonly string[]).includes(type) ? t(`page.jobs.type.${type}` as I18nKey) : type;
const statusLabel = (s: string) =>
  (STATUSES as string[]).includes(s) ? t(`page.jobs.status.${s}` as I18nKey) : s;

/** The loaded list for one request key (filters + refresh tick). */
interface LoadedPage {
  key: string;
  jobs: BackgroundJob[];
  nextCursor?: string;
  err: string | null;
}

interface Notice {
  theme: "success" | "warning" | "error";
  text: string;
}

/** `emailLogHref`: where "Xem ở Nhật ký email" goes; the Studio page passes its own route (default: the customer's admin page). */
export function BackgroundJobs({ emailLogHref = "/admin/email-log" }: { emailLogHref?: string } = {}) {
  const embedded = useEmbedded();
  // Default view = what the customer came for: the deliveries that failed.
  const [status, setStatus] = useState<BackgroundJobStatus | "">("failed");
  const [type, setType] = useState("");
  const [tick, setTick] = useState(0);
  const [page, setPage] = useState<LoadedPage | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);
  // Rule id → name; undefined while loading, null when it could not be read (then no "deleted" claim).
  const [ruleNames, setRuleNames] = useState<Map<string, string> | null | undefined>(undefined);
  const [target, setTarget] = useState<BackgroundJob | null>(null);
  const [retrying, setRetrying] = useState(false);
  const [notice, setNotice] = useState<Notice | null>(null);
  const noticeRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    let alive = true;
    listWorkflows()
      .then((ws) => { if (alive) setRuleNames(new Map(ws.map((w) => [w.id, w.name]))); })
      .catch(() => { if (alive) setRuleNames(null); });
    return () => { alive = false; };
  }, []);

  // One request per (filters, tick); `loading` is derived from the key, so a slower answer for an
  // older filter never overwrites the newer one (same shape as AuditLog).
  const requestKey = `${status}|${type}#${tick}`;
  const loading = page?.key !== requestKey;
  const jobs = useMemo(() => page?.jobs ?? [], [page]);
  const nextCursor = page?.nextCursor;
  const err = page?.err ?? null;

  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        await ensureAuth();
        const res = await listBackgroundJobs({ status: status || undefined, type: type || undefined, limit: PAGE_LIMIT });
        if (alive) setPage({ key: requestKey, jobs: res.jobs, nextCursor: res.nextCursor, err: null });
      } catch (e) {
        if (alive) setPage({ key: requestKey, jobs: [], err: e instanceof ApiError ? e.message : t("page.jobs.loadError") });
      }
    })();
    return () => { alive = false; };
  }, [status, type, requestKey]);

  const loadMore = async () => {
    if (!nextCursor || loadingMore) return;
    setLoadingMore(true);
    try {
      const res = await listBackgroundJobs({ status: status || undefined, type: type || undefined, limit: PAGE_LIMIT, after: nextCursor });
      setPage((p) => p && { ...p, jobs: [...p.jobs, ...res.jobs], nextCursor: res.nextCursor });
    } catch (e) {
      setNotice({ theme: "error", text: e instanceof ApiError ? e.message : t("page.jobs.loadMoreError") });
    } finally {
      setLoadingMore(false);
    }
  };

  const ruleLabel = useMemo(
    () => (j: BackgroundJob) => {
      if (!j.workflowId || !ruleNames) return "—";
      return ruleNames.get(j.workflowId) ?? t("page.jobs.ruleDeleted");
    },
    [ruleNames],
  );

  async function handleRetry(job: BackgroundJob) {
    setRetrying(true);
    try {
      const next = await retryBackgroundJob(job.id);
      setPage((p) => p && { ...p, jobs: p.jobs.map((j) => (j.id === next.id ? next : j)) });
      setNotice({ theme: "success", text: t("page.jobs.retry.done") });
    } catch (e) {
      const code = e instanceof ApiError ? e.code : "";
      if (code === "JOB_NOT_RETRYABLE" || code === "JOB_NOT_FOUND") {
        // The row changed under us (another press, another tab): say so and show the truth.
        const notRetryable = embedded ? "studio.operate.jobs.retry.notRetryable" : "page.jobs.retry.notRetryable";
        setNotice({ theme: "warning", text: t(code === "JOB_NOT_FOUND" ? "page.jobs.retry.notFound" : notRetryable) });
        setTick((n) => n + 1);
      } else if (code === "JOBS_DISABLED") {
        setNotice({ theme: "error", text: t("page.jobs.retry.disabled") });
      } else {
        setNotice({ theme: "error", text: e instanceof ApiError ? e.message : t("page.jobs.retry.error") });
      }
    } finally {
      setRetrying(false);
      setTarget(null);
    }
  }

  const hasKeyField = jobs.some((j) => j.credentialName !== undefined);
  const columns = useMemo<ColumnDef<BackgroundJob>[]>(
    () => [
      { id: "createdAt", header: t("page.jobs.col.createdAt"), size: 128, accessorFn: (j) => formatDateTime(j.createdAt) },
      { id: "type", header: t("page.jobs.col.type"), size: 120, accessorFn: (j) => typeLabel(j.jobType, embedded) },
      { id: "rule", header: t("page.jobs.col.rule"), size: 160, accessorFn: ruleLabel },
      // Only when the server sends the field (any row has the key, null included): an older build has
      // nothing to show and must not get an empty column.
      ...(hasKeyField
        ? [{ id: "credential", header: t("page.jobs.col.credential"), size: 136, accessorFn: (j: BackgroundJob) => j.credentialName ?? "—" }]
        : []),
      {
        id: "status",
        header: t("page.jobs.col.status"),
        size: 96,
        accessorFn: (j) => statusLabel(j.status),
        cell: ({ row }) => <Badge theme={STATUS_THEME[row.original.status]}>{statusLabel(row.original.status)}</Badge>,
      },
      {
        id: "attempts",
        header: t("page.jobs.col.attempts"),
        size: 88,
        meta: { numeric: true },
        accessorFn: (j) => tf("page.jobs.attempts", { n: j.attempts, max: j.maxAttempts }),
      },
      { id: "lastError", header: t("page.jobs.col.lastError"), meta: { wrap: true }, accessorFn: (j) => j.lastError ?? "—" },
      {
        id: "finishedAt",
        header: t("page.jobs.col.finishedAt"),
        size: 128,
        accessorFn: (j) => (j.finishedAt ? formatDateTime(j.finishedAt) : "—"),
      },
      {
        id: "action",
        header: t("page.jobs.col.action"),
        size: 176,
        cell: ({ row }) => {
          const j = row.original;
          if (j.status !== "failed" && !(j.status === "done" && j.jobType === RESENDABLE)) return null;
          // Deleted only once the rule list has loaded (null = unreadable claims nothing): the handler
          // fails a deleted rule's delivery permanently, so a resend could only fail again.
          const ruleGone = !!j.workflowId && !!ruleNames && !ruleNames.has(j.workflowId);
          if (j.jobType === "email_alert") {
            return (
              <span className="flex items-center gap-1">
                <Link href={emailLogHref}>{t("page.jobs.emailLogLink")}</Link>
                <Tooltip trigger={<ButtonIcon icon={Info} label={t("page.jobs.notRetryableHelp")} />}>
                  {t("page.jobs.emailWhere")}
                </Tooltip>
              </span>
            );
          }
          if (j.jobType === RESENDABLE && !ruleGone) {
            return (
              <Button
                aria-label={tf("page.jobs.retryRow", { at: formatDateTime(j.createdAt), rule: ruleLabel(j) })}
                onClick={() => setTarget(j)}
              >
                {t("page.jobs.retry")}
              </Button>
            );
          }
          return (
            <span className="flex items-center gap-1">
              <span className="slds-text-color_weak">{t("page.jobs.notRetryable")}</span>
              <Tooltip trigger={<ButtonIcon icon={Info} label={t("page.jobs.notRetryableHelp")} />}>
                {t(ruleGone ? "page.jobs.ruleGoneWhy" : "page.jobs.notRetryableWhy")}
              </Tooltip>
            </span>
          );
        },
      },
    ],
    [ruleLabel, ruleNames, hasKeyField, embedded, emailLogHref],
  );

  const changeFilter = (apply: () => void) => {
    apply();
    setNotice(null);
  };

  const statusOptions = [
    { value: "", label: t("page.jobs.filter.statusAll") },
    ...STATUSES.map((s) => ({ value: s, label: statusLabel(s) })),
  ];
  const typeOptions = [
    { value: "", label: t("page.jobs.filter.typeAll") },
    ...TYPES.map((ty) => ({ value: ty, label: typeLabel(ty, embedded) })),
  ];

  let list: React.ReactNode;
  if (loading) {
    list = (
      <div className="relative min-h-24 slds-border_top">
        <Spinner size="medium" label={t("page.jobs.loading")} />
      </div>
    );
  } else if (err) {
    list = (
      <div className="slds-p-around_medium slds-border_top">
        <ScopedNotification theme="error">{err}</ScopedNotification>
      </div>
    );
  } else if (jobs.length === 0) {
    const failedView = status === "failed" && !type;
    list = (
      <div className="slds-border_top">
        <EmptyState
          className="slds-p-around_large"
          icon={Send}
          title={t(failedView ? "page.jobs.emptyFailed" : "page.jobs.empty")}
          description={failedView ? t("page.jobs.emptyFailedDesc") : undefined}
        />
      </div>
    );
  } else {
    list = (
      <>
        {/* The sized columns add up to 56rem (+8.5rem with the key column) and the error column takes the rest; on a narrow screen
            the table keeps an 84rem floor and scrolls inside the card instead of squeezing the error
            column to nothing. */}
        <div className="slds-scrollable_x">
          <DataTable
            label={t("page.jobs.table")}
            columns={columns}
            data={jobs}
            className="min-w-[84rem]"
            getRowId={(j) => j.id}
            rowDetail={(j) =>
              embedded && target?.id === j.id ? (
                <div className="slds-p-around_small">
                  <InlineConfirm
                    tone="warning"
                    message={tf("studio.operate.jobs.retry.ask", { rule: ruleLabel(j) })}
                    confirmLabel={t("page.jobs.retry")}
                    busy={retrying}
                    onConfirm={() => handleRetry(j)}
                    onCancel={() => setTarget(null)}
                  />
                </div>
              ) : null
            }
          />
        </div>
        {nextCursor && (
          <div className="slds-p-around_small slds-text-align_center slds-border_top">
            <Button onClick={loadMore} disabled={loadingMore}>
              {loadingMore ? t("page.jobs.loadingMore") : t("page.jobs.loadMore")}
            </Button>
          </div>
        )}
      </>
    );
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
      <div className="p-4">
        <ObjectHomePage
          icon={{ glyph: Send, name: "send-log" }}
          label={t("page.jobs.label")}
          title={t(status ? (`page.jobs.view.${status}` as I18nKey) : "page.jobs.view.all")}
          meta={loading ? "…" : tf(nextCursor ? "page.jobs.metaMore" : "page.jobs.meta", { n: jobs.length })}
          controls={[
            <ButtonIcon
              key="refresh"
              icon={RefreshCw}
              variant="border-filled"
              label={t("action.refresh")}
              onClick={() => changeFilter(() => setTick((n) => n + 1))}
            />,
          ]}
        >
          <div className="flex flex-wrap items-center gap-2 slds-p-horizontal_medium slds-p-bottom_small">
            <Combobox
              className="w-48"
              label={t("page.jobs.filter.status")}
              labelHidden
              options={statusOptions}
              value={status}
              onChange={(v) => changeFilter(() => setStatus((v ?? "") as BackgroundJobStatus | ""))}
            />
            <Combobox
              className="w-48"
              label={t("page.jobs.filter.type")}
              labelHidden
              options={typeOptions}
              value={type}
              onChange={(v) => changeFilter(() => setType(v ?? ""))}
            />
          </div>
          {notice && (
            <div ref={noticeRef} tabIndex={-1} className="slds-p-horizontal_medium slds-p-bottom_small">
              <ScopedNotification theme={notice.theme}>{notice.text}</ScopedNotification>
            </div>
          )}
          {list}
        </ObjectHomePage>
      </div>

      <ConfirmPrompt
        open={target !== null && !embedded}
        onOpenChange={(o) => { if (!o) setTarget(null); }}
        title={t(target?.status === "done" ? "page.jobs.retry.titleDone" : "page.jobs.retry.title")}
        description={target && tf(target.status === "done" ? "page.jobs.retry.descDone" : "page.jobs.retry.desc", { rule: ruleLabel(target) })}
        confirmLabel={t("page.jobs.retry")}
        loading={retrying}
        onConfirm={() => target && handleRetry(target)}
        returnFocus={() => noticeRef.current}
      />
    </div>
  );
}
