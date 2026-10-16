"use client";

// Vận hành › Việc tự động & kết nối (specs/078 lát u7-tu-dong-tong-quan, FR-070..FR-079): "Nhật ký hai phần".
// A lede with the failure counts (each number a link), the last 24 hours as one flat table, and the connections.
// No tile grid of links, no second tab strip. "Gửi lại" runs one POST at a time and re-checks the project scope
// before EVERY call (FR-131): between two calls the token may already belong to another customer.
import { useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import Link from "next/link";
import { useMemo, useRef, useState, type ReactNode } from "react";
import { Button } from "@/components/slds/Button";
import { DataTable, fill } from "@/components/slds/DataTable";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { QueryState } from "@/components/studio/QueryState";
import { StatusText } from "@/components/studio/StatusText";
import { useProjectQuery } from "@/components/studio/useProjectQuery";
import { OperateSection } from "@/components/studio/operate/OperateSection";
import { useScopeGate } from "@/components/studio/operate/useScopeGate";
import { ApiError, getTenantHealth, listApiKeys, listCredentials, retryBackgroundJob, type ApiKey, type NamedCredential } from "@/lib/admin";
import { listInboundHooks, listWorkflows, type InboundHook } from "@/lib/api";
import { formatTime } from "@/lib/format";
import { t } from "@/lib/i18n";
import { EmailResendConfirm } from "./EmailResendConfirm";
import { buildActivityFeed, countRecentFailures, useOperateFeedSources, type ActivityItem } from "@/lib/operateFeed";
import { healthLede, type HealthLede } from "@/lib/operateHealth";
import { studioHref } from "@/lib/studioRoutes";

const SHOWN = 20;

const sameDay = (a: Date, b: Date) => a.toDateString() === b.toDateString();
function whenLabel(iso: string, now = new Date()): string {
  const d = new Date(iso);
  const hm = formatTime(d);
  return sameDay(d, now) ? hm : fill(t("studio.operate.auto.feed.yesterday"), { time: hm });
}

/** The sentence of one retry error. 429 and 503 stop the loop; 409 counts as "cannot be resent". */
function retryReason(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.code === "JOB_NOT_RETRYABLE") return t("studio.operate.auto.retry.notRetryable");
    if (e.code === "JOBS_DISABLED") return t("studio.operate.auto.retry.disabled");
    return e.message;
  }
  return t("studio.query.retry");
}

/** From #414: the counts come from GET /admin/health (no "từ 50 trở lên"); background jobs only as "{m} trong 7 ngày". */
function HealthLedeLine({ slug, h }: { slug: string; h: HealthLede }) {
  const link = (route: "lan-chay" | "email" | "cong-nhan" | "viec-nen", text: string, qs?: Record<string, string>) => (
    <Link key={route} href={studioHref(slug, `van-hanh/tu-dong/${route}`, {}, qs)}>
      <b className="tabular-nums">{text}</b>
    </Link>
  );
  const parts = [
    h.parts.runs > 0 && link("lan-chay", fill(t("studio.operate.auto.lede.runs"), { n: h.parts.runs })),
    h.parts.emails > 0 && link("email", fill(t("studio.operate.auto.lede.email"), { n: h.parts.emails }), { "trang-thai": "failed" }),
    h.parts.hooks > 0 && link("cong-nhan", fill(t("studio.operate.auto.lede.hooks"), { n: h.parts.hooks })),
  ].filter(Boolean);
  return (
    <p className="nx-lede">
      {h.n === 0 ? t(h.m > 0 ? "studio.operate.auto.lede.cleanNoJobs" : "studio.operate.auto.lede.clean") : <>{fill(t("studio.operate.auto.lede.head"), { total: h.n })}{parts.flatMap((p, i) => (i ? [", ", p] : [p]))}.</>}
      {h.m > 0 && (
        <>
          {" "}
          {link("viec-nen", fill(t("studio.operate.auto.lede.jobs7"), { m: h.m }))}
          {t("studio.operate.health.lede.in7")}.
        </>
      )}
    </p>
  );
}

