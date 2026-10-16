// lib/workflowRuns.ts — W6: pure shaping for workflow run history + dry-run trace.
// Contract: api-conventions.md §9.3 (runs) + §9.4 (dry-run) — camelCase, verified live shapes.
// NO fetch here — lib/api.ts owns the HTTP calls and imports these types (same pattern as runFilters).

import { formatDateTime } from "./format";
import { beforeTriggerLabel, isBeforeTrigger } from "./workflowBefore";
import { t } from "@/lib/i18n";

// ── Raw API shapes ────────────────────────────────────────────────────────────

export interface WorkflowRun {
  id: string;
  tenantId?: string;
  workflowId: string;
  triggerType: string;
  recordId: string | null;
  status: string; // success | failed | skipped | blocked (specs/048 before rules) | running (#338: written before the actions)
  error: string | null;
  // null (skipped) | ActionResultEntry[] | { scanned, matched, failed, webhooksHeld? } (scheduled summary)
  // | { blocked, applied, failed } (one before rule over one bulk-create / CSV-import batch)
  actionsResult: unknown;
  startedAt: string;
  finishedAt: string;
}

export interface WorkflowTestStep {
  step: number;
  kind: "trigger" | "condition" | "action";
  matched?: boolean; // trigger/condition steps
  evaluated?: boolean; // action steps
  detail?: Record<string, unknown>;
}

export interface WorkflowTestResult {
  workflowId: string;
  entityName: string;
  recordId: string; // "" on a simulated create without a record (specs/048 S6)
  trigger: string;
  op?: "create" | "update" | "delete"; // the simulated write (absent for scheduled / date_offset)
  matched: boolean;
  trace: WorkflowTestStep[];
}

// ── WF-01 (§9.1): which field does a config-time 422 point at? ────────────────
// Contract says details.field, but the live build omits `details` on these codes — the field
// name is only quoted inside the Vietnamese message (`field "lead_code" là …`). Prefer
// details.field when present; fall back to parsing the quoted name. Verified live 2026-07-03.
export function wf01Field(details: { field?: unknown } | undefined, message: string): string | null {
  if (details && typeof details.field === "string" && details.field !== "") return details.field;
  const m = /field "([^"]+)"/.exec(message ?? "");
  return m ? m[1] : null;
}

/**
 * The action ROW a config-time 422 points at by index — `details.field = "actions[i].<key>"`
 * (queue #79's SET_DEADLINE_CALENDAR_*): a field name cannot say which of two set_deadline pins
 * is the dead one. null for any other shape.
 */
export function actionRowPointer(details: { field?: unknown } | undefined): { index: number; key: string } | null {
  const m = typeof details?.field === "string" ? /^actions\[(\d+)\]\.(\w+)$/.exec(details.field) : null;
  return m ? { index: Number(m[1]), key: m[2] } : null;
}

/**
 * Action types whose config names a TARGET FIELD, i.e. the ones a 422 carrying `details.field`
 * can be pinned onto in the builder.
 *
 * This is a set rather than a literal comparison because the builder used to test
 * `a.type === "field_update"` alone. Every other field-targeting action — `assign` (its target
 * must be `user`/`text`), `set_deadline` (its target must be `datetime`) and `set_elapsed` (its target
 * must be `number`, SET_ELAPSED_FIELD_TYPE_INVALID) — raises the SAME
 * codes from the SAME guards in services.buildActionsJSON, and their message landed in the generic
 * footer instead of on the row that caused it. With several actions in one rule, "field %q phải là
 * kiểu datetime" in a footer does not say WHICH action to fix. `veto` (specs/048) names the field
 * its message is pinned under; an unknown one is RECORD_UNKNOWN_FIELD from the same guard.
 */
export const FIELD_TARGET_ACTIONS = new Set(["field_update", "assign", "set_deadline", "set_elapsed", "veto", "normalize"]);

// ── Status / trigger display meta ─────────────────────────────────────────────

// "pn" = pending: the action's work is on the job queue and has not settled yet (#339).
export type RunTone = "ac" | "dg" | "neu" | "ok" | "wn" | "pn";

// Why a before rule's veto let an exempt system write through (actionsResult[].skipped, specs/048).
const SKIP_REASON_KEYS = {
  approval_outcome: "workflow.runs.skipped.approval_outcome",
  payment_match: "workflow.runs.skipped.payment_match",
} as const;

