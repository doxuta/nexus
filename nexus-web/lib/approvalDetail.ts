// lib/approvalDetail.ts — W6: approval detail types + PascalCase↔camelCase adapters.
// Two live envelope dialects (api-conventions.md §9.5):
//   • GET /approvals/:id       → camelCase (new endpoint, follows convention)
//   • GET /approvals/pending   → PascalCase bpm.Instance (documented deviation — DO NOT break)
// Both funnel into ONE ApprovalDetailData shape so the inbox and the detail screen share
// rendering code. Pure module — no fetch (lib/api.ts owns HTTP and imports these).

import { t } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";

export interface ApprovalHistoryItem {
  step: number;
  action: string;
  actor: string;
  comment?: string;
  /** Delegation only: the user id the step was handed to (resolved to a name for display). */
  target?: string;
  /** The step's stable id and its name when this entry was written (specs/043 S0; absent before). */
  node?: string;
  stepName?: string;
  /** The process version the approval ran under when this entry was written (specs/043 S10, #70). */
  version?: number;
  /** One vote of a many-seat step that did NOT finish the step (specs/043 S4): the step is still open. */
  partial?: boolean;
  /** "override" = an admin decided in place of the seats («Quyết thay», FR-023). */
  via?: string;
  at: string;
}

/** Elapsed time of the whole approval (specs/043 S2, #40) — GET /approvals/:id `durations`. The
 *  business figure is present ONLY when basis is "business"; otherwise the wall figure is all there is. */
export interface ApprovalDurations {
  basis: string;
  totalBusinessSeconds?: number;
  totalWallSeconds: number;
}

export interface ApprovalProcessStep {
  /** Graph pin only (specs/043 S7): the node's stable id, its kind (approval|task|gateway|auto|wait|end) and routing. */
  id?: string;
  kind?: string;
  next?: string;
  branches?: { name: string; to: string }[];
  default?: string;
  /** The step's own label ("Giám đốc duyệt") — optional; pinned with the steps at arm time. */
  name?: string;
  approverType: string;
  approverId: string;
  slaSeconds?: number;
  /** A step held by several people (specs/043 S4): approverType/approverId are then empty. */
  approvers?: { type: string; id: string }[];
  /** "all" | "any" | "quorum" — only beside approvers. */
  completion?: string;
  /** The share that must approve — only beside `completion: "quorum"`. */
  completionPercent?: number;
}

/** One seat of the step the approval stands on (GET /approvals/:id `seats`, specs/043 S4). */
export interface ApprovalSeatData {
  id: string;
  assigneeType: string;
  assigneeId: string;
  /** open | done | cancelled */
  status: string;
  /** approved | rejected | reassigned | superseded | recalled — absent while open. */
  outcome?: string;
  decidedBy?: string;
  comment?: string;
  decidedAt?: string;
}

/** "2/3 — cần 2": the tally of a step held by several people (GET /approvals/:id `completion`). */
export interface ApprovalCompletionData {
  mode: string;
  total: number;
  approved: number;
  rejected: number;
  open: number;
  required: number;
  /** The quorum percentage (specs/043 S6b); 0 for every other mode. */
  percent: number;
}

export interface ApprovalProcessInfo {
  id: string;
  name: string;
  entityName: string;
  trigger: string;
  steps: ApprovalProcessStep[];
  isActive?: boolean;
  /** Live process row deleted after arming — steps are the instance's pinned copy (still what the engine walks). */
  deleted?: boolean;
  /** `steps` is the copy pinned at arm time, not the live process — set even when the live row is
   *  alive and has been edited since, so the screen never presents a stale chain as current. */
  pinned?: boolean;
  /** The process version this instance runs under — the one it pinned when it started (specs/043
   *  S10). Absent for an approval started before versions existed: never guessed. */
  version?: number;
}

