"use client";

// "Việc của tôi" (/approvals) — the work-item inbox (specs/043 S5, FE-BPM-INBOX #129).
// GET /work-items?view=mine|pool|done (camelCase, names not codes) · POST /work-items/:id/claim|release|complete
// · POST /approvals/:instanceId/approve|reject (the server finds the actor's seat, including a claimed pool seat).
// Tabs "Của tôi / Chờ nhận / Đã xử lý" over one DataTable; only the active tab is fetched. A row offers the
// verbs workItemActions() derives from kind/assignee/claim (the server re-checks every one). Deadlines read as
// time + what it is counted in ("giờ làm việc") + countdown, with an SLA badge; a returned approval's reason
// shows under the record. Reject needs a reason (blank refused here; the server's 422 comes back at the box).
// A 409 (someone else decided / claimed first) refreshes every tab instead of leaving a stale row.
// Delegate and the step history live on the record page (/approvals/[id]) — "Mở hồ sơ".
// SLDS (specs/038 T031): ObjectHomePage template + Tabs + DataTable (RESEARCH §5 /approvals).
// The nav badge and Home still read /approvals/pending (qk.pendingApprovals): decisions invalidate both.
import { useState, type ReactNode } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { CheckSquare, Inbox } from "lucide-react";
import {
  ensureAuth, approveRecord, rejectRecord, listWorkItems, workItemCounts, claimWorkItem, releaseWorkItem,
  completeWorkItem, ApiError, type WorkItem, type WorkItemView,
} from "@/lib/api";
import { useAuth } from "@/lib/auth/AuthProvider";
import { qk } from "@/lib/query/keys";
import { errorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { dueText, kindLabel, slaState, SLA_KEYS, workItemActions } from "@/lib/workItems";
import { ObjectHomePage } from "@/components/slds/templates/ObjectHomePage";
import { Badge } from "@/components/slds/Badge";
import { Button, buttonClassName } from "@/components/slds/Button";
import { DataTable } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { FormElement } from "@/components/slds/FormElement";
import { MenuItem } from "@/components/slds/Menu";
import { Modal } from "@/components/slds/Modal";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Tabs } from "@/components/slds/Tabs";
import { Textarea } from "@/components/slds/Textarea";
import { toast } from "@/components/ui/Toaster";

const VIEWS: WorkItemView[] = ["mine", "pool", "done"];
const TAB_KEYS = { mine: "page.inbox.tab.mine", pool: "page.inbox.tab.pool", done: "page.inbox.tab.done" } as const;
const EMPTY_KEYS = { mine: "page.inbox.empty.mine", pool: "page.inbox.empty.pool", done: "page.inbox.empty.done" } as const;

const title = (w: WorkItem) => (w.recordLabel ? `${w.entityLabel || w.entityName}: ${w.recordLabel}` : `${w.entityLabel || w.entityName} #${w.recordId.slice(0, 8)}`);
const SLA_THEME = { paused: undefined, overdue: "error", soon: "warning", ok: "success" } as const;

type Dialog = { item: WorkItem; mode: "approve" | "reject" | "complete" };