/** A `skipped` value of an action result in words ("approval_outcome" → "Bỏ qua (ghi kết quả duyệt)"). */
export function skipReasonText(v: unknown): string {
  return v === "approval_outcome" || v === "payment_match" ? t(SKIP_REASON_KEYS[v]) : String(v);
}

function skipReason(actionsResult: unknown): keyof typeof SKIP_REASON_KEYS | null {
  if (!Array.isArray(actionsResult)) return null;
  for (const a of actionsResult) {
    const r = a && typeof a === "object" ? (a as { skipped?: unknown }).skipped : undefined;
    if (r === "approval_outcome" || r === "payment_match") return r;
  }
  return null;
}

// success = teal (accent — "engine did its job"), failed = danger, skipped = muted, blocked (a
// before rule refused the save/delete) = danger. actionsResult (optional) lets a skipped run of a
// before rule say which exempt write it let through.
export function runStatusMeta(status: string, actionsResult?: unknown): { tone: RunTone; label: string } {
  switch (status) {
    case "success":
      return { tone: "ac", label: "Thành công" };
    case "failed":
      return { tone: "dg", label: "Lỗi" };
    case "skipped": {
      const reason = skipReason(actionsResult);
      return { tone: "neu", label: reason ? t(SKIP_REASON_KEYS[reason]) : "Bỏ qua" };
    }
    case "blocked":
      return { tone: "dg", label: t("workflow.runs.blocked") };
    case "running":
      return { tone: "neu", label: t("workflow.runs.running") };
    default:
      return { tone: "neu", label: status || "—" };
  }
}

const TRIGGER_LABELS: Record<string, string> = {
  after_create: "Tạo mới",
  after_update: "Cập nhật",
  after_delete: "Xoá",
  scheduled: "Theo lịch",
  date_offset: "Mốc thời gian",
};

export function triggerLabel(trigger: string): string {
  if (isBeforeTrigger(trigger)) return beforeTriggerLabel(trigger);
  return TRIGGER_LABELS[trigger] ?? trigger ?? "—";
}

// ── actionsResult → discriminated view model ─────────────────────────────────

export interface ActionResultEntry {
  type: string;
  ok?: boolean;
  error?: string;
  // Set when the action DID run but not the way its config reads — today only a business_hours
  // set_deadline stamped on the wall-clock fallback because the tenant had no usable default
  // working calendar (DAILY-QUEUE #82, specs/037 §2.4). The run itself stays `success`.
  warning?: string;
  // Where a QUEUED action's work is (#339): queued (the queue still holds it / retries) | sent | failed.
  delivery?: string;
  [k: string]: unknown;
}

/**
 * Dot tone of one action row: failed = danger, ran-with-a-warning = warn, no outcome yet (a slot of a
 * run still `running` carries no `ok` — #338) = neutral, otherwise ok.
 */
export function actionResultTone(a: ActionResultEntry): "dg" | "wn" | "ok" | "neu" | "pn" {
  if (a.ok === false) return "dg";
  if (a.ok === undefined) return "neu";
  if (a.delivery === "queued") return "pn";
  return typeof a.warning === "string" && a.warning !== "" ? "wn" : "ok";
}

/**
 * The state word of an action row whose outcome is not simply "ok": "Đang chạy" (the run has not
 * finished), "Đang chờ gửi" (a queued mail the queue still holds / retries — #339), "Đã gửi" (the
 * queue delivered it). A failed delivery is not here: ok=false + the masked reason say it. null = no word.
 */
export function actionStateLabel(a: ActionResultEntry, runStatus: string): string | null {
  if (runStatus === "running" && a.ok === undefined) return t("workflow.runs.running");
  if (a.delivery === "queued") return t("workflow.runs.delivery.queued");
  if (a.delivery === "sent") return t("workflow.runs.delivery.sent");
  return null;
}

export type ShapedActionsResult =
  | { kind: "none" }
  | { kind: "actions"; actions: ActionResultEntry[] }
  // warned/warnings (queue #105): action results of the scan that ran with a warning, and each
  // distinct sentence once — the one-row summary's stand-in for actionsResult[].warning.
  | { kind: "scheduled"; scanned: number; matched: number; failed: number; webhooksHeld: number; warned: number; warnings: string[] }
  // One before rule over one all-or-nothing bulk create / CSV import: rows it refused (blocked), rows
  // a value-changing rule changed (applied) or could not change (failed). Rendering: FE-before-save-hooks-4.
  | { kind: "batch"; blocked: number; applied: number; failed: number };

