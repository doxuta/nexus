"use client";

// WorkflowRunHistory (W6 §3.4) — per-rule run history + tenant-wide recent-runs strip, on SLDS
// (specs/038 T043). GET /admin/workflows/:id/runs (paged, newest-first) · GET /admin/workflows/runs.
// Every run the engine evaluated is here — success / failed / skipped — the "is my rule alive?" trust
// gap all 7 benchmark tools cover. Blueprints: `data-tables` (the runs grid), `cards` (the opened
// run), `scoped-notifications` (run error + action warnings), `split-view` list items (recent runs).
// Queue #106: a `business_hours` deadline that fell back to wall-clock time still ends `success`, and
// actionsResult[].warning is its only trace (api-conventions §9.3) — the row flags it and the detail
// prints the sentence verbatim with the warning marker (actionResultTone).
// Shaping logic (status meta, actionsResult union, durations) lives in lib/workflowRuns.ts.

import { useCallback, useEffect, useId, useMemo, useState } from "react";
import Link from "next/link";
import type { ColumnDef } from "@tanstack/react-table";
import { CircleCheck, CircleMinus, CircleX, Clock, History, RotateCw, TriangleAlert, type LucideIcon } from "lucide-react";
import { listWorkflowRuns, listRecentWorkflowRuns, type WorkflowSummary } from "@/lib/api";
import { errorMessage } from "@/lib/errorMessages";
import {
  runStatusMeta, triggerLabel, shapeActionsResult, relTime, runDurationMs, fmtDuration,
  actionResultTone, actionStateLabel, actionTitle, skipReasonText, type ActionResultEntry, type RunTone, type WorkflowRun,
} from "@/lib/workflowRuns";
import { isBeforeTrigger } from "@/lib/workflowBefore";
import { formatDateTime } from "@/lib/format";
import { t } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Card } from "@/components/slds/Card";
import { DataTable, fill } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { Icon } from "@/components/slds/Icon";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { useEmbedded } from "@/components/slds/EmbeddedSurface";
import { StatusText } from "@/components/studio/StatusText";
import { useProjectEntities } from "@/lib/operateData";
import { projectSlugAt } from "@/lib/studioContext";
import { studioHref } from "@/lib/studioRoutes";
import { usePathname } from "next/navigation";

const PAGE = 20;

// Status → glyph + icon tone (run status: ac/dg/neu; action result: ok/wn/dg).
const TONE: Record<RunTone, { glyph: LucideIcon; tone?: "success" | "warning" | "error" }> = {
  ac: { glyph: CircleCheck, tone: "success" },
  ok: { glyph: CircleCheck, tone: "success" },
  wn: { glyph: TriangleAlert, tone: "warning" },
  dg: { glyph: CircleX, tone: "error" },
  neu: { glyph: CircleMinus },
  pn: { glyph: Clock },
};

const hasWarning = (a: ActionResultEntry) => actionResultTone(a) === "wn";

function runWarned(run: WorkflowRun): boolean {
  const r = shapeActionsResult(run.actionsResult);
  return (r.kind === "actions" && r.actions.some(hasWarning)) || (r.kind === "scheduled" && r.warned > 0);
}

// A run without a record id is one of three things: a before rule that refused a CREATE (the record
// was never made), a before rule's summary over a whole batch, or a scheduled scan.
function noRecordLabel(run: WorkflowRun): string {
  if (shapeActionsResult(run.actionsResult).kind === "batch") return t("workflow.runs.batchRows");
  if (isBeforeTrigger(run.triggerType)) return t("workflow.runs.newRecord");
  return t("page.workflow.runs.scheduledScan");
}

