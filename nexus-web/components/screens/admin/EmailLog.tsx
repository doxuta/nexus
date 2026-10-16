"use client";

// Nhật ký email — the customer admin's log of the mail their automation rules sent (specs/066 L1b).
// Answers "did the customer get the confirmation mail?": one row per mail with the result of the
// delivery. The log holds NO address, rendered subject or body — the recipient is masked
// ("l***@gmail.com"), the subject is the rule's TEMPLATE and the error is masked at the source — and
// this screen shows exactly that. Route is adminGated (the customer's own admin, not an operator).
//
// SLDS (specs/038): ObjectHomePage + Combobox filter + the kit DataTable (error column wrapped in
// full), badges for the status, scoped-notification when the server has no mail configured.
// "Gửi lại" (specs/066 L2): a failed rule mail the server says canResend opens a Modal that shows the
// PREVIEW first (masked recipient, the subject as it will be sent, a "luật đã đổi" warning) — nothing is sent
// until the second click. The mail is rebuilt from the live rule and the record's current data, and each
// failed row can be resent once; the loser of a double press gets a toast and a refreshed list. There is no
// link between the old row and the new one (the contract returns no id on the old row): the old row says
// "Đã gửi lại · HH:mm", the new one carries a "Gửi lại" badge.
// specs/066 L3: the log also holds the forgot-password mail and the link a customer admin issued, plus a rule's
// mail that failed BEFORE it was sent (empty address). A "Nguồn" column + filter tell them apart; a reset_link row
// names who issued it ("bởi An", or "bởi Người dựng" when that id is not a user of this customer). Auth rows are
// never resendable (their token is single use), so they never show "Gửi lại".
// A record is named by its table's LABEL ("Liên hệ"), never the machine name; a table that no longer
// exists reads "Bảng đã xoá" without a link. Server-ordered keyset log: no client-side sort.

import { useEffect, useMemo, useRef, useState } from "react";
import Link from "next/link";
import type { ColumnDef } from "@tanstack/react-table";
import { Mail, RefreshCw } from "lucide-react";
import {
  listEmailLog, resendEmail, ensureAuth, ApiError,
  type EmailLogEntry, type EmailLogSource, type EmailLogStatus, type EmailResendPreview,
} from "@/lib/admin";
import { listEntities } from "@/lib/api";
import { resendRefusal } from "@/lib/emailResend";
import { EmailResendConfirm } from "@/components/studio/operate/EmailResendConfirm";
import { formatDateTime, formatTime } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { Alert } from "@/components/slds/Alert";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Combobox } from "@/components/slds/Combobox";
import { DataTable } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { Modal } from "@/components/slds/Modal";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { toast } from "@/components/slds/Toast";
import { useEmbedded } from "@/components/slds/EmbeddedSurface";
import { ObjectHomePage } from "@/components/slds/templates/ObjectHomePage";

const PAGE_LIMIT = 50;
const STATUSES: EmailLogStatus[] = ["failed", "queued", "sent"];
const STATUS_THEME: Partial<Record<EmailLogStatus, "success" | "error">> = { failed: "error", sent: "success" };

const statusLabel = (s: string) =>
  (STATUSES as string[]).includes(s) ? t(`page.emailLog.status.${s}` as I18nKey) : s;

// specs/066 L3: a mail comes from a rule, a forgot-password request or a link the customer's admin issued.
const SOURCES: EmailLogSource[] = ["workflow", "password_reset", "reset_link"];
const sourceLabel = (s: string) =>
  (SOURCES as string[]).includes(s) ? t(`page.emailLog.source.${s}` as I18nKey) : s;

/** "Link do quản trị cấp · bởi An": the issuer by name; an id that is no user of this customer is the builder. */
function sourceCell(e: EmailLogEntry): string {
  const label = sourceLabel(e.source);
  if (!e.createdBy) return label;
  if (e.source !== "reset_link") return label;
  return `${label} · ${e.createdByName ? tf("page.emailLog.issuedBy", { name: e.createdByName }) : t("page.emailLog.issuedByBuilder")}`;
}

/** The loaded list for one request key (filter + refresh tick). */
interface LoadedPage {
  key: string;
  entries: EmailLogEntry[];
  nextCursor?: string;
  mailConfigured: boolean;
  err: string | null;
}

// ?status= is read ONCE at build (like FieldACL's ?entity=): useSearchParams on an /admin page needs a
// <Suspense> and breaks the Next build. "Xem email lỗi" links from the health screen arrive here.
function statusFromUrl(): EmailLogStatus | "" {
  if (typeof window === "undefined") return "failed";
  const s = new URLSearchParams(window.location.search).get("status");
  if (s !== null && (STATUSES as string[]).includes(s)) return s as EmailLogStatus;
  return s === null ? "failed" : "";
}