function Lede({ slug, sources, ready, health }: { slug: string; sources: Parameters<typeof countRecentFailures>[0]; ready: boolean; health: HealthLede | null }) {
  if (health) return <HealthLedeLine slug={slug} h={health} />;
  if (!ready) return <p className="nx-lede" />;
  const c = countRecentFailures(sources);
  const num = (n: number) => (c.atLeast ? fill(t("studio.operate.auto.lede.atLeast"), { n }) : String(n));
  const part = (n: number, key: "runs" | "jobs" | "email", route: "lan-chay" | "viec-nen" | "email"): ReactNode =>
    n > 0 && (
      <Link key={key} href={studioHref(slug, `van-hanh/tu-dong/${route}`)}>
        <b className="tabular-nums">{fill(t(`studio.operate.auto.lede.${key}` as "studio.operate.auto.lede.runs"), { n: num(n) })}</b>
      </Link>
    );
  const parts = [part(c.runs, "runs", "lan-chay"), part(c.jobs, "jobs", "viec-nen"), part(c.email, "email", "email")].filter(Boolean);
  return (
    <p className="nx-lede">
      {c.unknown.length > 0 && <>{fill(t("studio.operate.auto.lede.unknown"), { sources: c.unknown.join(", ") })} </>}
      {c.clean && t("studio.operate.auto.lede.clean")}
      {parts.length > 0 && (
        <>
          {fill(t("studio.operate.auto.lede.head"), { total: num(c.total) })}
          {parts.flatMap((p, i) => (i ? [", ", p] : [p]))}.
        </>
      )}
    </p>
  );
}

type RetryState =
  | { phase: "idle" }
  | { phase: "confirm"; ids: string[] }
  | { phase: "running"; ids: string[] }
  | { phase: "done"; text: string; tone: "info" | "warning" | "error" };