// Embedded in the Studio frame (specs/078 T108): the record id is a link inside the frame, and a table that no longer
// exists says so instead of leading to a dead page.
function EmbeddedRecordLink({ entityName, recordId }: { entityName?: string; recordId: string }) {
  const slug = projectSlugAt(usePathname());
  const entities = useProjectEntities(slug ?? "");
  const short = <span className="slds-text-font_monospace" title={recordId}>{recordId.slice(0, 8)}</span>;
  if (!slug || !entityName) return short;
  if (entities.data && !entities.data.some((e) => e.name === entityName)) {
    return <span className="slds-text-color_weak">{t("studio.operate.auto.tableGone")}</span>;
  }
  return <Link href={studioHref(slug, "van-hanh/du-lieu/:bang/:id", { bang: entityName, id: recordId })}>{short}</Link>;
}

// ── Per-rule run history panel ────────────────────────────────────────────────
export function WorkflowRunHistory({
  workflowId,
  entityName,
  openRunId,
}: {
  workflowId: string;
  /** Rule's entity — lets record ids link to /:entity/:id. */
  entityName?: string;
  /** Run to show opened on arrival (the list's "last run failed" mark, #337). */
  openRunId?: string | null;
}) {
  const [runs, setRuns] = useState<WorkflowRun[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [open, setOpen] = useState<string | null>(openRunId ?? null);
  const detailId = useId();
  const embedded = useEmbedded();

  const load = useCallback(async (reset: boolean, offset: number) => {
    if (reset) { setLoading(true); setErr(null); } else { setLoadingMore(true); }
    try {
      const page = await listWorkflowRuns(workflowId, PAGE, offset);
      setTotal(page.total);
      setRuns((prev) => (reset ? page.runs : [...prev, ...page.runs]));
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setLoading(false);
      setLoadingMore(false);
    }
  }, [workflowId]);

  // First page. The parent keys this panel by rule id, so a new rule mounts fresh (loading, nothing
  // opened) and no state is reset here; "Làm mới" / "Tải thêm" go through load().
  useEffect(() => {
    let alive = true;
    listWorkflowRuns(workflowId, PAGE, 0).then(
      (page) => {
        if (!alive) return;
        setTotal(page.total);
        setRuns(page.runs);
        setLoading(false);
      },
      (e) => {
        if (!alive) return;
        setErr(errorMessage(e));
        setLoading(false);
      },
    );
    return () => { alive = false; };
  }, [workflowId]);

  // The detail card sits under the grid (the kit table has no expandable rows), up to 20+ rows below
  // the clicked one — bring it into view so opening a run visibly does something.
  useEffect(() => {
    if (open) document.getElementById(detailId)?.scrollIntoView?.({ block: "nearest" });
  }, [open, detailId]);

  const columns = useMemo<ColumnDef<WorkflowRun>[]>(() => [
    {
      id: "startedAt",
      header: t("page.workflow.runs.col.started"),
      cell: ({ row }) => {
        const r = row.original;
        return (
          <Button
            variant="base"
            aria-expanded={open === r.id}
            aria-controls={open === r.id ? detailId : undefined}
            title={formatDateTime(r.startedAt)}
            onClick={() => setOpen((cur) => (cur === r.id ? null : r.id))}
          >
            {relTime(r.startedAt)}
          </Button>
        );
      },
    },
    {
      id: "status",
      header: t("page.workflow.runs.col.status"),
      cell: ({ row }) => {
        const meta = runStatusMeta(row.original.status, row.original.actionsResult);
        return (
          <span className="inline-flex items-center gap-1">
            {embedded ? (
              <StatusText tone={meta.tone === "ac" ? "success" : meta.tone === "dg" ? "error" : "weak"}>{meta.label}</StatusText>
            ) : (
              <Badge theme={meta.tone === "ac" ? "success" : meta.tone === "dg" ? "error" : undefined}>{meta.label}</Badge>
            )}
            {runWarned(row.original) && (
              <Icon icon={TriangleAlert} size="x-small" tone="warning" label={t("page.workflow.runs.warned")} />
            )}
          </span>
        );
      },
    },
    {
      id: "trigger",
      header: t("page.workflow.runs.col.trigger"),
      cell: ({ row }) => triggerLabel(row.original.triggerType),
    },
    {
      id: "record",
      header: t("page.workflow.runs.col.record"),
      cell: ({ row }) => {
        const r = row.original;
        if (!r.recordId) return <span className="slds-text-color_weak">{noRecordLabel(r)}</span>;
        if (embedded) return <EmbeddedRecordLink entityName={entityName} recordId={r.recordId} />;
        return <span className="slds-text-font_monospace" title={r.recordId}>{r.recordId.slice(0, 8)}</span>;
      },
    },
    {
      id: "duration",
      header: t("page.workflow.runs.col.duration"),
      meta: { numeric: true },
      cell: ({ row }) => fmtDuration(runDurationMs(row.original)),
    },
  ], [open, detailId, embedded, entityName]);

  if (loading) {
    return (
      <div className="flex items-center gap-2 py-8">
        <Spinner size="x-small" inline label={t("page.workflow.runs.loading")} />
        <span className="slds-text-color_weak">{t("page.workflow.runs.loading")}</span>
      </div>
    );
  }

  if (err) return <ScopedNotification theme="error">{err}</ScopedNotification>;

  if (runs.length === 0) {
    return (
      <EmptyState
        icon={History}
        title={t("page.workflow.runs.emptyTitle")}
        description={t("page.workflow.runs.emptyDesc")}
        className="py-14"
      />
    );
  }

  const opened = runs.find((r) => r.id === open) ?? null;

  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-center justify-between">
        <p className="slds-text-color_weak">{fill(t("page.workflow.runs.count"), { n: total })}</p>
        <ButtonIcon icon={RotateCw} label={t("page.workflow.runs.refresh")} variant="border-filled" onClick={() => load(true, 0)} />
      </div>

      <DataTable label={t("page.workflow.runs.grid")} columns={columns} data={runs} />

      {runs.length < total && (
        <div className="flex justify-center">
          <Button onClick={() => load(false, runs.length)} disabled={loadingMore}>
            {loadingMore ? t("page.workflow.runs.loadingMore") : fill(t("page.workflow.runs.loadMore"), { n: total - runs.length })}
          </Button>
        </div>
      )}

      {opened && <RunDetail id={detailId} run={opened} entityName={entityName} />}
    </div>
  );
}