/**
 * `initialStatus` (the Studio page reads `?trang-thai=` and passes it down) wins over `window.location`; `recordHref`
 * builds the link of a record (the Studio page opens it in the same tab, inside its own frame). Both default to the
 * customer's admin behaviour.
 */
export function EmailLog({ initialStatus, recordHref = (entity, id) => `/${entity}/${id}` }: {
  initialStatus?: EmailLogStatus | "";
  recordHref?: (entity: string, id: string) => string;
} = {}) {
  const embedded = useEmbedded();
  // Default view = what the customer came for: the mail that did not go out.
  const [status, setStatus] = useState<EmailLogStatus | "">(() => initialStatus ?? statusFromUrl());
  const [source, setSource] = useState<EmailLogSource | "">("");
  const [tick, setTick] = useState(0);
  const [page, setPage] = useState<LoadedPage | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);
  const [moreErr, setMoreErr] = useState<string | null>(null);
  // The resend Modal: the row being resent, its server-built preview, and what went wrong.
  const [target, setTarget] = useState<EmailLogEntry | null>(null);
  const [preview, setPreview] = useState<EmailResendPreview | null>(null);
  const [resendErr, setResendErr] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  // Studio: the same flow as an InlineConfirm above the list instead of a Modal.
  const [studioTarget, setStudioTarget] = useState<EmailLogEntry | null>(null);
  // Table name → label; undefined while loading, null when it could not be read (then no "deleted" claim).
  const [tableLabels, setTableLabels] = useState<Map<string, string> | null | undefined>(undefined);

  useEffect(() => {
    let alive = true;
    listEntities()
      .then((es) => { if (alive) setTableLabels(new Map(es.map((e) => [e.name, e.label]))); })
      .catch(() => { if (alive) setTableLabels(null); });
    return () => { alive = false; };
  }, []);

  // `loading` is derived from the key, so a slower answer for an older filter never overwrites the
  // newer one (same shape as BackgroundJobs / AuditLog).
  const requestKey = `${status}|${source}#${tick}`;
  const loading = page?.key !== requestKey;
  const entries = useMemo(() => page?.entries ?? [], [page]);
  const nextCursor = page?.nextCursor;
  const err = page?.err ?? null;

  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        await ensureAuth();
        const res = await listEmailLog({ status: status || undefined, source: source || undefined, limit: PAGE_LIMIT });
        if (alive) setPage({ key: requestKey, entries: res.entries, nextCursor: res.nextCursor, mailConfigured: res.mailConfigured, err: null });
      } catch (e) {
        if (alive) setPage({ key: requestKey, entries: [], mailConfigured: true, err: e instanceof ApiError ? e.message : t("page.emailLog.loadError") });
      }
    })();
    return () => { alive = false; };
  }, [status, source, requestKey]);

  const loadMore = async () => {
    if (!nextCursor || loadingMore) return;
    setLoadingMore(true);
    setMoreErr(null);
    try {
      const res = await listEmailLog({ status: status || undefined, source: source || undefined, limit: PAGE_LIMIT, after: nextCursor });
      setPage((p) => p && { ...p, entries: [...p.entries, ...res.entries], nextCursor: res.nextCursor });
    } catch (e) {
      setMoreErr(e instanceof ApiError ? e.message : t("page.emailLog.loadMoreError"));
    } finally {
      setLoadingMore(false);
    }
  };

  // A late preview of a row the admin already closed must not fill the Modal of the next row.
  const resendReq = useRef(0);
  const openResend = (entry: EmailLogEntry) => {
    const mine = ++resendReq.current;
    setTarget(entry);
    setPreview(null);
    setResendErr(null);
    resendEmail(entry.id)
      .then((p) => { if (resendReq.current === mine) setPreview(p); })
      .catch((e) => {
        if (resendReq.current !== mine) return;
        const r = resendRefusal(e);
        if (r.closes) {
          // The row was resent meanwhile (another admin): say so and drop the stale button, like the confirm path.
          toast.warning(r.text);
          resendReq.current++;
          setTarget(null);
        } else {
          setResendErr(r.text);
        }
        if (r.reload) setTick((n) => n + 1);
      });
  };

  const closeResend = () => {
    if (sending) return;
    resendReq.current++;
    setTarget(null);
    setPreview(null);
    setResendErr(null);
  };

  const confirmResend = async () => {
    if (!target || sending) return;
    setSending(true);
    setResendErr(null);
    try {
      await resendEmail(target.id, false);
      toast.success(t("page.emailLog.resend.done"));
      resendReq.current++;
      setTarget(null);
      setPreview(null);
      setTick((n) => n + 1);
    } catch (e) {
      const r = resendRefusal(e);
      if (r.closes) {
        toast.warning(r.text);
        resendReq.current++;
        setTarget(null);
        setPreview(null);
      } else {
        setResendErr(r.text);
      }
      if (r.reload) setTick((n) => n + 1);
    } finally {
      setSending(false);
    }
  };

  const anyResendable = entries.some((e) => e.canResend);
  const columns = useMemo<ColumnDef<EmailLogEntry>[]>(
    () => [
      { id: "createdAt", header: t("page.emailLog.col.createdAt"), size: 128, accessorFn: (e) => formatDateTime(e.createdAt) },
      { id: "source", header: t("page.emailLog.col.source"), size: 176, meta: { wrap: true }, accessorFn: sourceCell },
      {
        id: "recipient",
        header: t("page.emailLog.col.recipient"),
        size: 176,
        // A failed row with no recipient is a mail that never left (empty address / bad template).
        accessorFn: (e) => e.recipientMasked || (e.status === "failed" ? t("page.emailLog.noAddress") : "—"),
        meta: { wrap: true },
      },
      { id: "subject", header: t("page.emailLog.col.subject"), size: 200, meta: { wrap: true }, accessorFn: (e) => e.subjectTemplate ?? "—" },
      {
        id: "rule",
        header: t("page.emailLog.col.rule"),
        size: 160,
        accessorFn: (e) => e.workflowName ?? (e.workflowId ? t("page.emailLog.ruleDeleted") : "—"),
      },
      {
        id: "record",
        header: t("page.emailLog.col.record"),
        size: 144,
        accessorFn: (e) => e.entityName ?? "",
        cell: ({ row }) => {
          const e = row.original;
          if (!e.entityName || !e.recordId) return "—";
          // Unreadable table list claims nothing: the machine name is never shown, so just a generic label.
          if (tableLabels === undefined) return "…";
          if (tableLabels === null) return <Link href={recordHref(e.entityName, e.recordId)}>{t("page.emailLog.openRecord")}</Link>;
          const label = tableLabels.get(e.entityName);
          if (!label) return <span className="slds-text-color_weak">{t("page.emailLog.tableDeleted")}</span>;
          return <Link href={recordHref(e.entityName, e.recordId)}>{label}</Link>;
        },
      },
      {
        id: "status",
        header: t("page.emailLog.col.status"),
        size: 128,
        accessorFn: (e) => statusLabel(e.status),
        cell: ({ row }) => {
          const e = row.original;
          return (
            <span className="flex flex-wrap items-center gap-1">
              <Badge theme={STATUS_THEME[e.status]}>{statusLabel(e.status)}</Badge>
              {e.attempts > 1 && <span className="slds-text-color_weak">{tf("page.emailLog.attempt", { n: e.attempts })}</span>}
              {e.resendOf && <Badge variant="lightest">{t("page.emailLog.isResend")}</Badge>}
              {e.resentAt && <Badge variant="lightest">{tf(embedded ? "studio.operate.email.resend.at" : "page.emailLog.resent", { at: formatTime(e.resentAt) })}</Badge>}
            </span>
          );
        },
      },
      { id: "error", header: t("page.emailLog.col.error"), meta: { wrap: true }, accessorFn: (e) => e.lastError ?? "—" },
      ...(anyResendable
        ? [{
            id: "actions",
            header: t("page.emailLog.col.actions"),
            size: 96,
            accessorFn: (e: EmailLogEntry) => (e.canResend ? t("page.emailLog.resend") : ""),
            cell: ({ row }: { row: { original: EmailLogEntry } }) => {
              const e = row.original;
              if (!e.canResend) return null;
              return (
                <Button
                  aria-label={tf("page.emailLog.resendRow", { at: formatDateTime(e.createdAt), to: e.recipientMasked })}
                  onClick={() => (embedded ? setStudioTarget(e) : openResend(e))}
                >
                  {t("page.emailLog.resend")}
                </Button>
              );
            },
          } satisfies ColumnDef<EmailLogEntry>]
        : []),
    ],
    [tableLabels, anyResendable, recordHref, embedded],
  );

  const statusOptions = [
    { value: "", label: t("page.emailLog.filter.statusAll") },
    ...STATUSES.map((s) => ({ value: s, label: statusLabel(s) })),
  ];

  const sourceOptions = [
    { value: "", label: t("page.emailLog.filter.sourceAll") },
    ...SOURCES.map((s) => ({ value: s, label: sourceLabel(s) })),
  ];

  let list: React.ReactNode;
  if (loading) {
    list = (
      <div className="relative min-h-24 slds-border_top">
        <Spinner size="medium" label={t("page.emailLog.loading")} />
      </div>
    );
  } else if (err) {
    list = (
      <div className="slds-p-around_medium slds-border_top">
        <ScopedNotification theme="error">{err}</ScopedNotification>
      </div>
    );
  } else if (entries.length === 0) {
    const noFilter = status === "" && source === "";
    list = (
      <div className="slds-border_top">
        <EmptyState
          className="slds-p-around_large"
          icon={Mail}
          title={t(noFilter ? "page.emailLog.emptyAll" : "page.emailLog.empty")}
          description={noFilter ? t("page.emailLog.emptyAllDesc") : undefined}
        />
      </div>
    );
  } else {
    list = (
      <>
        <div className="slds-scrollable_x">
          <DataTable label={t("page.emailLog.table")} columns={columns} data={entries} className="min-w-[76rem]" />
        </div>
        {moreErr && (
          <div className="slds-p-around_small slds-border_top">
            <ScopedNotification theme="error">{moreErr}</ScopedNotification>
          </div>
        )}
        {nextCursor && (
          <div className="slds-p-around_small slds-text-align_center slds-border_top">
            <Button onClick={loadMore} disabled={loadingMore}>
              {loadingMore ? t("page.emailLog.loadingMore") : t("page.emailLog.loadMore")}
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
          icon={{ glyph: Mail, name: "email-log" }}
          label={t("page.emailLog.label")}
          title={t(status ? (`page.emailLog.view.${status}` as I18nKey) : "page.emailLog.view.all")}
          meta={loading ? "…" : tf(nextCursor ? "page.emailLog.metaMore" : "page.emailLog.meta", { n: entries.length })}
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
              label={t("page.emailLog.filter.status")}
              labelHidden
              options={statusOptions}
              value={status}
              onChange={(v) => setStatus((v ?? "") as EmailLogStatus | "")}
            />
            {/* An older server sends no `source`: then there is nothing to filter by. */}
            {(!embedded || source !== "" || entries.some((e) => e.source)) && (
              <Combobox
                className="w-56"
                label={t("page.emailLog.filter.source")}
                labelHidden
                options={sourceOptions}
                value={source}
                onChange={(v) => setSource((v ?? "") as EmailLogSource | "")}
              />
            )}
            <span className="slds-text-body_small slds-text-color_weak">{t("page.emailLog.privacy")}</span>
          </div>
          {page && !page.err && !page.mailConfigured && (
            <div className="slds-p-horizontal_medium slds-p-bottom_small">
              <ScopedNotification theme="warning">{t(embedded ? "studio.operate.email.mailOff" : "page.emailLog.mailOff")}</ScopedNotification>
            </div>
          )}
          {embedded && studioTarget && (
            <div className="slds-p-horizontal_medium slds-p-bottom_small">
              <EmailResendConfirm entry={studioTarget} onClose={() => setStudioTarget(null)} onChanged={() => setTick((n) => n + 1)} />
            </div>
          )}
          {list}
        </ObjectHomePage>
      </div>

      <Modal
        open={!embedded && target !== null}
        onOpenChange={(o) => { if (!o) closeResend(); }}
        title={t("page.emailLog.resend.title")}
        footer={
          <>
            <Button onClick={closeResend} disabled={sending}>{t("action.cancel")}</Button>
            <Button variant="brand" onClick={confirmResend} disabled={sending || preview === null}>
              {t(sending ? "page.emailLog.resend.sending" : "page.emailLog.resend.confirm")}
            </Button>
          </>
        }
      >
        {resendErr && <Alert variant="error" className="slds-m-bottom_small">{resendErr}</Alert>}
        {preview === null && !resendErr && (
          <div className="relative min-h-16"><Spinner size="small" label={t("page.emailLog.resend.loading")} /></div>
        )}
        {preview && (
          <>
            {preview.ruleChangedSinceSend && (
              <Alert variant="warning" className="slds-m-bottom_small">{t("page.emailLog.resend.ruleChanged")}</Alert>
            )}
            <dl className="slds-m-bottom_small">
              <dt className="slds-text-title">{t("page.emailLog.resend.to")}</dt>
              <dd className="slds-m-bottom_x-small">{preview.recipientMasked}</dd>
              <dt className="slds-text-title">{t("page.emailLog.resend.subject")}</dt>
              <dd className="slds-m-bottom_x-small">{preview.subject}</dd>
              {target?.workflowName && (
                <>
                  <dt className="slds-text-title">{t("page.emailLog.resend.rule")}</dt>
                  <dd>{target.workflowName}</dd>
                </>
              )}
            </dl>
            <p>{t("page.emailLog.resend.rebuilt")}</p>
            <p className="slds-m-top_x-small">{t("page.emailLog.resend.duplicate")}</p>
          </>
        )}
      </Modal>
    </div>
  );
}