export interface ApprovalDetailData {
  id: string;
  processId?: string;
  entityName: string;
  recordId: string | null;
  currentStep: number;
  status: string;
  currentApproverType?: string;
  currentApproverId?: string;
  slaDueAt?: string;
  /** What slaDueAt was computed on (specs/043 S1): business | wall | wall_no_calendar | wall_calendar_error. */
  slaBasis?: string;
  escalated?: boolean;
  history: ApprovalHistoryItem[];
  createdBy?: string;
  createdAt?: string;
  updatedAt?: string;
  process?: ApprovalProcessInfo | null;
  /** Detail endpoint only (specs/043 S2). */
  durations?: ApprovalDurations;
  /** Detail endpoint only (specs/043 S4): the seats of the current step, and — when several people
   *  hold it — the tally. Absent on an older server. */
  seats?: ApprovalSeatData[];
  completion?: ApprovalCompletionData;
  /** The SERVER's answer to "does this approval sit in my inbox now" (a seat I hold — specs/043 S4).
   *  On a step with several people current_approver_* is empty, so the screen must not re-derive
   *  "is it mine" from it. Absent on an older server: the projection rule below still applies. */
  awaitsMe?: boolean;
  /** Detail endpoint only (specs/043 S8): the approval was returned to its submitter and waits for
   *  "Gửi lại" — nobody approves or rejects it meanwhile. returnReason is the rejector's note. */
  awaitingRework?: boolean;
  returnReason?: string;
  /** Gateway id → name of the branch the approval took (specs/043 S7) — the path the screen draws. */
  gateway?: Record<string, string>;
}

// ── History normalization (note|comment → comment) ───────────────────────────

function str(v: unknown): string {
  return typeof v === "string" ? v : v == null ? "" : String(v);
}

function optStr(v: unknown): string | undefined {
  const s = str(v);
  return s === "" ? undefined : s;
}

function toInt(v: unknown): number {
  const n = typeof v === "number" ? v : Number(v);
  return Number.isFinite(n) ? n : 0;
}

export function normalizeHistory(raw: unknown): ApprovalHistoryItem[] {
  if (!Array.isArray(raw)) return [];
  return raw
    .filter((h): h is Record<string, unknown> => typeof h === "object" && h !== null)
    .map((h) => ({
      step: toInt(h.step),
      action: str(h.action),
      actor: str(h.actor),
      // Detail endpoint says `comment`, pending history says `note` — accept both.
      comment: optStr(h.comment) ?? optStr(h.note),
      target: optStr(h.target) ?? optStr(h.Target),
      node: optStr(h.node),
      stepName: optStr(h.stepName),
      at: str(h.at),
    }));
}

// ── Adapters ─────────────────────────────────────────────────────────────────

// camelCase payload from GET /approvals/:id — mostly passthrough + history/process hygiene.
function toGateway(v: unknown): Record<string, string> | undefined {
  if (!v || typeof v !== "object") return undefined;
  const out: Record<string, string> = {};
  for (const [k, b] of Object.entries(v as Record<string, unknown>)) if (typeof b === "string") out[k] = b;
  return Object.keys(out).length ? out : undefined;
}

