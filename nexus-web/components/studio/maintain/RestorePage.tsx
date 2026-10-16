"use client";

// RestorePage — "Khôi phục về bản sao lưu lúc …" (bao-tri/sao-luu/khoi-phuc/:id[?lan=]; specs/079 f7-khoi-phuc-ke-hoach
// + f7-khoi-phuc-chay). Three phases on ONE route in one Card: the plan (dry run, writes nothing), the progress of the
// run and its report. `?lan=<restoreId>` decides the phase, so a reload in the middle of a restore keeps watching it.
// tenantId and the project code come from useBackupTenant (never the URL); nothing per-tenant is asked before it is
// `ok` (FR-101, FR-002a). The plan is never cached: it is recomputed on every visit.
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { Button } from "@/components/slds/Button";
import { Card } from "@/components/slds/Card";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { ApiError, enterWorkspace, getTenantBackup, previewTenantRestore, startTenantRestore, type RestorePlan, type TenantBackup } from "@/lib/api";
import { isEngineMissing } from "@/lib/engineGap";
import { formatDateTime } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import { maintainErrorText } from "@/lib/maintainErrors";
import { restoreFailureText, restoreLock } from "@/lib/maintenance";
import { studioHref } from "@/lib/studioRoutes";
import { ConfirmPrompt, type ImpactRow } from "../workspaceShared";
import { PROJECTS_QUERY_KEY } from "../ProjectSettings";
import { RestorePlanView } from "./RestorePlanView";
import { RestoreProgress } from "./RestoreProgress";
import { RestoreReportView } from "./RestoreReportView";
import { useBackupTenant, type BackupTenant } from "./useBackupTenant";

const POLL_MS = 2000;
const ROUTE = "bao-tri/sao-luu/khoi-phuc/:id" as const;
const n = (v: number) => v.toLocaleString("vi-VN");

const Loading = () => (
  <div className="flex items-center gap-2"><Spinner size="x-small" inline /><span className="slds-text-color_weak">{t("studio.maintain.restore.loading")}</span></div>
);

export function RestorePage({ project, backupId, lan }: { project: string; backupId: string; lan?: string }) {
  const tenant = useBackupTenant(project);
  let body: ReactNode;
  if (tenant.state === "loading") body = <Loading />;
  else if (tenant.state === "missing") body = <ScopedNotification theme="warning">{t("studio.maintain.restore.noProject")}</ScopedNotification>;
  else if (tenant.state === "error") body = <ScopedNotification theme="error">{t("studio.maintain.restore.projectsFailed")}</ScopedNotification>;
  else body = lan ? <RunPhase key={`${tenant.tenantId}|${lan}`} bt={tenant} backupId={backupId} lan={lan} /> : <PlanPhase key={`${tenant.tenantId}|${backupId}`} bt={tenant} backupId={backupId} />;
  return (
    <div className="slds-p-around_medium" style={{ maxWidth: "56rem" }}>
      <p className="slds-m-bottom_small">
        <Link href={studioHref(project, "bao-tri/sao-luu")}>{`← ${t("studio.route.backups")}`}</Link>
      </p>
      <Card title={t("studio.route.restore")} hideHeader>{body}</Card>
    </div>
  );
}

function Title({ children, focusRef }: { children: ReactNode; focusRef?: React.Ref<HTMLHeadingElement> }) {
  return <h2 ref={focusRef} tabIndex={-1} className="slds-text-heading_medium slds-m-bottom_small">{children}</h2>;
}

