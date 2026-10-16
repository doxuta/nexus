// TIMER-1 — date_offset workflow trigger helpers (WorkflowBuilder "Theo mốc thời gian").
// The wire/storage shape mirrors the backend exactly (sys_workflows.condition.timer):
//   { dateField, offset: { amount, unit }, direction, calendarId? }
// Backend validator: services.buildTimerConfig (codes WORKFLOW_TIMER_*).

import { t as tr } from "@/lib/i18n";

export interface WorkflowTimer {
  dateField: string;
  offset: { amount: number; unit: string };
  direction: string;
  calendarId?: string;
  /** Recurrence (optional). Absent = fire once — every rule written before this existed. */
  repeat?: WorkflowTimerRepeat;
}

/** Bounded recurrence: every `every` × `unit`, `count` times in total (the first fire included).
 *  Deliberately no "forever" and no `until` date — see workflow.TimerRepeat on the backend. */
export interface WorkflowTimerRepeat {
  every: number;
  unit: string;
  count: number;
}

// Repeat units mirror workflow.RepeatUnit*. Minutes/hours are absent on purpose: a reminder that
// repeats every few minutes is a polling loop, not a workflow.
export const REPEAT_UNITS = [
  { key: "days", label: "ngày" },
  { key: "weeks", label: "tuần" },
] as const;

/** Mirrors workflow.maxRepeatCount — the picker must not offer what the server refuses. */
export const MAX_REPEAT_COUNT = 60;

// Closed unit set — mirrors workflow.ValidTimerUnit. business_hours is "after"-only (v1).
export const TIMER_UNITS = [
  { key: "minutes", label: "phút" },
  { key: "hours", label: "giờ" },
  { key: "days", label: "ngày" },
  { key: "business_hours", label: "giờ làm việc" },
] as const;

export const TIMER_DIRECTIONS = [
  { key: "before", label: "trước" },
  { key: "after", label: "sau" },
] as const;

// The one system column accepted as anchor besides date/datetime fields (backend v1 contract).
export const TIMER_CREATED_AT = "created_at";
// Second system stamp (#287): "N days after the last edit" = nothing touched the record for N days.
export const TIMER_UPDATED_AT = "updated_at";

function unitLabel(unit: string): string {
  return TIMER_UNITS.find((u) => u.key === unit)?.label ?? unit;
}

function directionLabel(direction: string): string {
  return TIMER_DIRECTIONS.find((d) => d.key === direction)?.label ?? direction;
}

/**
 * Plain-language preview of a timer config: "3 ngày trước Ngày hết hạn".
 * fieldLabel is the human label of the anchor field (falls back to the raw name).
 */
export function timerPreview(t: WorkflowTimer, fieldLabel?: string): string {
  const system = t.dateField === TIMER_CREATED_AT ? tr("page.workflow.timer.createdAtShort") : t.dateField === TIMER_UPDATED_AT ? tr("page.workflow.timer.updatedAtShort") : t.dateField;
  const anchor = fieldLabel || system || "…";
  const amount = t.offset.amount > 0 ? t.offset.amount : "…";
  const once = `${amount} ${unitLabel(t.offset.unit)} ${directionLabel(t.direction)} ${anchor}`;
  if (!t.repeat) return once;
  const ru = REPEAT_UNITS.find((u) => u.key === t.repeat!.unit)?.label ?? t.repeat.unit;
  // Says the TOTAL, not "repeats N more times": a bounded recurrence is easier to reason about as
  // "5 lần" than as a first fire plus four follow-ups, and the count field means exactly that.
  return `${once}, rồi nhắc lại mỗi ${t.repeat.every} ${ru} — tổng ${t.repeat.count} lần`;
}

/** Parse the timer block out of a stored workflow's condition JSON (null-safe). */
export function timerFromCondition(condition: unknown): WorkflowTimer | null {
  if (!condition || typeof condition !== "object") return null;
  const t = (condition as { timer?: unknown }).timer;
  if (!t || typeof t !== "object") return null;
  const raw = t as { dateField?: unknown; offset?: { amount?: unknown; unit?: unknown }; direction?: unknown; calendarId?: unknown };
  return {
    dateField: typeof raw.dateField === "string" ? raw.dateField : "",
    offset: {
      amount: typeof raw.offset?.amount === "number" ? raw.offset.amount : 1,
      unit: typeof raw.offset?.unit === "string" ? raw.offset.unit : "days",
    },
    direction: typeof raw.direction === "string" ? raw.direction : "before",
    calendarId: typeof raw.calendarId === "string" && raw.calendarId ? raw.calendarId : undefined,
    repeat: parseRepeat((raw as { repeat?: unknown }).repeat),
  };
}