export function fromDetailPayload(raw: Record<string, unknown>): ApprovalDetailData {
  const proc = raw.process;
  let process: ApprovalProcessInfo | null = null;
  if (proc && typeof proc === "object") {
    const p = proc as Record<string, unknown>;
    process = {
      id: str(p.id),
      name: str(p.name),
      entityName: str(p.entityName),
      trigger: str(p.trigger),
      isActive: p.isActive === true,
      deleted: p.deleted === true,
      pinned: p.pinned === true,
      version: typeof p.version === "number" && p.version > 0 ? p.version : undefined,
      steps: Array.isArray(p.steps)
        ? p.steps
            .filter((s): s is Record<string, unknown> => typeof s === "object" && s !== null)
            .map((s) => ({
              id: optStr(s.id),
              kind: optStr(s.kind),
              next: optStr(s.next),
              branches: Array.isArray(s.branches)
                ? s.branches
                    .filter((b): b is Record<string, unknown> => typeof b === "object" && b !== null)
                    .map((b) => ({ name: str(b.name), to: str(b.to) }))
                : undefined,
              default: optStr(s.default),
              name: optStr(str(s.name).trim()),
              approverType: str(s.approverType),
              approverId: str(s.approverId),
              slaSeconds: typeof s.slaSeconds === "number" ? s.slaSeconds : undefined,
              approvers: Array.isArray(s.approvers)
                ? s.approvers
                    .filter((a): a is Record<string, unknown> => typeof a === "object" && a !== null)
                    .map((a) => ({ type: str(a.type), id: str(a.id) }))
                : undefined,
              completion: s.completion && typeof s.completion === "object" ? optStr((s.completion as Record<string, unknown>).mode) : undefined,
              completionPercent:
                s.completion && typeof s.completion === "object" && typeof (s.completion as Record<string, unknown>).percent === "number"
                  ? ((s.completion as Record<string, unknown>).percent as number)
                  : undefined,
            }))
        : [],
    };
  }
  return {
    id: str(raw.id),
    processId: optStr(raw.processId),
    entityName: str(raw.entityName),
    recordId: optStr(raw.recordId) ?? null,
    currentStep: toInt(raw.currentStep),
    status: str(raw.status),
    currentApproverType: optStr(raw.currentApproverType),
    currentApproverId: optStr(raw.currentApproverId),
    slaDueAt: optStr(raw.slaDueAt),
    slaBasis: raw.sla && typeof raw.sla === "object" ? optStr((raw.sla as Record<string, unknown>).basis) : undefined,
    escalated: raw.escalated === true,
    history: normalizeHistory(raw.history),
    createdBy: optStr(raw.createdBy),
    createdAt: optStr(raw.createdAt),
    updatedAt: optStr(raw.updatedAt),
    process,
    durations: toDurations(raw.durations),
    seats: toSeats(raw.seats),
    completion: toCompletion(raw.completion),
    awaitsMe: typeof raw.awaitsMe === "boolean" ? raw.awaitsMe : undefined,
    awaitingRework: raw.awaitingRework === true ? true : undefined,
    returnReason: optStr(raw.returnReason),
    gateway: toGateway(raw.gateway),
  };
}

function toSeats(v: unknown): ApprovalSeatData[] | undefined {
  if (!Array.isArray(v)) return undefined;
  return v
    .filter((s): s is Record<string, unknown> => typeof s === "object" && s !== null)
    .map((s) => ({
      id: str(s.id),
      assigneeType: str(s.assigneeType),
      assigneeId: str(s.assigneeId),
      status: str(s.status),
      outcome: optStr(s.outcome),
      decidedBy: optStr(s.decidedBy),
      comment: optStr(s.comment),
      decidedAt: optStr(s.decidedAt),
    }));
}

function toCompletion(v: unknown): ApprovalCompletionData | undefined {
  if (!v || typeof v !== "object") return undefined;
  const c = v as Record<string, unknown>;
  return {
    mode: str(c.mode), total: toInt(c.total), approved: toInt(c.approved), rejected: toInt(c.rejected),
    open: toInt(c.open), required: toInt(c.required), percent: toInt(c.percent),
  };
}

// durations: totals + basis only — the per-step stretches (perNode) are FE-BPM-DETAIL's (T213).
function toDurations(v: unknown): ApprovalDurations | undefined {
  if (!v || typeof v !== "object") return undefined;
  const d = v as Record<string, unknown>;
  return {
    basis: str(d.basis),
    totalBusinessSeconds: typeof d.totalBusinessSeconds === "number" ? d.totalBusinessSeconds : undefined,
    totalWallSeconds: toInt(d.totalWallSeconds),
  };
}