// ── Decision / completion dialog (modals/base) ───────────────────────────────
function DecisionModal({ item, mode, onClose, onDone, onStale }: { item: WorkItem; mode: Dialog["mode"]; onClose: () => void; onDone: () => void; onStale: () => void }) {
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [noteErr, setNoteErr] = useState<string | null>(null);
  const [stale, setStale] = useState(false);
  const reject = mode === "reject";
  const close = stale ? onStale : onClose;
  const label = mode === "approve" ? "page.approvals.decide.confirmApprove" : reject ? "page.approvals.decide.confirmReject" : "page.inbox.completeConfirm";

  async function submit() {
    setErr(null); setNoteErr(null);
    if (reject && !note.trim()) { setNoteErr(t("page.approvals.decide.noteRequired")); return; }
    setBusy(true);
    try {
      await ensureAuth();
      if (mode === "approve") await approveRecord(item.instanceId, note.trim() || undefined);
      else if (reject) await rejectRecord(item.instanceId, note.trim());
      else await completeWorkItem(item.id, note.trim() || undefined);
      toast.success(t(mode === "approve" ? "page.approvals.decide.approved" : reject ? "page.approvals.decide.rejected" : "page.inbox.completed"));
      onDone();
    } catch (e) {
      if (e instanceof ApiError && e.code === "APPROVAL_REJECT_REASON_REQUIRED") setNoteErr(errorMessage(e));
      else if (e instanceof ApiError && e.code === "WORK_ITEM_REQUIRED_FIELDS") {
        const f = (e.details as { fields?: string[] }).fields ?? [];
        setErr(f.length ? tf("page.inbox.requiredFields", { fields: f.join(", ") }) : errorMessage(e));
      } else setErr(e instanceof ApiError ? errorMessage(e) : t("page.inbox.actionError"));
      if (e instanceof ApiError && (e.status === 409 || e.code === "WORK_ITEM_NOT_FOUND")) setStale(true);
      setBusy(false);
    }
  }

  return (
    <Modal
      open
      onOpenChange={(o) => { if (!o && !busy) close(); }}
      title={mode === "approve" ? tf("page.inbox.approveTitle", { n: item.nodeName }) : reject ? tf("page.inbox.rejectTitle", { n: item.nodeName }) : t("page.inbox.completeTitle")}
      tagline={title(item)}
      footer={
        <>
          <Button onClick={close} disabled={busy}>{t("action.cancel")}</Button>
          <Button variant={reject ? "destructive" : "brand"} onClick={submit} disabled={busy}>{busy ? t("page.approvals.decide.working") : t(label)}</Button>
        </>
      }
    >
      {item.dueAt && (
        <FormElement label={t("page.approvals.decide.due")} layout="stacked" readOnly>
          <span className={item.overdue ? "slds-text-color_error" : undefined}>{dueText(item)}</span>
        </FormElement>
      )}
      {err && <ScopedNotification theme="error" className="slds-m-vertical_small">{err}</ScopedNotification>}
      <Textarea
        label={t("page.approvals.decide.note")}
        required={reject}
        rows={3}
        value={note}
        onChange={(e) => { setNote(e.target.value); setNoteErr(null); }}
        placeholder={t(reject ? "page.approvals.decide.noteReject" : "page.approvals.decide.noteApprove")}
        error={noteErr ?? undefined}
      />
    </Modal>
  );
}

// ── One tab: its own query, table and empty state ────────────────────────────
function InboxList({ view, onAct }: { view: WorkItemView; onAct: (d: Dialog) => void }) {
  const qc = useQueryClient();
  const router = useRouter();
  const q = useInfiniteQuery({
    queryKey: qk.workItems(view),
    queryFn: async ({ pageParam }) => {
      await ensureAuth();
      return listWorkItems(view, pageParam);
    },
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.nextCursor ?? undefined,
    staleTime: 0,
  });
  const items = q.data?.pages.flatMap((p) => p.items) ?? [];

  async function run(verb: "claim" | "release", w: WorkItem) {
    try {
      await ensureAuth();
      if (verb === "claim") await claimWorkItem(w.id); else await releaseWorkItem(w.id);
      toast.success(t(verb === "claim" ? "page.inbox.claimed" : "page.inbox.released"));
    } catch (e) {
      toast.error(e instanceof ApiError ? errorMessage(e) : t("page.inbox.actionError"));
    } finally {
      qc.invalidateQueries({ queryKey: qk.workItems() });
    }
  }

  const columns: ColumnDef<WorkItem>[] = [
    {
      id: "record", header: t("page.inbox.col.record"), accessorFn: (w) => title(w),
      cell: ({ row: { original: w } }) => (
        <>
          <Link href={`/approvals/${w.instanceId}`}>{title(w)}</Link>
          {w.returnReason && <div className="slds-text-body_small slds-text-color_weak">{tf("page.inbox.reworkReason", { reason: w.returnReason })}</div>}
        </>
      ),
    },
    { id: "process", header: t("page.inbox.col.process"), accessorFn: (w) => w.processName },
    { id: "step", header: t("page.inbox.col.step"), accessorFn: (w) => w.nodeName },
    { id: "kind", header: t("page.inbox.col.kind"), accessorFn: (w) => kindLabel(w.kind), cell: ({ row }) => <Badge>{kindLabel(row.original.kind)}</Badge> },
    {
      id: "due", header: t("page.inbox.col.due"), accessorFn: (w) => dueText(w),
      sortingFn: (a, b) => (a.original.dueAt ?? "9").localeCompare(b.original.dueAt ?? "9"),
      cell: ({ row: { original: w } }) => {
        const s = slaState(w);
        return (
          <>
            <span className={s === "overdue" ? "slds-text-color_error" : undefined}>{dueText(w)}</span>
            {s && <Badge theme={SLA_THEME[s]} className="slds-m-left_xx-small">{t(SLA_KEYS[s])}</Badge>}
          </>
        );
      },
    },
    {
      id: "assignee", header: t("page.inbox.col.assignee"),
      accessorFn: (w) => (w.claimedByMe ? t("page.inbox.claimedByMe") : w.assigneeName || "—"),
    },
  ];

  if (q.error) return <div className="slds-p-around_medium"><ScopedNotification theme="error">{q.error instanceof ApiError ? q.error.message : t("page.inbox.loadError")}</ScopedNotification></div>;
  if (!q.isPending && items.length === 0) {
    return (
      <EmptyState
        className="slds-p-around_large"
        icon={Inbox}
        title={t(EMPTY_KEYS[view])}
        description={t("page.inbox.emptyDesc")}
        action={<div className="slds-m-top_medium"><Link href="/home" className={buttonClassName("neutral")}>{t("page.approvals.home")}</Link></div>}
      />
    );
  }
  return (
    <>
      <DataTable
        label={t("page.inbox.grid")}
        columns={columns}
        data={items}
        getRowId={(w) => w.id}
        sortable
        loading={q.isPending}
        rowActions={(w) => (
          <>
            {workItemActions(w, view).map((a) =>
              a === "claim" || a === "release"
                ? <MenuItem key={a} label={t(`page.inbox.act.${a}`)} onSelect={() => run(a, w)} />
                : a === "complete"
                  ? <MenuItem key={a} label={t("page.inbox.act.complete")} onSelect={() => onAct({ item: w, mode: "complete" })} />
                  : <MenuItem key={a} label={t(a === "approve" ? "page.approvals.approve" : "page.approvals.reject")} onSelect={() => onAct({ item: w, mode: a })} />,
            )}
            <MenuItem label={t("page.inbox.act.open")} onSelect={() => router.push(`/approvals/${w.instanceId}`)} />
          </>
        )}
      />
      {q.hasNextPage && (
        <div className="slds-m-around_small slds-text-align_center">
          <Button onClick={() => q.fetchNextPage()} disabled={q.isFetchingNextPage}>{t("page.inbox.loadMore")}</Button>
        </div>
      )}
    </>
  );
}