function toNum(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}

export function shapeActionsResult(raw: unknown): ShapedActionsResult {
  if (Array.isArray(raw)) {
    const actions = raw.filter(
      (x): x is ActionResultEntry => typeof x === "object" && x !== null,
    );
    return actions.length ? { kind: "actions", actions } : { kind: "none" };
  }
  if (raw && typeof raw === "object") {
    const o = raw as Record<string, unknown>;
    // Before-rule batch summary { blocked, applied, failed } (§9.3) — checked first: it shares `failed`.
    if ("blocked" in o) {
      return { kind: "batch", blocked: toNum(o.blocked), applied: toNum(o.applied), failed: toNum(o.failed) };
    }
    // Scheduled-run summary { scanned, matched, failed } (§9.2).
    if ("scanned" in o || "matched" in o || "failed" in o) {
      return {
        kind: "scheduled",
        scanned: toNum(o.scanned),
        matched: toNum(o.matched),
        failed: toNum(o.failed),
        // Webhook deliveries this firing held (not sent) because earlier ones were still pending.
        webhooksHeld: toNum(o.webhooksHeld),
        warned: toNum(o.warned),
        warnings: Array.isArray(o.warnings) ? o.warnings.filter((w): w is string => typeof w === "string" && w !== "") : [],
      };
    }
  }
  return { kind: "none" };
}

// ── Time helpers (deterministic: `now` injectable for tests) ─────────────────

export function relTime(iso: string, now: number = Date.now()): string {
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return "—";
  const d = now - t;
  if (d < 45_000) return "vừa xong";
  if (d < 3_600_000) return `${Math.floor(d / 60_000)} phút trước`;
  if (d < 86_400_000) return `${Math.floor(d / 3_600_000)} giờ trước`;
  return `${Math.floor(d / 86_400_000)} ngày trước`;
}

export function runDurationMs(run: Pick<WorkflowRun, "startedAt" | "finishedAt"> & { status?: string }): number | null {
  if (run.status === "running") return null; // finished_at = started_at until FinishRun: no real duration yet
  const a = new Date(run.startedAt).getTime();
  const b = new Date(run.finishedAt).getTime();
  if (!Number.isFinite(a) || !Number.isFinite(b) || b < a) return null;
  return b - a;
}

export function fmtDuration(ms: number | null): string {
  if (ms === null) return "—";
  if (ms < 1000) return `${ms}ms`;
  // VN decimal comma; strip trailing ",0"
  return `${(ms / 1000).toFixed(1).replace(".", ",").replace(/,0$/, "")}s`;
}

// ── Dry-run trace → step view model ──────────────────────────────────────────

export interface TraceRow {
  label: string;
  value: string;
  mono?: boolean;
}

// pass = will run / matched · fail = predicate false / would-not-send · blocked = WF-01 or
// action error · skipped = action not evaluated (an earlier condition failed).
export type TraceVerdict = "pass" | "fail" | "blocked" | "skipped";

export interface ShapedTraceStep {
  step: number;
  kind: "trigger" | "condition" | "action";
  title: string;
  verdict: TraceVerdict;
  verdictLabel: string;
  rows: TraceRow[];
  payload?: string; // pretty-printed JSON (webhook preview)
  error?: string;
  // The action would run, but not the way its config reads (set_elapsed on the wall-clock fallback).
  warning?: string;
}

// Two shipped action types were missing here, so a run/dry-run row showed the raw engine token
// (`create_record`, `assign`) where every other row showed Vietnamese — the machine's vocabulary
// on the exact screen a builder reads to decide whether a rule is safe to switch on.
const ACTION_TITLES: Record<string, string> = {
  field_update: "Cập nhật trường",
  in_app_notification: "Thông báo trong app",
  email_alert: "Gửi email",
  outbound_webhook: "Gọi webhook",
  create_record: "Tạo bản ghi mới",
  assign: "Giao việc",
  set_deadline: "Đặt hạn xử lý",
  set_elapsed: t("page.workflow.trace.elapsed.title"),
  veto: t("workflow.action.veto"),
  normalize: "Chuẩn hoá chữ",
};

/** An action type in plain Vietnamese — the dry-run trace and the run-history detail. */
export function actionTitle(type: string): string {
  return ACTION_TITLES[type] ?? type;
}