// The opened run: blueprint `cards` with a horizontal description list, the run error, and one list
// item per action (status glyph · type · params · warning / error).
function RunDetail({ id, run, entityName }: { id: string; run: WorkflowRun; entityName?: string }) {
  const embedded = useEmbedded();
  const result = shapeActionsResult(run.actionsResult);
  const title = fill(t("page.workflow.runs.detailTitle"), { when: formatDateTime(run.startedAt) });
  return (
    <section id={id} aria-label={title}>
      <Card title={title} icon={{ glyph: History, name: "flow" }}>
        <div className="flex flex-col gap-3">
          <dl className="slds-list_horizontal slds-wrap">
            <dt className="slds-item_label slds-text-color_weak">{t("page.workflow.runs.started")}</dt>
            <dd className="slds-item_detail slds-text-font_monospace">{formatDateTime(run.startedAt)}</dd>
            <dt className="slds-item_label slds-text-color_weak">{t("page.workflow.runs.finished")}</dt>
            <dd className="slds-item_detail slds-text-font_monospace">{formatDateTime(run.finishedAt)}</dd>
          </dl>
          {run.recordId && entityName && embedded && <p><EmbeddedRecordLink entityName={entityName} recordId={run.recordId} /></p>}
          {run.recordId && entityName && !embedded && (
            <Link href={`/${entityName}/${run.recordId}`}>{t("page.workflow.runs.openRecord")}</Link>
          )}

          {run.error && <ScopedNotification theme="error">{run.error}</ScopedNotification>}

          {result.kind === "actions" && (
            <ul className="slds-has-dividers_around-space">
              {result.actions.map((a, i) => {
                const tone = TONE[actionResultTone(a)];
                return (
                  <li key={i} className="slds-item">
                    <div className="slds-media">
                      <div className="slds-media__figure">
                        <Icon icon={tone.glyph} size="x-small" tone={tone.tone} label={t(`page.workflow.runs.tone.${actionResultTone(a)}` as I18nKey)} />
                      </div>
                      <div className="slds-media__body flex flex-col gap-1">
                        <p className="slds-text-title_bold">{actionTitle(a.type)}</p>
                        {actionStateLabel(a, run.status) && (
                          <p className="slds-text-body_small slds-text-color_weak">{actionStateLabel(a, run.status)}</p>
                        )}
                        {Object.entries(a)
                          .filter(([k]) => k !== "type" && k !== "ok" && k !== "error" && k !== "warning" && k !== "delivery")
                          .map(([k, v]) => (
                            <p key={k} className="slds-text-body_small slds-truncate">
                              <span className="slds-text-color_weak">{k === "skipped" ? t("workflow.runs.skippedKey") : k}: </span>
                              <span className={k === "skipped" ? undefined : "slds-text-font_monospace"}>
                                {k === "skipped" ? skipReasonText(v) : typeof v === "string" ? v : JSON.stringify(v)}
                              </span>
                            </p>
                          ))}
                        {hasWarning(a) && <ScopedNotification theme="warning">{a.warning as string}</ScopedNotification>}
                        {typeof a.error === "string" && a.error !== "" && (
                          <p className="slds-text-color_error slds-text-font_monospace slds-hyphenate">{a.error}</p>
                        )}
                      </div>
                    </div>
                  </li>
                );
              })}
            </ul>
          )}

          {result.kind === "batch" && (
            <>
              <dl className="slds-list_horizontal slds-wrap">
                <dt className="slds-item_label slds-text-color_weak">{t("workflow.runs.batch.blocked")}</dt>
                <dd className={result.blocked > 0 ? "slds-item_detail tabular-nums slds-text-color_error" : "slds-item_detail tabular-nums"}>{result.blocked}</dd>
                <dt className="slds-item_label slds-text-color_weak">{t("workflow.runs.batch.applied")}</dt>
                <dd className="slds-item_detail tabular-nums">{result.applied}</dd>
                <dt className="slds-item_label slds-text-color_weak">{t("workflow.runs.batch.failed")}</dt>
                <dd className="slds-item_detail tabular-nums">{result.failed}</dd>
              </dl>
              <p className="slds-text-body_small slds-text-color_weak">{t("workflow.runs.batch.hint")}</p>
            </>
          )}

          {result.kind === "scheduled" && (
            <dl className="slds-list_horizontal slds-wrap">
              <dt className="slds-item_label slds-text-color_weak">{t("page.workflow.runs.scanned")}</dt>
              <dd className="slds-item_detail tabular-nums">{result.scanned}</dd>
              <dt className="slds-item_label slds-text-color_weak">{t("page.workflow.runs.matched")}</dt>
              <dd className="slds-item_detail tabular-nums">{result.matched}</dd>
              <dt className="slds-item_label slds-text-color_weak">{t("page.workflow.runs.failed")}</dt>
              <dd className={result.failed > 0 ? "slds-item_detail tabular-nums slds-text-color_error" : "slds-item_detail tabular-nums"}>
                {result.failed}
              </dd>
              {result.webhooksHeld > 0 && (
                <>
                  <dt className="slds-item_label slds-text-color_weak" title={t("page.workflow.runs.webhooksHeldHint")}>
                    <Icon icon={TriangleAlert} size="x-small" tone="warning" label={t("page.workflow.runs.webhooksHeldHint")} />{" "}
                    {t("page.workflow.runs.webhooksHeld")}
                  </dt>
                  <dd className="slds-item_detail tabular-nums">{result.webhooksHeld}</dd>
                </>
              )}
              {result.warned > 0 && (
                <>
                  <dt className="slds-item_label slds-text-color_weak">
                    <Icon icon={TriangleAlert} size="x-small" tone="warning" />{" "}
                    {t("page.workflow.runs.warnedActions")}
                  </dt>
                  <dd className="slds-item_detail tabular-nums">{result.warned}</dd>
                </>
              )}
            </dl>
          )}
          {/* Queue #105: the sentences the scan's actions warned with, verbatim (same as an event run). */}
          {result.kind === "scheduled" && result.warnings.map((w) => (
            <ScopedNotification key={w} theme="warning">{w}</ScopedNotification>
          ))}

          {result.kind === "none" && !run.error && (
            <p className="slds-text-color_weak">
              {run.status === "skipped" ? t("page.workflow.runs.skippedNone") : t("page.workflow.runs.none")}
            </p>
          )}
        </div>
      </Card>
    </section>
  );
}