export function OperateOverview({ slug }: { slug: string }) {
  const qc = useQueryClient();
  const guard = useScopeGate(true);
  const { runs, jobs, email, sources } = useOperateFeedSources(slug);
  const rules = useProjectQuery(slug, ["operate", "rules"], listWorkflows);
  const hooks = useProjectQuery(slug, ["operate", "hooks"], listInboundHooks);
  const keys = useProjectQuery(slug, ["operate", "apikeys"], listApiKeys);
  // #414: when GET /admin/health is fully read its numbers replace the 50-row windows; otherwise the windows (each fail-closed) stay.
  const healthQ = useProjectQuery(slug, ["operate", "health"], getTenantHealth, { retry: false });
  const fromHealth = healthQ.data ? healthLede(healthQ.data, slug) : null;
  // 503 = the server has no master key yet: the vault is simply empty here, not a broken list.
  const creds = useProjectQuery(slug, ["operate", "credentials"], () => listCredentials().catch((e) => (e instanceof ApiError && e.status === 503 ? [] : Promise.reject(e))));
  const [retry, setRetry] = useState<RetryState>({ phase: "idle" });
  const [rowRetry, setRowRetry] = useState<string | null>(null);
  const [resendId, setResendId] = useState<string | null>(null);
  const stop = useRef(false);

  const ready = !runs.isPending && !jobs.isPending && !email.isPending;
  // eslint-disable-next-line react-hooks/exhaustive-deps -- `sources` is rebuilt each render from these three
  const feed = useMemo(() => buildActivityFeed(sources), [runs.data, jobs.data, email.data]);
  const retryable = feed.filter((i) => i.retryable);
  const ruleName = (id: string | null) => (id ? rules.data?.find((w) => w.id === id)?.name ?? t("studio.operate.auto.ruleGone") : null);

  const refreshAll = () => qc.invalidateQueries({ queryKey: ["project", slug, "operate"] });

  async function runRetry(ids: string[]) {
    stop.current = false;
    setRetry({ phase: "running", ids });
    let ok = 0;
    let bad = 0;
    let first = "";
    let halted: "scope" | "rate" | null = null;
    for (const id of ids) {
      // ponytail: the retry API takes no AbortSignal; the gate is read between calls, so at most one call finishes after a stop.
      if (stop.current || guard.signal?.aborted || !guard.assertScope?.()) {
        halted = "scope";
        break;
      }
      try {
        await retryBackgroundJob(id);
        ok++;
      } catch (e) {
        bad++;
        first ||= retryReason(e);
        if (e instanceof ApiError && e.status === 429) {
          halted = "rate";
          break;
        }
        if (e instanceof ApiError && e.code === "JOBS_DISABLED") break;
      }
    }
    if (guard.signal?.aborted) return; // page left: nothing to show
    const n = ids.length;
    void refreshAll();
    if (halted) {
      setRetry({ phase: "done", tone: "warning", text: fill(t(halted === "scope" ? "studio.operate.auto.retry.scope" : "studio.operate.auto.retry.rate"), { ok, n }) });
      return;
    }
    const text = fill(t("studio.operate.auto.retry.done"), { ok, bad, n }) + (first ? ` ${fill(t("studio.operate.auto.retry.firstReason"), { reason: first })}` : "");
    setRetry({ phase: "done", tone: bad ? "warning" : "info", text });
  }

  const showAction = feed.some((i) => i.retryable || i.resendable);
  const columns = useMemo<ColumnDef<ActivityItem>[]>(() => {
    const cols: ColumnDef<ActivityItem>[] = [
      { id: "when", size: 76, header: t("studio.operate.auto.col.when"), cell: ({ row }) => <span className="tabular-nums">{whenLabel(row.original.at)}</span> },
      {
        id: "what",
        header: t("studio.operate.auto.col.what"),
        cell: ({ row: { original: i } }) => {
          const rule = ruleName(i.workflowId);
          const label = [t(`studio.operate.auto.kind.${i.kind}` as "studio.operate.auto.kind.run"), rule].filter(Boolean).join(" · ");
          return (
            <>
              {i.kind === "run" && i.workflowId ? (
                <Link href={studioHref(slug, "van-hanh/tu-dong/lan-chay", {}, { luat: i.workflowId })}>{label}</Link>
              ) : (
                <span>{label}</span>
              )}
              {i.detail && <span className="slds-text-body_small slds-text-color_weak slds-show slds-truncate" title={i.detail}>{i.detail}</span>}
            </>
          );
        },
      },
      {
        id: "result",
        size: 84,
        header: t("studio.operate.auto.col.result"),
        cell: ({ row: { original: i } }) =>
          i.failed ? <StatusText tone="error">{t("studio.operate.auto.result.failed")}</StatusText> : <StatusText tone="success">{t("studio.operate.auto.result.ok")}</StatusText>,
      },
    ];
    if (showAction) {
      cols.push({
        id: "action",
        size: 64,
        header: () => <span className="slds-assistive-text">{t("studio.operate.auto.col.action")}</span>,
        enableSorting: false,
        cell: ({ row: { original: i } }) =>
          i.resendable ? (
            <Button variant="base" aria-label={t("studio.operate.email.resend.row")} onClick={() => setResendId(i.id)}>
              {t("page.emailLog.resend")}
            </Button>
          ) : i.retryable ? (
            <Button
              variant="base"
              aria-label={fill(t("studio.operate.auto.retryRow"), { name: ruleName(i.workflowId) ?? i.id })}
              disabled={retry.phase === "running"}
              onClick={() => setRowRetry(i.id)}
            >
              {t("studio.operate.auto.retry")}
            </Button>
          ) : null,
      });
    }
    return cols;
    // eslint-disable-next-line react-hooks/exhaustive-deps -- ruleName reads rules.data
  }, [slug, showAction, rules.data, retry.phase]);

  const shown = feed.slice(0, SHOWN);
  const resendEntry = resendId ? email.data?.find((e) => e.id === resendId) : undefined;
  const rowConfirm = rowRetry ? feed.find((i) => i.id === rowRetry) : undefined;

  return (
    <div className="slds-p-around_medium">
      <Lede slug={slug} sources={sources} ready={ready} health={fromHealth && fromHealth.unknown.length === 0 ? fromHealth : null} />

      <OperateSection
        title={t("studio.operate.auto.feed.title")}
        seeAll={{ href: studioHref(slug, "van-hanh/tu-dong/lan-chay") }}
        actions={
          retryable.length > 0 && retry.phase === "idle" && !rowRetry ? (
            <Button variant="neutral" onClick={() => setRetry({ phase: "confirm", ids: retryable.map((i) => i.id) })}>
              {fill(t("studio.operate.auto.retryAll"), { n: retryable.length })}
            </Button>
          ) : null
        }
      >
        {retry.phase === "confirm" && (
          <div className="slds-m-bottom_small">
            <InlineConfirm
              tone="warning"
              message={fill(t("studio.operate.auto.retryAll.confirm"), { n: retry.ids.length })}
              confirmLabel={t("studio.operate.auto.retry")}
              onConfirm={() => void runRetry(retry.ids)}
              onCancel={() => setRetry({ phase: "idle" })}
            />
          </div>
        )}
        {retry.phase === "running" && (
          <div className="slds-is-relative slds-m-bottom_small slds-p-around_medium">
            <Spinner size="small" />
            <span className="slds-assistive-text">{t("studio.operate.auto.retry.running")}</span>
          </div>
        )}
        {retry.phase === "done" && (
          <div className="slds-m-bottom_small">
            <ScopedNotification theme={retry.tone}>{retry.text}</ScopedNotification>
          </div>
        )}
        {rowConfirm && (
          <div className="slds-m-bottom_small">
            <InlineConfirm
              tone="warning"
              message={t("studio.operate.auto.retryOne.confirm")}
              confirmLabel={t("studio.operate.auto.retry")}
              onConfirm={() => {
                setRowRetry(null);
                void runRetry([rowConfirm.id]);
              }}
              onCancel={() => setRowRetry(null)}
            />
          </div>
        )}
        {resendEntry && (
          <div className="slds-m-bottom_small">
            <EmailResendConfirm entry={resendEntry} onClose={() => setResendId(null)} onChanged={() => void refreshAll()} />
          </div>
        )}
        {[runs, jobs, email].some((q) => q.isError) && (
          <div className="slds-m-bottom_small">
            <ScopedNotification theme="error">{t("studio.operate.auto.feed.error")}</ScopedNotification>
          </div>
        )}
        <QueryState
          query={{ isPending: !ready, isError: false, error: null, data: feed, refetch: () => undefined }}
          empty={<p className="slds-text-color_weak">{t("studio.operate.auto.feed.empty")}</p>}
        >
          <DataTable label={t("studio.operate.auto.feed.title")} columns={columns} data={shown} />
        </QueryState>
      </OperateSection>

      <OperateSection title={t("studio.operate.auto.conn.title")}>
        <Connections slug={slug} hooks={hooks} keys={keys} creds={creds} />
      </OperateSection>
    </div>
  );
}