// ── Phase 1: the plan, and the button that starts the run ─────────────────────────────────────────────────────────
function PlanPhase({ bt, backupId }: { bt: BackupTenant; backupId: string }) {
  const router = useRouter();
  const tenantId = bt.tenantId as string;
  const plan = useQuery({
    queryKey: ["project", bt.slug, tenantId, "restore-plan", backupId],
    queryFn: () => previewTenantRestore(tenantId, backupId),
    staleTime: 0,
    gcTime: 0,
    refetchOnMount: "always",
    refetchOnWindowFocus: false,
    retry: false,
  });
  // The prompt belongs to the scope it was opened in; a changed project or backup closes it by construction (FR-101a).
  const scope = `${bt.slug}|${tenantId}|${backupId}`;
  const [askedIn, setAskedIn] = useState<string | null>(null);
  const [starting, setStarting] = useState(false);
  const [problem, setProblem] = useState<string | null>(null);
  const live = useRef(scope);
  useEffect(() => { live.current = scope; }, [scope]);
  const asking = askedIn === scope;

  async function start() {
    const mine = scope;
    setStarting(true);
    setProblem(null);
    try {
      const r = await startTenantRestore(tenantId, backupId, bt.slug);
      if (live.current !== mine) return;
      setAskedIn(null);
      router.replace(studioHref(bt.slug, ROUTE, { id: backupId }, { lan: r.restoreId }));
    } catch (e) {
      if (live.current === mine) setProblem(maintainErrorText(e));
    } finally {
      if (live.current === mine) setStarting(false);
    }
  }

  if (plan.isPending) return <Loading />;
  if (plan.isError) {
    // A refusal the server named (4xx) will answer the same again: "Thử lại" only for a dropped line or a 5xx.
    return (
      <>
        <ScopedNotification theme={isEngineMissing(plan.error) ? "info" : "error"}>
          {isEngineMissing(plan.error) ? t("studio.maintain.restore.missing") : maintainErrorText(plan.error)}
        </ScopedNotification>
        {!(plan.error instanceof ApiError && plan.error.status < 500) && (
          <div className="slds-m-top_small"><Button variant="neutral" onClick={() => void plan.refetch()}>{t("studio.query.retry")}</Button></div>
        )}
      </>
    );
  }
  const p = plan.data;
  const when = formatDateTime(p.backupCreatedAt);
  const can = p.integrity.ok && p.schema.ok;
  return (
    <>
      <Title>{tf("studio.maintain.restore.title", { time: when })}</Title>
      <RestorePlanView plan={p} />
      {can && (
        <div className="slds-m-top_medium">
          <Button variant="destructive" disabled={starting} onClick={() => { setProblem(null); setAskedIn(scope); }}>{t("studio.maintain.restore.action")}</Button>
        </div>
      )}
      <ConfirmPrompt
        open={asking}
        onOpenChange={(o) => setAskedIn(o ? scope : null)}
        title={tf("studio.maintain.restore.confirm.title", { name: bt.name ?? bt.slug, time: when })}
        description={
          <>
            {t("studio.maintain.restore.confirm.desc")}
            {problem && <span className="slds-m-top_small" style={{ display: "block" }}><ScopedNotification theme="error">{problem}</ScopedNotification></span>}
          </>
        }
        impact={impactOf(p, when)}
        confirmName={bt.slug}
        danger
        confirmLabel={t("studio.maintain.restore.confirm.label")}
        loading={starting}
        onConfirm={() => void start()}
      />
    </>
  );
}

function impactOf(p: RestorePlan, when: string): ImpactRow[] {
  const rows = (p.groups ?? []).reduce((a, g) => a + g.tables.reduce((b, tb) => b + tb.rowsInBackup, 0), 0);
  const out: ImpactRow[] = [{ label: tf("studio.maintain.restore.confirm.rows", { rows: n(rows) }) }];
  // accessChanges missing or not a number is NOT "no change" (FR-003 closed-when-missing): the prompt says it does not know.
  if (!p.accessChanges || typeof p.accessChanges.total !== "number") out.push({ label: t("studio.maintain.restore.confirm.widenedUnknown"), tone: "warn" });
  else if (p.accessChanges.total > 0) out.push({ label: tf("studio.maintain.restore.confirm.widened", { n: n(p.accessChanges.total) }), tone: "warn" });
  out.push({ label: tf("studio.maintain.restore.confirm.history", { time: when }) }, { label: t("studio.maintain.restore.confirm.undo") });
  return out;
}