// ── Screen ───────────────────────────────────────────────────────────────────
export function ApprovalsInbox({ initialView = "mine" }: { initialView?: WorkItemView } = {}) {
  const { user: me } = useAuth();
  const qc = useQueryClient();
  const [view, setView] = useState<WorkItemView>(initialView);
  const [dialog, setDialog] = useState<Dialog | null>(null);
  const { data: counts } = useQuery({
    queryKey: qk.workItemCounts(),
    queryFn: async () => { await ensureAuth(); return workItemCounts(); },
    staleTime: 0,
    enabled: !!me,
  });

  const refresh = () => {
    qc.invalidateQueries({ queryKey: qk.workItems() });
    qc.invalidateQueries({ queryKey: qk.pendingApprovals() });
  };
  const done = () => { setDialog(null); refresh(); };
  const stale = () => { setDialog(null); refresh(); toast.error(t("page.inbox.stale")); };
  const n = (v: WorkItemView) => (v === "mine" ? counts?.mine : v === "pool" ? counts?.pool : undefined);

  const tabs = VIEWS.map((v) => ({
    value: v,
    label: n(v) ? `${t(TAB_KEYS[v])} (${n(v)})` : t(TAB_KEYS[v]),
    // Only the active tab fetches: the others stay empty until opened.
    content: v === view ? <InboxList view={v} onAct={setDialog} /> : (null as ReactNode),
  }));

  return (
    <div className="flex min-h-0 flex-1 flex-col p-4">
      <ObjectHomePage
        icon={{ glyph: CheckSquare, name: "approval" }}
        label={t("page.approvals.label")}
        title={t("page.inbox.title")}
        meta={counts ? tf("page.inbox.count", { n: counts.mine }) : undefined}
      >
        <Tabs items={tabs} value={view} onValueChange={(v) => setView(v as WorkItemView)} label={t("page.inbox.tabs")} />
      </ObjectHomePage>
      {dialog && <DecisionModal key={`${dialog.item.id}-${dialog.mode}`} item={dialog.item} mode={dialog.mode} onClose={() => setDialog(null)} onDone={done} onStale={stale} />}
    </div>
  );
}