type Conn = { id: string; name: string; sub: string; status: ReactNode; href: string };

type Src<T> = { data?: T[]; isPending: boolean; isError: boolean; error: unknown; refetch: () => unknown };

function Connections({ slug, hooks, keys, creds }: { slug: string; hooks: Src<InboundHook>; keys: Src<ApiKey>; creds: Src<NamedCredential> }) {
  const rows: Conn[] = [
    ...(hooks.data ?? []).map((h) => ({
      id: `h-${h.id}`,
      name: h.name,
      sub: fill(t("studio.operate.auto.conn.hook"), { entity: h.entityName }),
      status: h.isActive ? <StatusText tone="success">{t("studio.operate.auto.conn.on")}</StatusText> : <StatusText tone="weak">{t("studio.operate.auto.conn.off")}</StatusText>,
      href: studioHref(slug, "van-hanh/tu-dong/cong-nhan"),
    })),
    ...(keys.data ?? []).map((k) => ({
      id: `k-${k.id}`,
      name: k.name,
      sub: fill(t("studio.operate.auto.conn.key"), { prefix: k.keyPrefix }),
      status: k.revokedAt ? <StatusText tone="weak">{t("studio.operate.auto.conn.revoked")}</StatusText> : <StatusText tone="success">{t("studio.operate.auto.conn.on")}</StatusText>,
      href: studioHref(slug, "van-hanh/tu-dong/khoa-truy-cap"),
    })),
    ...(creds.data ?? []).map((c) => ({
      id: `c-${c.id}`,
      name: c.label || c.name,
      sub: c.rotatedAt ? fill(t("studio.operate.auto.conn.cred"), { at: whenLabel(c.rotatedAt) }) : t("studio.operate.auto.conn.credNever"),
      status: <StatusText tone={c.status === "ready" ? "success" : c.status === "key_unavailable" ? "error" : "warn"}>{t(`page.credentials.status.${c.status}` as "page.credentials.status.ready").replace("{time}", c.previousExpiresAt ? whenLabel(c.previousExpiresAt) : "")}</StatusText>,
      href: studioHref(slug, "van-hanh/tu-dong/khoa-ket-noi"),
    })),
  ];
  const all = [hooks, keys, creds];
  const q = { isPending: all.some((x) => x.isPending), isError: all.some((x) => x.isError) && rows.length === 0, error: all.find((x) => x.error)?.error, data: rows, refetch: () => { all.forEach((x) => void x.refetch()); } };
  return (
    <QueryState query={q} empty={<p className="slds-text-color_weak">{t("studio.operate.auto.conn.empty")}</p>}>
      {all.some((x) => x.isError) && <p className="slds-text-color_error slds-m-bottom_x-small" role="alert">{t("studio.operate.auto.conn.partial")}</p>}
      <ul className="slds-has-dividers_bottom">
        {rows.map((r) => (
          <li key={r.id} className="slds-item slds-grid slds-grid_vertical-align-center">
            <div className="slds-col slds-has-flexi-truncate">
              <strong className="slds-show">{r.name}</strong>
              <span className="slds-text-body_small slds-text-color_weak slds-show">{r.sub}</span>
            </div>
            <div className="slds-m-horizontal_medium">{r.status}</div>
            <Link href={r.href} aria-label={fill(t("studio.operate.auto.conn.openOf"), { name: r.name })}>
              {t("studio.operate.auto.conn.open")}
            </Link>
          </li>
        ))}
      </ul>
    </QueryState>
  );
}