// ── Phases 2-3: progress and report, read from the pre_restore row named by ?lan= ─────────────────────────────────
function RunPhase({ bt, backupId, lan }: { bt: BackupTenant; backupId: string; lan: string }) {
  const tenantId = bt.tenantId as string;
  const qc = useQueryClient();
  const run = useQuery({
    queryKey: ["project", bt.slug, tenantId, "restore-run", lan],
    queryFn: () => getTenantBackup(tenantId, lan),
    staleTime: 0,
    gcTime: 0,
    retry: false,
    refetchOnWindowFocus: false,
    // Server-written status decides; a dropped poll keeps the last data, so the interval keeps going. React Query
    // pauses the interval in a hidden tab (refetchIntervalInBackground false) and never has two requests in flight.
    refetchInterval: (q) => (q.state.data?.restore?.status === "running" ? POLL_MS : false),
    refetchIntervalInBackground: false,
  });
  const heading = useRef<HTMLHeadingElement>(null);
  // The heading only exists once the first answer is in (Loading before), so focus when that happens, not at mount.
  const loaded = !run.isPending;
  useEffect(() => { if (loaded) heading.current?.focus(); }, [loaded]);
  const status = run.data?.restore?.status;
  const ended = status === "done" || status === "failed";
  useEffect(() => {
    if (!ended) return;
    void qc.invalidateQueries({ queryKey: [...PROJECTS_QUERY_KEY] });
    void qc.invalidateQueries({ queryKey: ["projects"] });
    // The list of backups gains the pre_restore row ("Hoàn tác" entry point); its 2-minute staleTime must not hide it.
    void qc.invalidateQueries({ queryKey: ["home", bt.slug, tenantId, "backups"] });
  }, [ended, qc, bt.slug, tenantId]);

  const planHref = studioHref(bt.slug, ROUTE, { id: backupId });
  const notFound = (
    <>
      <Title focusRef={heading}>{t("studio.maintain.restore.notFound")}</Title>
    </>
  );
  if (run.isPending) return <Loading />;
  // A refetch that fails keeps the last data (status "error" but data set): only an error with nothing to show ends the view.
  if (!run.data) {
    if (run.error instanceof ApiError && run.error.code === "BACKUP_NOT_FOUND") return notFound;
    return (
      <>
        <ScopedNotification theme="error">{maintainErrorText(run.error)}</ScopedNotification>
        <div className="slds-m-top_small"><Button variant="neutral" onClick={() => void run.refetch()}>{t("studio.query.retry")}</Button></div>
      </>
    );
  }
  const row: TenantBackup = run.data;
  const r = row.restore;
  // Only the pre_restore row of THIS backup's restore is shown here; any other id in ?lan= is not a restore of this page.
  if (row.kind !== "pre_restore" || !r || r.sourceBackupId !== backupId) return notFound;

  const duration = r.finishedAt ? new Date(r.finishedAt).getTime() - new Date(row.createdAt).getTime() : undefined;
  return (
    <div className="flex flex-col gap-3">
      <Title focusRef={heading}>{t("studio.maintain.restore.progress")}</Title>
      <div aria-live="polite" className="slds-assistive-text">{status === "done" ? t("studio.maintain.restore.doneLive") : ""}</div>
      {run.isRefetchError && status === "running" && <ScopedNotification theme="warning">{t("studio.maintain.restore.pollFailed")}</ScopedNotification>}
      {status === "running" && <ScopedNotification theme="info">{t("studio.maintain.restore.running")}</ScopedNotification>}
      {status === "failed" && (
        <ScopedNotification theme="error">
          {r.committedAt && <><strong>{t("studio.maintain.restore.failure.partial")}</strong>{" "}</>}
          {restoreFailureText(r, row.error?.code)}
        </ScopedNotification>
      )}
      <RestoreProgress restore={r} />
      {r.status === "done" && r.report && <RestoreReportView report={r.report} durationMs={duration} />}
      {r.status === "done" && (
        <div className="flex flex-wrap items-center gap-3">
          <Button variant="brand" onClick={() => void enterWorkspace(bt.slug, { next: studioHref(bt.slug, "van-hanh/du-lieu") })}>{t("studio.maintain.restore.openProject")}</Button>
          <Link href={studioHref(bt.slug, ROUTE, { id: lan })}>{t("studio.maintain.restore.undoRun")}</Link>
        </div>
      )}
      {r.status === "failed" && !r.committedAt && (
        <div className="flex flex-wrap items-center gap-3"><Link href={planHref}>{t("studio.maintain.restore.failure.reviewPlan")}</Link></div>
      )}
      {r.status === "failed" && r.committedAt && (
        <div className="flex flex-wrap items-center gap-3">
          {restoreLock(row) === null ? (
            <Link className="slds-button slds-button_brand" href={studioHref(bt.slug, ROUTE, { id: lan })}>{t("studio.maintain.restore.failure.undoPrior")}</Link>
          ) : (
            <span>{tf("studio.maintain.restore.failure.priorUnusable", { reason: restoreLock(row) as string })}</span>
          )}
        </div>
      )}
    </div>
  );
}