// ── Recent-runs strip (workflows screen, tenant-wide) ────────────────────────
// Compact entry point under the rule list: last N runs across ALL rules, as split-view list items.
export function RecentRunsStrip({
  workflows,
  workflowIds,
  onOpen,
}: {
  workflows: WorkflowSummary[];
  /** Only these rules' runs — the automation panel of one app (queue #297). Undefined = every rule. */
  workflowIds?: string[];
  onOpen: (workflowId: string) => void;
}) {
  // Runs are kept with the rule list they were read for, so a scope change shows "Đang tải…"
  // instead of the previous app's runs until the new read lands.
  const idsKey = workflowIds ? workflowIds.join(",") : "*";
  const [fetched, setFetched] = useState<{ key: string; runs: WorkflowRun[] } | null>(null);
  const runs = fetched?.key === idsKey ? fetched.runs : null;

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const rs = await listRecentWorkflowRuns(8, workflowIds);
        if (!cancelled) setFetched({ key: idsKey, runs: rs });
      } catch {
        // Non-fatal side strip — hide on error instead of shouting.
        if (!cancelled) setFetched({ key: idsKey, runs: [] });
      }
    })();
    return () => { cancelled = true; };
  }, [idsKey]); // eslint-disable-line react-hooks/exhaustive-deps -- workflowIds is read through idsKey

  const nameOf = (id: string) => workflows.find((w) => w.id === id)?.name ?? `#${id.slice(0, 8)}`;

  return (
    <section className="slds-border_top" aria-labelledby="nx-wf-recent">
      <h2 id="nx-wf-recent" className="slds-text-title_caps slds-p-horizontal_small slds-p-top_small slds-p-bottom_xx-small">
        {t("page.workflow.recent.title")}
      </h2>
      {runs === null ? (
        <p className="slds-text-body_small slds-text-color_weak slds-p-horizontal_small slds-p-bottom_small">{t("page.workflow.recent.loading")}</p>
      ) : runs.length === 0 ? (
        <p className="slds-text-body_small slds-text-color_weak slds-p-horizontal_small slds-p-bottom_small">{t("page.workflow.recent.empty")}</p>
      ) : (
        // Capped so the rule list above keeps the room: eight split-view rows would push it off screen.
        <ul className="slds-scrollable_y max-h-56">
          {runs.map((r) => {
            const meta = runStatusMeta(r.status, r.actionsResult);
            const tone = TONE[meta.tone];
            return (
              <li key={r.id} className="slds-split-view__list-item">
                <button
                  type="button"
                  onClick={() => onOpen(r.workflowId)}
                  title={`${meta.label} · ${formatDateTime(r.startedAt)}`}
                  className="slds-split-view__list-item-action slds-button_reset slds-grow slds-has-flexi-truncate slds-text-align_left"
                >
                  <span className="flex items-center gap-2">
                    <Icon icon={tone.glyph} size="xx-small" tone={tone.tone} label={meta.label} />
                    <span className="slds-truncate slds-text-body_small slds-text-color_default">{nameOf(r.workflowId)}</span>
                    <span className="slds-text-body_small slds-col_bump-left shrink-0">{relTime(r.startedAt)}</span>
                  </span>
                </button>
              </li>
            );
          })}
        </ul>
      )}
    </section>
  );
}
