// calendarGrid — pure date maths for the week/day calendar (DAILY-QUEUE #288). Datetimes are stored in
// UTC but a VN product reads them in Asia/Ho_Chi_Minh (lib/format.ts D6), a fixed +07:00 with no DST,
// so the grid does plain arithmetic instead of trusting the browser zone.
import { parseServerDateTime } from "@/lib/format";

const VN_OFFSET_MIN = 7 * 60;
/** How long an event block is drawn (the engine has no end-time field): overlap + height unit. */
export const EVENT_MINUTES = 30;

const pad = (n: number) => String(n).padStart(2, "0");
const fmtDay = (d: Date) => `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}`;
const dayDate = (day: string) => new Date(`${day}T00:00:00Z`);

export function addDays(day: string, n: number): string {
  const d = dayDate(day);
  d.setUTCDate(d.getUTCDate() + n);
  return fmtDay(d);
}

/** Monday of the week holding `day`. */
export function weekStart(day: string): string {
  return addDays(day, -((dayDate(day).getUTCDay() + 6) % 7));
}

export interface When { day: string; minutes: number | null }

/** Calendar day (and minute-of-day for datetime) in VN time; null when the value is not a date. */
export function eventWhen(raw: unknown, fieldType: "date" | "datetime"): When | null {
  if (!raw) return null;
  const s = String(raw);
  if (fieldType === "date") return /^\d{4}-\d{2}-\d{2}/.test(s) ? { day: s.slice(0, 10), minutes: null } : null;
  if (!/^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}/.test(s)) return null;
  const d = parseServerDateTime(s);
  if (isNaN(d.getTime())) return null;
  const local = new Date(d.getTime() + VN_OFFSET_MIN * 60_000);
  return { day: fmtDay(local), minutes: local.getUTCHours() * 60 + local.getUTCMinutes() };
}

/** `[between]` operand for the visible days. A datetime filter is compared as an instant (UTC), so the
 *  VN day span is converted — and the end must run to the last second of the last day, not its 00:00. */
export function rangeFilter(fieldType: "date" | "datetime", startDay: string, endDay: string): string {
  if (fieldType === "date") return `${startDay},${endDay}`;
  return `${addDays(startDay, -1)}T17:00:00Z,${endDay}T16:59:59Z`;
}

/** Value to PUT when an event lands on `day` (+ minute-of-day for datetime). */
export function toWire(fieldType: "date" | "datetime", day: string, minutes: number | null): string {
  if (fieldType === "date") return day;
  const m = minutes ?? 0;
  return `${day}T${pad(Math.floor(m / 60))}:${pad(m % 60)}:00+07:00`;
}

/** [firstHour, endHour) of the time axis: office hours, widened to hold every event. */
export function hourWindow(minutes: number[]): [number, number] {
  let from = 7, to = 19;
  for (const m of minutes) {
    from = Math.min(from, Math.floor(m / 60));
    to = Math.max(to, Math.floor((m + EVENT_MINUTES - 1) / 60) + 1);
  }
  return [from, Math.min(to, 24)];
}

/** Side-by-side lanes for events that overlap in time (each is drawn EVENT_MINUTES long). */
export function assignLanes(events: { id: string; minutes: number }[]): Map<string, { lane: number; lanes: number }> {
  const sorted = [...events].sort((a, b) => a.minutes - b.minutes || a.id.localeCompare(b.id));
  const out = new Map<string, { lane: number; lanes: number }>();
  let cluster: string[] = [];
  let laneEnds: number[] = [];
  const flush = () => {
    for (const id of cluster) out.get(id)!.lanes = laneEnds.length;
    cluster = []; laneEnds = [];
  };
  for (const e of sorted) {
    if (cluster.length && laneEnds.every((end) => end <= e.minutes)) flush();
    let lane = laneEnds.findIndex((end) => end <= e.minutes);
    if (lane < 0) { lane = laneEnds.length; laneEnds.push(0); }
    laneEnds[lane] = e.minutes + EVENT_MINUTES;
    out.set(e.id, { lane, lanes: 1 });
    cluster.push(e.id);
  }
  flush();
  return out;
}

/** Today's calendar day in VN time, whatever the browser zone (events are read in VN time too). */
export function todayVN(now: number = Date.now()): string {
  return fmtDay(new Date(now + VN_OFFSET_MIN * 60_000));
}