const viNum = new Intl.NumberFormat("vi-VN", { maximumFractionDigits: 1 });

/**
 * set_elapsed's number as a person reads it: "1.234 phút (≈ 20,6 giờ làm việc)". The hours are
 * "giờ làm việc" only when the minutes came through the org's working calendar; on the no-calendar
 * fallback (`wallClock`) they are plain hours. Under an hour the minutes say it all.
 */
function elapsedMinutesVi(v: unknown, wallClock: boolean): string {
  if (typeof v !== "number" || !Number.isFinite(v)) return "—";
  const n = viNum.format(v);
  if (v < 60) return t("page.workflow.trace.elapsed.minutes").replace("{n}", n);
  const key = wallClock ? "page.workflow.trace.elapsed.wallHours" : "page.workflow.trace.elapsed.bizHours";
  return t(key).replace("{n}", n).replace("{h}", viNum.format(v / 60));
}

// Offset units in plain Vietnamese for the dry-run trace. Kept local (a Record, not an import from
// workflowTimer) so this pure display module stays free of the builder's config types.
const UNIT_LABELS_VI: Record<string, string> = {
  minutes: "phút",
  hours: "giờ",
  days: "ngày",
  business_hours: "giờ làm việc",
};

function unitLabelVi(u: string): string {
  return UNIT_LABELS_VI[u] ?? u;
}

/**
 * Render a workflow-trace instant in Vietnam time (D6: a VN product never shows a UTC clock).
 *
 * Two shapes arrive side by side in the SAME set_deadline row and they parse differently:
 *   - `after`  is RFC3339 from the engine ("2026-09-11T08:23:30Z") — `new Date` reads it as UTC.
 *   - `before` is the PERSISTED value, the MySQL literal in UTC ("2026-09-11 08:23:30") — which
 *     `new Date` reads as the VIEWER'S LOCAL time, quietly shifting it by the offset.
 * Printing them raw put a UTC ISO string next to a UTC-looking literal inside the one widget whose
 * entire job is letting a person compare an instant against what they meant: a "+4 giờ" rule
 * rehearsed at 22:00 VN read back as 19:00 — seven hours off, and on the previous day.
 */
export function traceInstantVN(v: unknown): string {
  if (typeof v !== "string" || v.trim() === "") return traceValue(v);
  const s = v.trim();
  // MySQL literal (no 'T', no zone) → say it is UTC before handing it to Date.
  const iso = /^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/.test(s) ? s.replace(" ", "T") + "Z" : s;
  const d = new Date(iso);
  return isNaN(d.getTime()) ? s : formatDateTime(d);
}

const OP_DISPLAY: Record<string, string> = {
  eq: "=",
  neq: "≠",
  gt: ">",
  gte: "≥",
  lt: "<",
  lte: "≤",
  contains: "chứa",
  isnull: "rỗng",
  notnull: "không rỗng",
  changed: "vừa đổi",
  changedTo: "vừa đổi thành",
};