// PascalCase item from GET /approvals/pending (bpm.Instance serialization).
export function fromPendingItem(raw: Record<string, unknown>): ApprovalDetailData {
  return {
    id: str(raw.ID),
    processId: optStr(raw.ProcessID),
    entityName: str(raw.EntityName),
    recordId: optStr(raw.RecordID) ?? null,
    currentStep: toInt(raw.CurrentStep),
    status: str(raw.Status) || "pending",
    currentApproverType: optStr(raw.CurrentApproverType),
    currentApproverId: optStr(raw.CurrentApproverID),
    slaDueAt: optStr(raw.SLADueAt),
    slaBasis: optStr(raw.slaBasis), // camelCase: a new key riding the legacy PascalCase row
    escalated: raw.Escalated === true,
    history: normalizeHistory(raw.History),
    createdBy: optStr(raw.CreatedBy),
    createdAt: optStr(raw.CreatedAt),
    updatedAt: optStr(raw.UpdatedAt),
    process: null,
    awaitsMe: typeof raw.awaitsMe === "boolean" ? raw.awaitsMe : undefined, // camelCase: new key on the PascalCase row
    awaitingRework: raw.awaitingRework === true ? true : undefined, // S8: a returned record offers no Duyệt / Từ chối
  };
}

// Dialect-agnostic: some instance endpoints serialize bpm.Instance PascalCase
// (/approvals/pending AND /:entity/:id/approvals — verified live 2026-07-03), the W6 detail
// endpoint is camelCase. Sniff the shape and route to the right adapter.
export function fromAnyInstance(raw: Record<string, unknown>): ApprovalDetailData {
  return "ID" in raw ? fromPendingItem(raw) : fromDetailPayload(raw);
}

// ── Inbox tab filter: is the CURRENT step waiting on this user? ───────────────
// Mirrors backend ACL (§9.5): user-target = me, role-target = a role I hold.

export function isAwaitingUser(
  a: Pick<ApprovalDetailData, "status" | "currentApproverType" | "currentApproverId" | "awaitsMe">,
  me: { id?: string; roleId?: string },
): boolean {
  if (a.status !== "pending") return false;
  // The server's own answer wins when it sent one (seats, specs/043 S4); the projection below is
  // the fallback for an older server — and is empty on a step held by several people.
  if (typeof a.awaitsMe === "boolean") return a.awaitsMe;
  if (!a.currentApproverId) return false;
  if (a.currentApproverType === "user") return !!me.id && a.currentApproverId === me.id;
  if (a.currentApproverType === "role") return !!me.roleId && a.currentApproverId === me.roleId;
  return false;
}

type Viewer = { id?: string; roleId?: string; isAdmin?: boolean } | null | undefined;

// Who may use which verb on a PENDING approval — the same rules the server enforces, so a screen
// never offers a button that can only come back APPROVAL_NOT_AUTHORIZED / APPROVAL_NOT_SUBMITTER.
// canDecide mirrors bpm.CanDecide (approve, reject AND delegate): admin, or the current approver.
export function canDecide(
  a: Pick<ApprovalDetailData, "status" | "currentApproverType" | "currentApproverId" | "awaitsMe" | "awaitingRework">,
  me: Viewer,
): boolean {
  if (!me || a.status !== "pending") return false;
  if (a.awaitingRework) return false; // with the submitter (S8): the only verb is their "Gửi lại"
  return !!me.isAdmin || isAwaitingUser(a, me);
}

// canRecall mirrors bpm Requester.Recall: admin, or the person who sent the request.
export function canRecall(a: Pick<ApprovalDetailData, "status" | "createdBy">, me: Viewer): boolean {
  if (!me || a.status !== "pending") return false;
  return !!me.isAdmin || (!!me.id && a.createdBy === me.id);
}

// ── Display meta ─────────────────────────────────────────────────────────────

// bpm.Instance steps are 0-INDEXED (verified live: fresh instance → currentStep: 0).
// Humans count from 1 — every display goes through this.
export function stepLabel(step0: number): string {
  return `Bước ${step0 + 1}`;
}

export type ApprovalTone = "ac" | "ok" | "dg" | "wn" | "neu";

export function approvalStatusMeta(status: string): { tone: ApprovalTone; label: string } {
  switch (status) {
    case "pending":
      return { tone: "wn", label: "Đang chờ duyệt" };
    case "approved":
      return { tone: "ok", label: "Đã duyệt" };
    case "rejected":
      return { tone: "dg", label: "Đã từ chối" };
    case "cancelled":
      // The engine's word is "cancelled"; the thing that happened is that the sender took it
      // back. Showing the raw status was the machine's vocabulary on the customer face.
      return { tone: "neu", label: "Đã thu hồi" };
    default:
      return { tone: "neu", label: status || "—" };
  }
}

