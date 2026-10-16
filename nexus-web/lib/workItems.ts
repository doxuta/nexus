// "Việc của tôi" helpers (specs/043 S5/S8/S13, FE-BPM-INBOX): what a work item lets its viewer do,
// and how its deadline reads. Pure — the screen owns the calls (lib/api.ts listWorkItems & co).
import type { WorkItem, WorkItemView } from "@/lib/api";
import { fmtSLA, slaBasisLabel } from "@/lib/approvalDetail";
import { formatDateTime } from "@/lib/format";
import { t } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";

export type WorkItemAction = "claim" | "release" | "approve" | "reject" | "complete";

/** A seat held by one named person has no pool: only role/team/queue seats can be claimed or released. */
export const isPoolItem = (it: WorkItem) => it.assigneeType !== "user";

/** The verbs a signed-in person is offered on a row. The server re-checks every one (404/409/422). */
export function workItemActions(it: WorkItem, view: WorkItemView): WorkItemAction[] {
  if (view === "done" || it.status !== "open") return [];
  if (isPoolItem(it) && !it.claimedBy) return ["claim"];
  if (isPoolItem(it) && !it.claimedByMe) return []; // someone else holds it
  const verbs: WorkItemAction[] = it.kind === "approval" ? ["approve", "reject"] : ["complete"];
  return isPoolItem(it) ? [...verbs, "release"] : verbs;
}

const KIND_KEYS: Record<string, I18nKey> = {
  approval: "page.inbox.kind.approval",
  task: "page.inbox.kind.task",
  rework: "page.inbox.kind.rework",
};
export const kindLabel = (kind: string) => (KIND_KEYS[kind] ? t(KIND_KEYS[kind]) : kind);

/** "05/10/2026 11:00 (giờ làm việc) · 3 giờ còn lại" — the time, what it was counted in, the countdown. */
export function dueText(it: Pick<WorkItem, "dueAt" | "dueBasis" | "paused" | "status">, now = Date.now()): string {
  if (!it.dueAt) return "—";
  const basis = slaBasisLabel(it.dueBasis);
  const tail = it.status !== "open" ? "" : it.paused ? ` · ${t("page.inbox.sla.paused").toLowerCase()}` : ` · ${fmtSLA(it.dueAt, now).label}`;
  return `${formatDateTime(it.dueAt)}${basis ? ` (${basis})` : ""}${tail}`;
}

export type SlaState = "paused" | "overdue" | "soon" | "ok";
export const SOON_MS = 4 * 3_600_000;

/** Badge state of an OPEN item with a deadline; null = nothing to flag (done, or no deadline). */
export function slaState(it: Pick<WorkItem, "dueAt" | "overdue" | "paused" | "status">, now = Date.now()): SlaState | null {
  if (it.status !== "open" || !it.dueAt) return null;
  if (it.paused) return "paused";
  const due = new Date(it.dueAt).getTime();
  if (it.overdue || due < now) return "overdue";
  return due - now <= SOON_MS ? "soon" : "ok";
}
export const SLA_KEYS: Record<SlaState, I18nKey> = {
  paused: "page.inbox.sla.paused",
  overdue: "page.inbox.sla.overdue",
  soon: "page.inbox.sla.soon",
  ok: "page.inbox.sla.ok",
};