/** Parse the optional recurrence block. Anything malformed reads as "no recurrence" rather than a
 *  guessed one — a reminder that fires more often than configured is worse than one that does not
 *  repeat at all. */
function parseRepeat(v: unknown): WorkflowTimerRepeat | undefined {
  if (!v || typeof v !== "object") return undefined;
  const r = v as { every?: unknown; unit?: unknown; count?: unknown };
  if (typeof r.every !== "number" || typeof r.count !== "number" || typeof r.unit !== "string") return undefined;
  return { every: r.every, unit: r.unit, count: r.count };
}

// ---------------------------------------------------------------------------
// set_deadline action (DAILY-QUEUE #9) — the ACTION that computes a commitment
// instant, as opposed to the date_offset TRIGGER above that schedules a wake-up.
// ---------------------------------------------------------------------------

/** The set_deadline action's config: how far AFTER the fire instant the deadline lands, and the
 *  datetime field it is written into. Mirrors workflow.Action{Field, Offset}. */
export interface WorkflowDeadline {
  field: string;
  offset: { amount: number; unit: string };
}

/**
 * Units the set_deadline ACTION accepts. Since DAILY-QUEUE #10 threaded the tenant's working
 * calendar into the action, this is the SAME vocabulary the date_offset trigger uses (backend:
 * workflow.ValidTimerUnit) — `business_hours` included. Keep the two equal: a unit offered here
 * but refused there gives SET_DEADLINE_UNIT_INVALID on save, and a unit missing here is a
 * capability the admin cannot reach.
 */
export const DEADLINE_UNITS = TIMER_UNITS;

/** Plain-language preview of a deadline action: "4 giờ sau khi tạo, ghi vào Hạn xử lý". */
export function deadlinePreview(d: WorkflowDeadline, fieldLabel?: string): string {
  const amount = d.offset.amount > 0 ? d.offset.amount : "…";
  const target = fieldLabel || d.field || "…";
  return `${amount} ${unitLabel(d.offset.unit)} kể từ lúc luật chạy, ghi vào ${target}`;
}

/**
 * Read a stored set_deadline action back into the builder's flat string params.
 *
 * The builder's generic hydration stringifies every non-`type` key, which turns the nested
 * `offset` object into the literal "[object Object]" — the amount and unit would be gone the
 * moment an admin re-opened the rule, and saving again would send a rule with no offset. This is
 * why the action needs its own arm.
 */
export function deadlineParamsFromAction(a: Record<string, unknown>): Record<string, string> {
  const off = (a.offset ?? {}) as { amount?: unknown; unit?: unknown };
  const params: Record<string, string> = {
    field: typeof a.field === "string" ? a.field : "",
    amount: typeof off.amount === "number" ? String(off.amount) : "",
    unit: typeof off.unit === "string" ? off.unit : "hours",
  };
  // #81 policy params (edited by the set_deadline card, FE-BPM-QUEUE-WF #137) — carried so a
  // re-save sends them back; dropping `mode` turns a shorten-only rule into an overwrite.
  for (const k of DEADLINE_POLICY_KEYS) {
    if (typeof a[k] === "string" && a[k]) params[k] = a[k] as string;
  }
  return params;
}

/**
 * set_deadline's optional params kept outside offset/field — #81's policy (mode, stampField, anchor;
 * no control on this screen) and #79's pinned working calendar (calendarId; the "Lịch làm việc"
 * dropdown). Absent = the backend default.
 */
export const DEADLINE_POLICY_KEYS = ["mode", "stampField", "anchor", "calendarId"] as const;

/** The policy params present in the builder's flat params, ready to spread into the payload. */
export function deadlinePolicyPayload(params: Record<string, string>): Record<string, string> {
  // A pin only means something to business_hours — the server refuses it on any other unit
  // (SET_DEADLINE_CALENDAR_INVALID) and the dropdown that clears it is hidden there, so a unit
  // switch drops it.
  const carried = (k: (typeof DEADLINE_POLICY_KEYS)[number]) =>
    Boolean(params[k]) && (k !== "calendarId" || params.unit === "business_hours");
  return Object.fromEntries(DEADLINE_POLICY_KEYS.filter(carried).map((k) => [k, params[k]]));
}
