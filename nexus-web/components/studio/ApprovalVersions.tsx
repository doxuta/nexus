"use client";

// FE-BPM-VERSIONS (specs/043 T234-T237): the three pieces ApprovalBuilder mounts around a saved process —
// the version history tab, the "Hồ sơ đang chạy" tab (stuck hồ sơ: Thử lại / Huỷ / Quyết thay) and the
// modal that moves waiting hồ sơ to the active version (dry run first, then apply). All data comes from
// the admin endpoints; nothing is mocked. Strings go through lib/i18n.
import { useCallback, useEffect, useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import {
  ApiError, getApprovalVersion, listApprovalProcessInstances, listApprovalVersions, overrideApproval,
  repinApprovalInstances, retryApprovalInstance, cancelApprovalInstance,
  type ApprovalInstanceRow, type ApprovalRepinOutcome, type ApprovalVersionRow, type ApprovalStep,
} from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import { Button } from "@/components/slds/Button";
import { Checkbox } from "@/components/slds/Checkbox";
import { DataTable } from "@/components/slds/DataTable";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { Textarea } from "@/components/slds/Textarea";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { StatusText } from "@/components/studio/StatusText";

const msg = (e: unknown, fallback: string) => (e instanceof ApiError || e instanceof Error ? e.message : fallback) || fallback;

/** Short id for a hồ sơ in a row label (full ids are 36 chars of noise). */
const shortId = (id: string) => id.slice(0, 8);

// ───────────────────────── Phiên bản ─────────────────────────

export function VersionsTab({ processId, activeVersion, reloadKey }: { processId: string; activeVersion: number; reloadKey: number }) {
  const [rows, setRows] = useState<ApprovalVersionRow[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [open, setOpen] = useState<ApprovalVersionRow | null>(null);

  useEffect(() => {
    let alive = true;
    listApprovalVersions(processId)
      .then((v) => { if (alive) { setRows(v); setErr(null); } })
      .catch((e) => { if (alive) { setRows([]); setErr(msg(e, t("page.approvalVersions.err.versions"))); } });
    return () => { alive = false; };
  }, [processId, reloadKey]);

  async function view(v: ApprovalVersionRow) {
    try {
      setOpen(await getApprovalVersion(processId, v.version));
    } catch (e) {
      setErr(msg(e, t("page.approvalVersions.err.version")));
    }
  }

  const columns = useMemo<ColumnDef<ApprovalVersionRow>[]>(() => [
    {
      id: "version",
      header: t("page.approvalVersions.col.version"),
      cell: ({ row: { original: v } }) => (
        <span>
          {tf("page.approvalVersions.versionN", { n: v.version })}{" "}
          {v.version === activeVersion && <StatusText tone="success">{t("page.approvalVersions.badge.active")}</StatusText>}
        </span>
      ),
    },
    { id: "when", header: t("page.approvalVersions.col.when"), cell: ({ row: { original: v } }) => <span className="tabular-nums">{formatDateTime(v.createdAt)}</span> },
    { id: "note", header: t("page.approvalVersions.col.note"), meta: { wrap: true }, cell: ({ row: { original: v } }) => v.note ?? "" },
    {
      id: "view",
      header: t("page.approvalVersions.col.view"),
      cell: ({ row: { original: v } }) => (
        <Button variant="base" aria-label={tf("page.approvalVersions.viewLabel", { n: v.version })} onClick={() => void view(v)}>
          {t("page.approvalVersions.view")}
        </Button>
      ),
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps -- view closes over processId only
  ], [activeVersion, processId]);

  if (rows === null) return <Spinner size="small" label={t("page.approvalVersions.versionsLabel")} />;
  // The chosen version opens in place under its row.
  const detail = (v: ApprovalVersionRow) => {
    if (open?.version !== v.version) return null;
    const def = open.definition as { steps?: ApprovalStep[]; graph?: unknown } | undefined;
    return (
      <div className="slds-p-around_small">
        {def?.steps?.length ? (
          <ol className="slds-list_ordered" aria-label={tf("page.approvalVersions.versionSteps", { n: open.version })}>
            {def.steps.map((st, i) => <li key={st.id ?? i}>{st.name || tf("page.approvalBuilder.stepN", { n: i + 1 })}</li>)}
          </ol>
        ) : (
          <p className="slds-text-color_weak">{def?.graph ? t("page.approvalVersions.versionBranching") : t("page.approvalVersions.versionNoSteps")}</p>
        )}
        <Button className="slds-m-top_x-small" onClick={() => setOpen(null)}>{t("page.approvalVersions.close")}</Button>
      </div>
    );
  };
  return (
    <div>
      {err && <ScopedNotification theme="error" role="alert" className="slds-m-bottom_small">{err}</ScopedNotification>}
      {rows.length === 0 && !err
        ? <p className="slds-text-color_weak">{t("page.approvalVersions.versionsEmpty")}</p>
        : <DataTable label={t("page.approvalVersions.versionsLabel")} columns={columns} data={rows} getRowId={(v) => v.id} bordered rowDetail={detail} />}
    </div>
  );
}

// ───────────────────────── Hồ sơ đang chạy ─────────────────────────

type RowAction = { id: string; kind: "cancel" | "override" } | null;

export function InstancesTab({ processId, reloadKey, onChanged }: { processId: string; reloadKey: number; onChanged?: () => void }) {
  const [rows, setRows] = useState<ApprovalInstanceRow[] | null>(null);
  const [stuckOnly, setStuckOnly] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [ok, setOk] = useState(false);
  const [busy, setBusy] = useState<string | null>(null);
  const [action, setAction] = useState<RowAction>(null);
  const [reason, setReason] = useState("");
  const [reasonErr, setReasonErr] = useState(false);
  const [tick, setTick] = useState(0);

  useEffect(() => {
    let alive = true;
    listApprovalProcessInstances(processId, { status: "pending", stuck: stuckOnly })
      .then((v) => { if (alive) { setRows(v); setErr(null); } })
      .catch((e) => { if (alive) { setRows([]); setErr(msg(e, t("page.approvalVersions.err.instances"))); } });
    return () => { alive = false; };
  }, [processId, stuckOnly, reloadKey, tick]);

  const run = useCallback(async (id: string, fn: () => Promise<void>) => {
    setBusy(id); setErr(null); setOk(false);
    try {
      await fn();
      setAction(null); setReason(""); setReasonErr(false); setOk(true);
      setTick((n) => n + 1);
      onChanged?.();
    } catch (e) {
      setErr(msg(e, t("page.approvalVersions.err.instanceAction")));
    } finally {
      setBusy(null);
    }
  }, [onChanged]);

  const columns = useMemo<ColumnDef<ApprovalInstanceRow>[]>(() => [
    {
      id: "record",
      header: t("page.approvalVersions.instances.col.record"),
      cell: ({ row: { original: r } }) => (
        <div>
          <div>{r.entityName}</div>
          <div className="slds-text-body_small slds-text-color_weak tabular-nums">{shortId(r.recordId)} · {formatDateTime(r.createdAt)}</div>
        </div>
      ),
    },
    {
      id: "version",
      header: t("page.approvalVersions.instances.col.version"),
      cell: ({ row: { original: r } }) => (r.processVersion > 0 ? tf("page.approvalVersions.versionN", { n: r.processVersion }) : t("page.approvalVersions.instances.versionUnknown")),
    },
    { id: "step", header: t("page.approvalVersions.instances.col.step"), cell: ({ row: { original: r } }) => r.stepName || r.nodeId },
    {
      id: "state",
      header: t("page.approvalVersions.instances.col.state"),
      meta: { wrap: true },
      cell: ({ row: { original: r } }) => (
        <div>
          {r.stuck ? <StatusText tone="error">{t("page.approvalVersions.instances.stuck")}</StatusText> : <span>{t("page.approvalVersions.instances.waiting")}</span>}
          {r.escalated && <>{" "}<StatusText tone="warn">{t("page.approvalVersions.instances.overdue")}</StatusText></>}
          {r.effectFailed && <div className="slds-text-body_small slds-text-color_error">{t("page.approvalVersions.instances.effectFailed")}</div>}
          {r.lastError && <div className="slds-text-body_small slds-text-color_weak">{r.lastError}</div>}
        </div>
      ),
    },
    {
      id: "actions",
      header: t("page.approvalVersions.instances.col.actions"),
      cell: ({ row: { original: r } }) => (
        <div className="flex flex-wrap gap-1">
          {r.stuck ? (
            <Button disabled={busy === r.id} aria-label={tf("page.approvalVersions.instances.retryLabel", { id: shortId(r.id) })}
              onClick={() => void run(r.id, () => retryApprovalInstance(r.id))}>
              {t("page.approvalVersions.instances.retry")}
            </Button>
          ) : (
            <Button aria-label={tf("page.approvalVersions.instances.overrideLabel", { id: shortId(r.id) })}
              onClick={() => { setAction({ id: r.id, kind: "override" }); setReason(""); setReasonErr(false); }}>
              {t("page.approvalVersions.instances.override")}
            </Button>
          )}
          <Button variant="text-destructive" aria-label={tf("page.approvalVersions.instances.cancelLabel", { id: shortId(r.id) })}
            onClick={() => setAction({ id: r.id, kind: "cancel" })}>
            {t("page.approvalVersions.instances.cancel")}
          </Button>
        </div>
      ),
    },
  ], [busy, run]);

  const detail = (r: ApprovalInstanceRow) => {
    if (action?.id !== r.id) return null;
    if (action.kind === "cancel") {
      return (
        <InlineConfirm tone="error" message={t("page.approvalVersions.instances.cancelBody")}
          confirmLabel={t("page.approvalVersions.instances.cancelConfirm")} busy={busy === r.id}
          onConfirm={() => void run(r.id, () => cancelApprovalInstance(r.id))} onCancel={() => setAction(null)} />
      );
    }
    const decide = (decision: "approve" | "reject") => {
      if (!reason.trim()) { setReasonErr(true); return; }
      void run(r.id, () => overrideApproval(r.id, decision, reason.trim()));
    };
    return (
      <div className="slds-p-around_small">
        <p className="slds-text-body_small slds-m-bottom_x-small">{t("page.approvalVersions.instances.overrideBody")}</p>
        <Textarea label={t("page.approvalVersions.instances.overrideReason")} required value={reason}
          error={reasonErr ? t("page.approvalVersions.instances.reasonRequired") : undefined}
          onChange={(e) => { setReason(e.target.value); setReasonErr(false); }} />
        <div className="flex gap-2 slds-m-top_small">
          <Button variant="brand" disabled={busy === r.id} onClick={() => decide("approve")}>{t("page.approvalVersions.instances.approve")}</Button>
          <Button variant="destructive" disabled={busy === r.id} onClick={() => decide("reject")}>{t("page.approvalVersions.instances.reject")}</Button>
          <Button onClick={() => setAction(null)}>{t("page.approvalVersions.cancel")}</Button>
        </div>
      </div>
    );
  };

  return (
    <div>
      <Checkbox label={t("page.approvalVersions.instances.stuckOnly")} checked={stuckOnly} onChange={(e) => { setStuckOnly(e.target.checked); setRows(null); }} />
      {err && <ScopedNotification theme="error" role="alert" className="slds-m-vertical_small">{err}</ScopedNotification>}
      {ok && <ScopedNotification theme="success" className="slds-m-vertical_small">{t("page.approvalVersions.instances.done")}</ScopedNotification>}
      <div className="slds-m-top_small">
        {rows === null ? <Spinner size="small" label={t("page.approvalVersions.instances.label")} />
          : rows.length === 0 && !err ? <p className="slds-text-color_weak">{t("page.approvalVersions.instances.empty")}</p>
          : <DataTable label={t("page.approvalVersions.instances.label")} columns={columns} data={rows} getRowId={(r) => r.id} rowDetail={detail} bordered />}
      </div>
    </div>
  );
}

// ───────────────────────── Chuyển sang bản mới ─────────────────────────

/** In-place panel (Studio opens no dialogs): preview what moving would do, then apply. */
export function RepinPanel({ processId, version, onClose, onDone }: {
  processId: string; version: number; onClose: () => void; onDone: () => void;
}) {
  const [outcome, setOutcome] = useState<ApprovalRepinOutcome | null>(null);
  const [applied, setApplied] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [after, setAfter] = useState<string | undefined>();

  const call = useCallback(async (dryRun: boolean, cursor?: string) => {
    setBusy(true); setErr(null);
    try {
      const out = await repinApprovalInstances(processId, { toVersion: version, dryRun, after: cursor });
      setOutcome(out); setApplied(!dryRun); setAfter(out.nextAfter);
      if (!dryRun) onDone();
    } catch (e) {
      setErr(msg(e, t("page.approvalVersions.err.repin")));
    } finally {
      setBusy(false);
    }
  }, [processId, version, onDone]);

  const columns = useMemo<ColumnDef<ApprovalRepinOutcome["results"][number]>[]>(() => [
    { id: "i", header: t("page.approvalVersions.repin.col.instance"), cell: ({ row: { original: r } }) => <span className="tabular-nums">{shortId(r.instanceId)}</span> },
    { id: "from", header: t("page.approvalVersions.repin.col.from"), cell: ({ row: { original: r } }) => r.fromNode },
    { id: "to", header: t("page.approvalVersions.repin.col.to"), cell: ({ row: { original: r } }) => r.toNode },
    {
      id: "res",
      header: t("page.approvalVersions.repin.col.result"),
      meta: { wrap: true },
      cell: ({ row: { original: r } }) => r.ok
        ? <StatusText tone="success">{applied ? t("page.approvalVersions.repin.done") : t("page.approvalVersions.repin.ok")}</StatusText>
        : <span><StatusText tone="warn">{t("page.approvalVersions.repin.refused")}</StatusText> {r.reason}</span>,
    },
  ], [applied]);

  const okN = outcome?.results.filter((r) => r.ok).length ?? 0;
  const noN = (outcome?.results.length ?? 0) - okN;
  return (
    <section className="slds-box slds-m-bottom_small" aria-label={tf("page.approvalVersions.repin.title", { version })}>
      <h3 className="slds-text-heading_small slds-m-bottom_x-small">{tf("page.approvalVersions.repin.title", { version })}</h3>
      <p className="slds-m-bottom_small">{t("page.approvalVersions.repin.intro")}</p>
      {err && <ScopedNotification theme="error" role="alert" className="slds-m-bottom_small">{err}</ScopedNotification>}
      {busy && <Spinner size="small" />}
      {outcome && (
        outcome.results.length === 0
          ? <p className="slds-text-color_weak">{t("page.approvalVersions.repin.none")}</p>
          : (
            <>
              <p className="slds-m-bottom_x-small" role="status">
                {tf(applied ? "page.approvalVersions.repin.appliedSummary" : "page.approvalVersions.repin.summary", { ok: okN, no: noN })}
              </p>
              <DataTable label={t("page.approvalVersions.repin.table")} columns={columns} data={outcome.results} getRowId={(r) => r.instanceId} maxHeight="18rem" bordered />
            </>
          )
      )}
      {outcome && outcome.remaining > 0 && (
        <p className="slds-m-top_small">
          {tf("page.approvalVersions.repin.remaining", { n: outcome.remaining })}{" "}
          <Button variant="base" disabled={busy} onClick={() => void call(!applied, after)}>{t("page.approvalVersions.repin.next")}</Button>
        </p>
      )}
      <div className="flex gap-2 slds-m-top_small">
        <Button disabled={busy} onClick={() => void call(true)}>{t("page.approvalVersions.repin.preview")}</Button>
        <Button variant="brand" disabled={busy || applied || okN === 0} onClick={() => void call(false)}>{t("page.approvalVersions.repin.apply")}</Button>
        <Button onClick={onClose}>{t("page.approvalVersions.repin.close")}</Button>
      </div>
    </section>
  );
}