export function historyActionMeta(action: string): { tone: ApprovalTone; label: string } {
  switch (action) {
    case "approved":
    case "approve":
      return { tone: "ok", label: "Đã duyệt" };
    case "rejected":
    case "reject":
      return { tone: "dg", label: "Từ chối" };
    case "escalated":
    case "escalate":
      return { tone: "wn", label: "Leo thang" };
    case "submitted":
    case "created":
      return { tone: "ac", label: "Gửi duyệt" };
    case "recalled":
      return { tone: "neu", label: "Đã thu hồi" };
    case "resubmitted":
      // The submitter fixed a returned record and sent it again (specs/043 S8).
      return { tone: "ac", label: "Đã gửi lại" };
    case "delegated":
      return { tone: "wn", label: "Uỷ quyền" };
    case "completed":
      // A task step (specs/043 S5) is finished, not approved: say what happened.
      return { tone: "ok", label: "Đã hoàn thành" };
    default:
      return { tone: "neu", label: action || "—" };
  }
}

// slaBasisLabel: what a deadline was computed on, in words (specs/043 S1). null = no deadline or
// an unknown token — the screen then shows no basis rather than inventing one.
const SLA_BASIS_KEYS: Record<string, I18nKey> = {
  business: "approval.slaBasis.business",
  wall: "approval.slaBasis.wall",
  wall_no_calendar: "approval.slaBasis.wall_no_calendar",
  wall_calendar_error: "approval.slaBasis.wall_calendar_error",
};

export function slaBasisLabel(basis?: string): string | null {
  const key = basis ? SLA_BASIS_KEYS[basis] : undefined;
  return key ? t(key) : null;
}

// "3 giờ 20 phút" — whole minutes, hours not rolled into days (a working day is not 24 hours).
function fmtSpan(seconds: number): string {
  const m = Math.floor(Math.max(0, seconds) / 60);
  if (m === 0) return seconds > 0 ? "dưới 1 phút" : "0 phút";
  const h = Math.floor(m / 60);
  if (h === 0) return `${m} phút`;
  return m % 60 ? `${h} giờ ${m % 60} phút` : `${h} giờ`;
}

// elapsedLabel: "hồ sơ này duyệt mất bao lâu" in words (specs/043 S2, #40). Working time when the
// server measured it on the calendar; otherwise the wall figure WITH its reason, never passed off as
// working hours. A pending approval is still running: "Đã chờ", not "Mất". null = nothing to say.
export function elapsedLabel(d: ApprovalDurations | undefined, status: string): string | null {
  if (!d) return null;
  const verb = status === "pending" ? "Đã chờ" : "Mất";
  if (d.basis === "business") {
    // No business figure under a business basis is a malformed payload: say nothing rather than
    // print the wall figure with a "(giờ làm việc)" label.
    return typeof d.totalBusinessSeconds === "number" ? `${verb} ${fmtSpan(d.totalBusinessSeconds)} làm việc` : null;
  }
  const why = slaBasisLabel(d.basis);
  return `${verb} ${fmtSpan(d.totalWallSeconds)}${why ? ` (${why})` : ""}`;
}

// SLA countdown — deterministic via injectable `now`.
export function fmtSLA(iso?: string, now: number = Date.now()): { label: string; overdue: boolean } {
  if (!iso) return { label: "—", overdue: false };
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return { label: iso, overdue: false };
  const diff = t - now;
  if (diff < 0) return { label: "Quá hạn", overdue: true };
  const h = Math.floor(diff / 3_600_000);
  if (h < 1) return { label: `${Math.max(1, Math.floor(diff / 60_000))} phút còn lại`, overdue: false };
  if (h < 24) return { label: `${h} giờ còn lại`, overdue: false };
  return { label: `${Math.floor(h / 24)} ngày còn lại`, overdue: false };
}