// Stringify an arbitrary trace value for display. Never "undefined"/"null" literals.
export function traceValue(v: unknown): string {
  if (v === null || v === undefined) return "—";
  if (typeof v === "string") return v === "" ? "(rỗng)" : v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

function prettyJSON(v: unknown): string | undefined {
  if (v === null || v === undefined) return undefined;
  if (typeof v === "string") {
    // Payload may arrive pre-serialized — re-indent when parseable.
    try {
      return JSON.stringify(JSON.parse(v), null, 2);
    } catch {
      return v;
    }
  }
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}

/**
 * `calendarNames` (id → name) lets a set_deadline pinned to its own calendar (queue #79) name the
 * calendar it walked; without it the id is shown.
 */
export function shapeTraceStep(s: WorkflowTestStep, calendarNames: Record<string, string> = {}): ShapedTraceStep {
  const d = s.detail ?? {};
  const err = typeof d.error === "string" && d.error !== "" ? d.error : undefined;

  if (s.kind === "trigger") {
    const ok = s.matched === true;
    return {
      step: s.step,
      kind: "trigger",
      title: "Trigger",
      verdict: ok ? "pass" : "fail",
      verdictLabel: ok ? "Khớp" : "Không khớp",
      rows: [{ label: "Sự kiện", value: triggerLabel(traceValue(d.trigger)), mono: false }],
      error: err,
    };
  }

  if (s.kind === "condition") {
    const ok = s.matched === true;
    // No condition rules → single { matchAll: true } step (§9.4).
    if (d.matchAll === true) {
      return {
        step: s.step,
        kind: "condition",
        title: "Điều kiện",
        verdict: "pass",
        verdictLabel: "Luôn khớp",
        rows: [{ label: "Điều kiện", value: "Không có điều kiện — luôn khớp" }],
        error: err,
      };
    }
    return {
      step: s.step,
      kind: "condition",
      title: "Điều kiện",
      verdict: ok ? "pass" : "fail",
      verdictLabel: ok ? "Khớp" : "Không khớp",
      rows: [
        { label: "Trường", value: traceValue(d.field), mono: true },
        { label: "Toán tử", value: OP_DISPLAY[String(d.op)] ?? traceValue(d.op) },
        { label: "Kỳ vọng", value: traceValue(d.value) },
        { label: "Thực tế", value: traceValue(d.actual) },
      ],
      error: err,
    };
  }

  // Action step
  const type = String(d.type ?? "");
  const title = type ? actionTitle(type) : "Hành động";
  const would =
    d.wouldApply === false || d.wouldSend === false
      ? false
      : d.wouldApply === true || d.wouldSend === true
        ? true
        : undefined;

  let verdict: TraceVerdict;
  let verdictLabel: string;
  if (err) {
    verdict = "blocked";
    verdictLabel = "Bị chặn";
  } else if (s.evaluated === false) {
    verdict = "skipped";
    verdictLabel = "Không chạy";
  } else if (type === "veto" && d.wouldBlock === true) {
    // specs/048: a before rule's veto — the save would be refused (an error tone, not a failed step).
    verdict = "blocked";
    verdictLabel = t("workflow.test.before.willBlockVerdict");
  } else if (would === false) {
    verdict = "fail";
    verdictLabel = "Không thực hiện";
  } else {
    verdict = "pass";
    verdictLabel = "Sẽ chạy";
  }

  let rows: TraceRow[] = [];
  let payload: string | undefined;
  let warning: string | undefined;
  switch (type) {
    case "field_update":
      // specs/058: through a relation the dry run reads the related id, not the related record —
      // there is no honest "Trước" to show; an unlinked record's reason travels as `note`.
      rows = d.childEntity
        ? [
            { label: t("page.workflow.trace.children.of"), value: `${traceValue(d.childEntity)} · ${traceValue(d.childVia)}`, mono: true },
            { label: "Trường", value: traceValue(d.field), mono: true },
            { label: "Sau", value: traceValue(d.after) },
            { label: t("page.workflow.trace.children.budget"), value: traceValue(d.budget) },
          ]
        : d.via
        ? [
            { label: t("page.workflow.trace.related.via"), value: `${traceValue(d.via)} · ${traceValue(d.relatedId)}`, mono: true },
            { label: "Trường", value: traceValue(d.field), mono: true },
            { label: "Sau", value: traceValue(d.after) },
          ]
        : [
            { label: "Trường", value: traceValue(d.field), mono: true },
            { label: "Trước", value: traceValue(d.before) },
            { label: "Sau", value: traceValue(d.after) },
          ];
      if (typeof d.note === "string" && d.note) warning = d.note;
      break;
    case "veto":
      rows = [{ label: "Thông điệp", value: traceValue(d.message) }];
      if (typeof d.field === "string" && d.field) rows.unshift({ label: "Gắn dưới trường", value: d.field, mono: true });
      break;
    case "normalize":
      rows = [
        { label: "Trường", value: traceValue(d.field), mono: true },
        { label: "Trước", value: traceValue(d.before) },
        { label: "Sau", value: traceValue(d.after) },
      ];
      break;
    case "email_alert":
      rows = [
        { label: "Đến", value: traceValue(d.to), mono: true },
        { label: "Tiêu đề", value: traceValue(d.subject) },
        { label: "Nội dung", value: traceValue(d.body) },
      ];
      break;
    case "in_app_notification":
      rows = [
        { label: "Người nhận", value: traceValue(d.userId), mono: true },
        { label: "Tiêu đề", value: traceValue(d.title) },
        { label: "Nội dung", value: traceValue(d.message) },
      ];
      break;
    case "outbound_webhook":
      rows = [{ label: "URL", value: traceValue(d.url), mono: true }];
      payload = prettyJSON(d.payload);
      break;
    case "set_deadline": {
      // Without this arm the default below would label the rows with the raw wire keys
      // ("field"/"before"/"offset"/"after") and render the offset as `{"amount":4,"unit":"hours"}`.
      // The whole point of previewing a deadline is that a person can compare the instant against
      // what they meant, so the instant has to read like a date and not like a payload.
      const off = (d.offset ?? {}) as { amount?: unknown; unit?: unknown };
      // `keep` (queue #81) = tighten_only computed an instant but policy leaves the record alone.
      // The instant is then NOT what gets written, so it must not be labelled "Hạn sẽ đặt".
      const kept = typeof d.keep === "string" && d.keep !== "";
      if (kept && !err) verdictLabel = "Giữ hạn cũ";
      rows = [
        { label: "Trường", value: traceValue(d.field), mono: true },
        { label: "Cộng thêm", value: off.amount === undefined ? "—" : `${off.amount} ${unitLabelVi(String(off.unit ?? ""))}` },
        { label: "Trước", value: traceInstantVN(d.before) },
        { label: kept ? "Hạn tính được" : "Hạn sẽ đặt", value: traceInstantVN(d.after), mono: true },
      ];
      if (kept) rows.push({ label: "Giữ hạn cũ vì", value: String(d.keep) });
      // Absent anchor/mode = the pre-#81 defaults (fire instant, overwrite): no row, as before.
      if (d.anchor !== undefined) {
        rows.push({ label: "Mốc tính", value: d.anchor === "created_at" ? "Lúc tạo bản ghi" : d.anchor === "now" ? "Lúc luật chạy" : traceValue(d.anchor) });
      }
      if (d.mode !== undefined) {
        rows.push({
          label: "Chế độ",
          value: d.mode === "tighten_only" ? "Chỉ rút ngắn — không lùi hạn đã hứa, không đè hạn gõ tay"
            : d.mode === "overwrite" ? "Ghi đè" : traceValue(d.mode),
        });
      }
      if (d.stampField !== undefined) rows.push({ label: "Dấu hạn máy", value: traceValue(d.stampField), mono: true });
      // The rows above are a FIXED list, so a key the backend adds later lands nowhere — the
      // `default` arm that dumps unknown keys is unreachable once an action has its own case.
      // `calendar` is exactly that: the backend sets it when a business_hours deadline fell back
      // to wall-clock because the org has no working calendar, and it is the ONLY thing telling
      // the two apart on screen — the instant itself looks perfectly normal either way.
      // Queue #79: the calendar a pinned deadline walked. "12 giờ làm việc → 16:34 thứ Tư" on a
      // 24/7 pin reads like an arithmetic bug unless the trace says which clock was used.
      if (typeof d.calendarId === "string" && d.calendarId !== "") {
        const calName = calendarNames[d.calendarId];
        rows.push(calName ? { label: "Lịch ghim", value: calName } : { label: "Lịch ghim", value: d.calendarId, mono: true });
      }
      if (d.calendar !== undefined) rows.push({ label: "Lịch làm việc", value: traceValue(d.calendar) });
      break;
    }
    case "set_elapsed": {
      // "Sẽ ghi 1.234 phút, tính từ lúc tạo 16:00 thứ Sáu" — `from` is RFC3339 UTC, printed in VN
      // time like every other trace instant. `calendar` (the org has no working calendar, so the
      // minutes are wall-clock) is the only thing telling the two numbers apart: a warning, not a row.
      const wallClock = typeof d.calendar === "string" && d.calendar !== "";
      rows = [
        { label: t("page.workflow.trace.elapsed.field"), value: traceValue(d.field), mono: true },
        { label: t("page.workflow.trace.elapsed.before"), value: traceValue(d.before) },
        { label: t("page.workflow.trace.elapsed.from"), value: traceInstantVN(d.from) },
        { label: t("page.workflow.trace.elapsed.after"), value: elapsedMinutesVi(d.after, wallClock), mono: true },
      ];
      if (wallClock) warning = d.calendar as string;
      break;
    }
    default:
      // Unknown action type — dump detail keys honestly instead of hiding them.
      rows = Object.entries(d)
        .filter(([k]) => k !== "type" && k !== "error")
        .map(([k, v]) => ({ label: k, value: traceValue(v) }));
  }

  return { step: s.step, kind: "action", title, verdict, verdictLabel, rows, payload, error: err, ...(warning ? { warning } : {}) };
}
