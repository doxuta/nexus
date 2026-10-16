"use client";

// CalendarView — CALENDAR-VIEW (Wave 4.2) + week/day views (DAILY-QUEUE #288). Owns its data: one range
// query per visible span (<dateField>[between]=start,end — the engine's bracket filter), so every record
// in the span shows regardless of the list's page size. Three ways to look: Tháng (month grid), Tuần and
// Ngày (an hour axis, only when the field is a datetime — a bare date has no clock, so there the week/day
// are all-day columns). Click-a-day/slot → quick-create prefilled with that date (and hour), and
// drag-a-record → PUT {<dateField>: newValue} to reschedule, all metadata-driven. The span is laid out on
// the layout's declared `calendarField` (DAILY-QUEUE #5) when it names a date/datetime field this user
// actually has, and otherwise on the first date/datetime field. An event is titled by recordLabel, which
// follows the table's chosen name field (#272, `isLabel`) — relation names are fetched with ?expand.
//
// SLDS 2.264.1 has no calendar blueprint (RESEARCH §5.2), so it is composed (specs/038 T022): the toolbar
// is the datepicker's month filter row (title + prev/next button icons, "Hôm nay") plus a button-group
// for the mode, the month/all-day grid is a data-tables table (slds-table_bordered + col-bordered, fixed
// layout, no row hover) with weeks as rows the way the datepicker's month table lays them out, the hour
// axis is bordered rows (one droppable per hour) with events as linked `pills` pills overlaid, and the
// slot under a dragged pill shows the `drop-zone` "Drag Over" state. Behaviour is @dnd-kit. Keyboard:
// the record link keeps its tab stop (Enter opens it); the grip in the pill's trailing-button slot picks
// the pill up (Space/Enter), arrows jump it a slot, Space drops it (lib/dndKeyboard). Times are VN time
// (lib/calendarGrid).
import { useCallback, useMemo, useState, type MouseEvent, type PointerEventHandler } from "react";
import clsx from "clsx";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Calendar as CalendarIcon, ChevronLeft, ChevronRight, GripVertical, Plus } from "lucide-react";
import {
  DndContext, DragOverlay, PointerSensor, KeyboardSensor, useSensor, useSensors, useDraggable, useDroppable,
  type DragEndEvent,
} from "@dnd-kit/core";
import {
  ApiError, ensureAuth, listRecordsPage, updateRecord, type ApiRecord, type FieldMeta, type ListPage,
} from "@/lib/api";
import { formatSaveError } from "@/lib/saveErrors";
import { dndAccessibility, jumpToDroppable, pointerOrClosest } from "@/lib/dndKeyboard";
import {
  addDays, assignLanes, eventWhen, EVENT_MINUTES, hourWindow, rangeFilter, toWire, todayVN, weekStart, type When,
} from "@/lib/calendarGrid";
import { formatDate } from "@/lib/format";
import { t } from "@/lib/i18n";
import { recordLabel } from "@/lib/recordLabel";
import { usePermissions } from "@/lib/metadata/permissions";
import { toast } from "@/components/ui/Toaster";
import { QuickCreateModal } from "@/components/layout/QuickCreateModal";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { ButtonGroup } from "@/components/slds/ButtonGroup";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { EmptyState } from "@/components/slds/EmptyState";
import { Spinner } from "@/components/slds/Spinner";

const fill = (key: Parameters<typeof t>[0], values: Record<string, string | number>) =>
  t(key).replace(/\{(\w+)\}/g, (m, k: string) => (k in values ? String(values[k]) : m));

// Local YYYY-MM-DD (never toISOString — that shifts by the UTC offset and lands events on the
// wrong day for VN users). The engine's date filter + date field both speak this format.
function ymd(d: Date): string {
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}
const pad2 = (n: number) => String(n).padStart(2, "0");
const hhmm = (minutes: number) => `${pad2(Math.floor(minutes / 60))}:${pad2(minutes % 60)}`;
const dayNumOf = (day: string) => Number(day.slice(8, 10));
/** Monday=0 … Sunday=6 */
const dowOf = (day: string) => (new Date(`${day}T00:00:00Z`).getUTCDay() + 6) % 7;
const ddmm = (day: string) => `${day.slice(8, 10)}/${day.slice(5, 7)}`;

// Monday-first week: the kit datepicker's short weekday names are Sunday-first ("CN,T2,…,T7").
const WEEKDAYS = (() => {
  const short = t("slds.datepicker.weekdaysShort").split(",");
  const long = t("slds.datepicker.weekdaysLong").split(",");
  const monFirst = (xs: string[]) => [...xs.slice(1), xs[0]];
  return monFirst(short).map((s, i) => ({ short: s, long: monFirst(long)[i] }));
})();

const PILLS_PER_DAY = 3;
const ROW_H = 56; // px per hour on the time axis
const SLOT_SEP = "@"; // droppable id of an hour slot: "<day>@<minutes>"

type Mode = "month" | "week" | "day";
const MODES: Mode[] = ["month", "week", "day"];
type DateKind = "date" | "datetime";

export interface CalendarViewProps {
  entity: string;
  fields: FieldMeta[];
  onOpen?: (id: string) => void;
  /** Date field the layout DECLARED to lay the month out on (layout.views.list.calendarField,
   *  DAILY-QUEUE #5). Undefined, or a name that is not a date field here, falls back to the first
   *  date/datetime field. The declared field needs NO index: the range query filters with
   *  `[between]` (a JSON path) and still sorts by created_at. */
  dateFieldName?: string;
  /** A host that opens a full page to create (the Studio frame) gets the date field and its wire value instead of the quick-create modal. */
  onCreateAt?: (field: string, value: string) => void;
}

interface CreateAt { day: string; minutes: number | null }

export function CalendarView({ entity, fields, onOpen, dateFieldName, onCreateAt }: CalendarViewProps) {
  const { can } = usePermissions();
  const canEdit = can("edit", entity);
  // Creating from a day needs `create`, not `edit` (the old view gated it on edit: a create-only
  // role saw no "+", an edit-only role got a 403 on save).
  const canCreate = can("create", entity);
  const qc = useQueryClient();
  const isDateField = (f: FieldMeta) => f.fieldType === "date" || f.fieldType === "datetime";
  const dateField = useMemo(
    () =>
      (dateFieldName ? fields.find((f) => f.name === dateFieldName && isDateField(f)) : undefined) ??
      fields.find(isDateField),
    [fields, dateFieldName],
  );
  const kind: DateKind = dateField?.fieldType === "datetime" ? "datetime" : "date";

  // A clinic front desk lives in the week: a datetime field opens on Tuần, a plain date on Tháng.
  const [pickedMode, setMode] = useState<Mode | null>(null);
  const mode: Mode = pickedMode ?? (kind === "datetime" ? "week" : "month");
  const [cursor, setCursor] = useState(todayVN); // any day inside the visible span
  const todayKey = todayVN();

  const [startDay, endDay] = useMemo<[string, string]>(() => {
    if (mode === "day") return [cursor, cursor];
    if (mode === "week") { const s = weekStart(cursor); return [s, addDays(s, 6)]; }
    const y = Number(cursor.slice(0, 4)), m = Number(cursor.slice(5, 7));
    return [`${cursor.slice(0, 8)}01`, ymd(new Date(y, m, 0))];
  }, [mode, cursor]);

  const [createAt, setCreateAt] = useState<CreateAt | null>(null);

  // Relation values come back as bare ids; ?expand turns them into {id,label} so the title can be a
  // patient/doctor NAME when the table's name field (or a fallback) is one.
  const expandNames = useMemo(() => fields.filter((f) => f.fieldType === "relation").map((f) => f.name), [fields]);

  const { data, isFetching, isError } = useQuery({
    queryKey: ["calendar", entity, dateField?.name ?? "", startDay, endDay, expandNames.join(",")],
    queryFn: async () => {
      await ensureAuth();
      // sort=created_at, NOT the date field: a plain (non-indexed) date field fails RM-09's
      // sortable gate → 422 SORT_NOT_ALLOWED and the whole span silently rendered EMPTY
      // (the DP-4.4 sweep caught it live). The grid groups by day client-side anyway.
      const q = new URLSearchParams({ limit: "200", sort: "created_at", order: "asc" });
      q.set(`${dateField!.name}[between]`, rangeFilter(kind, startDay, endDay));
      if (expandNames.length) q.set("expand", expandNames.join(","));
      return listRecordsPage(entity, q.toString());
    },
    enabled: !!dateField,
    staleTime: 15_000,
    retry: false,
  });
  const records = useMemo(() => data?.records ?? [], [data]);
  const total = data?.pagination.total ?? records.length;

  const whenOf = useCallback(
    (r: ApiRecord): When | null => (dateField ? eventWhen(r.data[dateField.name], kind) : null),
    [dateField, kind],
  );

  const byDay = useMemo(() => {
    const m: Record<string, ApiRecord[]> = {};
    for (const r of records) {
      const w = whenOf(r);
      if (w) (m[w.day] ??= []).push(r);
    }
    // Earliest first inside a day (records without a clock keep arrival order).
    for (const list of Object.values(m)) list.sort((a, b) => (whenOf(a)?.minutes ?? 0) - (whenOf(b)?.minutes ?? 0));
    return m;
  }, [records, whenOf]);

  const reschedule = useCallback((rec: ApiRecord, toDay: string, toMinutes?: number) => {
    if (!dateField) return;
    const from = whenOf(rec);
    // Dropped on a bare day (month grid / all-day): the clock part stays what it was. Dropped on an
    // hour slot: the hour moves, the minutes (09:40) stay; the same hour on the same day is a no-op,
    // so a sloppy drag never rewrites a booking's time.
    const keep = from?.minutes != null ? from.minutes % 60 : 0;
    if (toMinutes !== undefined && from && from.day === toDay && from.minutes != null && Math.floor(from.minutes / 60) === Math.floor(toMinutes / 60)) return;
    const minutes = toMinutes !== undefined ? toMinutes + keep : from?.minutes ?? 0;
    if (from && from.day === toDay && (kind === "date" || from.minutes === minutes)) return;
    const newVal = toWire(kind, toDay, minutes);
    // Optimistic: the pill lands on its new slot in the drop's own render, so dnd-kit's focus return
    // after a keyboard drop finds the grip there (it would otherwise go to the old slot's grip, which
    // the refetch then removes). A failed save refetches and puts it back.
    qc.setQueriesData<ListPage>({ queryKey: ["calendar", entity, dateField.name] }, (old) =>
      old && { ...old, records: old.records.map((r) => (r.id === rec.id ? { ...r, data: { ...r.data, [dateField.name]: newVal } } : r)) },
    );
    (async () => {
      try {
        await ensureAuth();
        await updateRecord(entity, rec.id, { [dateField.name]: newVal });
        const when = kind === "datetime" && toMinutes !== undefined ? `${formatDate(toDay)} ${hhmm(minutes)}` : toDay;
        toast.success(fill("page.list.calendar.moved", { record: recordLabel(rec, fields), day: when }));
        qc.invalidateQueries({ queryKey: ["calendar", entity, dateField.name] });
      } catch (err) {
        // A refusal the server explains (a before-save rule's sentence, 403, 503 "busy") is shown as
        // said; only a transport failure keeps the generic line.
        toast.error(err instanceof ApiError ? fill("page.list.calendar.moveRefused", { msg: formatSaveError(err) }) : t("page.list.calendar.moveFailed"));
        qc.invalidateQueries({ queryKey: ["calendar", entity, dateField.name] });
      }
    })();
  }, [dateField, entity, fields, qc, whenOf, kind]);

  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 5 } }),
    useSensor(KeyboardSensor, { coordinateGetter: jumpToDroppable }),
  );
  const [dragId, setDragId] = useState<string | null>(null);
  const onDragEnd = useCallback((e: DragEndEvent) => {
    setDragId(null);
    const rec = records.find((r) => r.id === String(e.active.id));
    if (!rec || !e.over) return;
    const [day, min] = String(e.over.id).split(SLOT_SEP);
    reschedule(rec, day, min === undefined ? undefined : Number(min));
  }, [records, reschedule]);
  const dragged = dragId ? records.find((r) => r.id === dragId) : undefined;
  const accessibility = dndAccessibility(
    (id) => {
      const rec = records.find((r) => r.id === String(id));
      return rec ? recordLabel(rec, fields) : String(id);
    },
    (id) => {
      const [day, min] = String(id).split(SLOT_SEP);
      return min === undefined ? day : fill("page.list.calendar.hourLabel", { day, time: hhmm(Number(min)) });
    },
  );

  if (!dateField) {
    return (
      <div className="slds-border_top slds-p-around_medium">
        <EmptyState icon={CalendarIcon} title={t("page.list.calendar.noField")} description={t("page.list.calendar.noFieldHint")} />
      </div>
    );
  }

  const step = (dir: 1 | -1) => {
    if (mode === "day") setCursor(addDays(cursor, dir));
    else if (mode === "week") setCursor(addDays(cursor, 7 * dir));
    else setCursor(ymd(new Date(Number(cursor.slice(0, 4)), Number(cursor.slice(5, 7)) - 1 + dir, 1)));
  };

  const timed = kind === "datetime" && mode !== "month";
  const days = mode === "day" ? [cursor] : Array.from({ length: 7 }, (_, i) => addDays(startDay, i));
  const monthTitle = `${fill("slds.datepicker.month", { n: Number(startDay.slice(5, 7)) })} ${startDay.slice(0, 4)}`;
  const title =
    mode === "month" ? monthTitle
    : mode === "week" ? fill("page.list.calendar.weekTitle", { from: ddmm(startDay), to: formatDate(endDay) })
    : fill("page.list.calendar.dayTitle", { weekday: WEEKDAYS[dowOf(cursor)].long, date: formatDate(cursor) });
  const gridLabel =
    mode === "month" ? fill("page.list.calendar.label", { month: monthTitle })
    : mode === "week" ? fill("page.list.calendar.weekLabel", { range: `${ddmm(startDay)} – ${formatDate(endDay)}` })
    : fill("page.list.calendar.dayLabel", { date: formatDate(cursor) });

  // All-day table rows (month, and week/day of a plain date field): weeks of day keys, null = padding.
  let weeks: (string | null)[][] = [];
  if (!timed) {
    if (mode === "month") {
      const cells: (string | null)[] = [
        ...Array.from({ length: dowOf(startDay) }, () => null),
        ...Array.from({ length: dayNumOf(endDay) }, (_, i) => addDays(startDay, i)),
      ];
      while (cells.length % 7) cells.push(null);
      weeks = Array.from({ length: cells.length / 7 }, (_, w) => cells.slice(w * 7, w * 7 + 7));
    } else weeks = [days];
  }
  const headers = mode === "day" ? [WEEKDAYS[dowOf(cursor)]] : WEEKDAYS;
  const perDay = mode === "month" ? PILLS_PER_DAY : 50;
  const create = (day: string, minutes: number | null) =>
    onCreateAt && dateField ? onCreateAt(dateField.name, toWire(kind, day, minutes)) : setCreateAt({ day, minutes });

  return (
    <DndContext
      sensors={sensors}
      collisionDetection={pointerOrClosest}
      accessibility={accessibility}
      onDragStart={(e) => setDragId(String(e.active.id))}
      onDragEnd={onDragEnd}
      onDragCancel={() => setDragId(null)}
    >
      <div className="slds-border_top">
        {/* Toolbar — the datepicker's filter row: title, then mode · prev · Hôm nay · next */}
        <div className="slds-grid slds-grid_vertical-align-center slds-grid_align-spread slds-p-around_small">
          <div className="slds-grid slds-grid_vertical-align-center">
            <h2 className="slds-text-heading_small" aria-live="polite">{title}</h2>
            {isFetching && <Spinner size="x-small" inline className="slds-m-left_small" />}
            {/* DP-4.4: a failed query must never render as a silently-empty calendar */}
            {isError && <span className="slds-text-color_error slds-m-left_small">{t("page.list.calendar.loadError")}</span>}
          </div>
          <div className="slds-grid slds-grid_vertical-align-center">
            <ButtonGroup label={t("page.list.calendar.mode")} className="slds-m-right_small">
              {MODES.map((m) => (
                <Button key={m} variant={m === mode ? "brand" : "neutral"} aria-pressed={m === mode} onClick={() => setMode(m)}>
                  {t(`page.list.calendar.mode.${m}`)}
                </Button>
              ))}
            </ButtonGroup>
            <ButtonIcon icon={ChevronLeft} variant="border-filled" label={t(`page.list.calendar.prev.${mode}`)} onClick={() => step(-1)} />
            <Button className="slds-m-horizontal_xx-small" onClick={() => setCursor(todayVN())}>
              {t("slds.datepicker.today")}
            </Button>
            <ButtonIcon icon={ChevronRight} variant="border-filled" label={t(`page.list.calendar.next.${mode}`)} onClick={() => step(1)} />
          </div>
        </div>
        {/* A span with more rows than one page must say so, never quietly drop the rest */}
        {total > records.length && (
          <p className="slds-text-color_weak slds-p-horizontal_small slds-p-bottom_x-small">
            {fill("page.list.calendar.truncated", { n: records.length })}
          </p>
        )}

        {timed ? (
          <TimeGrid
            label={gridLabel}
            days={days}
            byDay={byDay}
            whenOf={whenOf}
            fields={fields}
            entity={entity}
            todayKey={todayKey}
            canDrag={canEdit}
            canCreate={canCreate}
            onOpen={onOpen}
            onCreate={create}
          />
        ) : (
          <table className="slds-table slds-table_bordered slds-table_col-bordered slds-table_fixed-layout slds-no-row-hover" aria-label={gridLabel}>
            <thead>
              <tr className="slds-line-height_reset">
                {headers.map((d) => (
                  <th key={d.short} scope="col" className="slds-text-align_center">
                    <abbr title={d.long}>{d.short}</abbr>
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {weeks.map((week, w) => (
                <tr key={w}>
                  {week.map((day, i) =>
                    day === null ? (
                      <td key={`pad-${w}-${i}`} className="h-24" />
                    ) : (
                      <CalendarDay
                        key={day}
                        dayKey={day}
                        isToday={day === todayKey}
                        records={byDay[day] ?? []}
                        max={perDay}
                        whenOf={whenOf}
                        fields={fields}
                        entity={entity}
                        canDrag={canEdit}
                        canCreate={canCreate}
                        onOpen={onOpen}
                        onCreate={() => create(day, null)}
                      />
                    ),
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {/* The moving copy; the pill left in its slot hides while lifted. */}
      <DragOverlay>
        {dragged ? (
          <span className="slds-pill">
            <span className="slds-pill__label">{recordLabel(dragged, fields)}</span>
          </span>
        ) : null}
      </DragOverlay>

      {createAt && (
        <QuickCreateModal
          entity={entity}
          open
          prefill={{ [dateField.name]: toWire(kind, createAt.day, createAt.minutes) }}
          onClose={() => setCreateAt(null)}
          onSaved={() => {
            setCreateAt(null);
            qc.invalidateQueries({ queryKey: ["calendar", entity, dateField.name] });
          }}
        />
      )}
    </DndContext>
  );
}

interface CellProps {
  whenOf: (r: ApiRecord) => When | null;
  fields: FieldMeta[];
  entity: string;
  canDrag: boolean;
  canCreate: boolean;
  onOpen?: (id: string) => void;
}

function CalendarDay({
  dayKey, isToday, records, max, whenOf, fields, entity, canDrag, canCreate, onOpen, onCreate,
}: CellProps & {
  dayKey: string;
  isToday: boolean;
  records: ApiRecord[];
  max: number;
  onCreate: () => void;
}) {
  const { setNodeRef, isOver } = useDroppable({ id: dayKey, disabled: !canDrag });
  const dayNum = dayNumOf(dayKey);
  return (
    <td
      ref={setNodeRef}
      className={clsx("slds-align-top slds-hint-parent h-24", isOver && "slds-drop-zone slds-drop-zone_drag")}
      aria-current={isToday ? "date" : undefined}
    >
      <div className="slds-grid slds-grid_vertical-align-center slds-grid_align-spread">
        {isToday ? <Badge variant="inverse">{dayNum}</Badge> : <span className="slds-text-body_small slds-text-color_weak">{dayNum}</span>}
        {canCreate && (
          <ButtonIcon icon={Plus} size="x-small" hint label={fill("page.list.calendar.createOn", { day: dayKey })} onClick={onCreate} />
        )}
      </div>
      <ul className="slds-m-top_xx-small">
        {records.slice(0, max).map((r) => (
          <li key={r.id}>
            <CalendarPill record={r} fields={fields} href={`/${entity}/${r.id}`} canDrag={canDrag} onOpen={onOpen} minutes={whenOf(r)?.minutes ?? null} />
          </li>
        ))}
      </ul>
      {records.length > max && (
        <span className="slds-text-body_small slds-text-color_weak">{fill("page.list.calendar.more", { n: records.length - max })}</span>
      )}
    </td>
  );
}

// Week/day on an hour axis: a header row (weekday · date · "+"), then one column per day with a
// droppable per hour and the events overlaid at their minute. The axis is office hours, widened to
// hold the earliest/latest event of the span, so nothing is ever drawn off-grid.
function TimeGrid({
  label, days, byDay, whenOf, fields, entity, todayKey, canDrag, canCreate, onOpen, onCreate,
}: CellProps & {
  label: string;
  days: string[];
  byDay: Record<string, ApiRecord[]>;
  todayKey: string;
  onCreate: (day: string, minutes: number) => void;
}) {
  const [from, to] = hourWindow(
    days.flatMap((d) => (byDay[d] ?? []).map((r) => whenOf(r)?.minutes).filter((m): m is number => m != null)),
  );
  const hours = Array.from({ length: to - from }, (_, i) => from + i);
  const cols = { gridTemplateColumns: `3.5rem repeat(${days.length}, minmax(0, 1fr))` };
  return (
    <div role="group" aria-label={label} className="slds-border_top">
      <div className="grid slds-border_bottom" style={cols}>
        <div />
        {days.map((d) => (
          <div key={d} className="slds-border_left slds-p-vertical_x-small slds-grid slds-grid_vertical-align-center slds-grid_align-center" aria-current={d === todayKey ? "date" : undefined}>
            <abbr title={WEEKDAYS[dowOf(d)].long} className="slds-m-right_x-small">{WEEKDAYS[dowOf(d)].short}</abbr>
            {d === todayKey ? <Badge variant="inverse">{dayNumOf(d)}</Badge> : <span className="slds-text-color_weak">{dayNumOf(d)}</span>}
            {canCreate && (
              <ButtonIcon
                icon={Plus} size="x-small" hint className="slds-m-left_xx-small"
                label={fill("page.list.calendar.createAt", { time: hhmm(Math.max(from, 9) * 60), day: formatDate(d) })}
                onClick={() => onCreate(d, Math.max(from, 9) * 60)}
              />
            )}
          </div>
        ))}
      </div>
      <div className="grid" style={cols}>
        <div>
          {hours.map((h) => (
            <div key={h} style={{ height: ROW_H }} className="slds-text-body_small slds-text-color_weak slds-text-align_right slds-p-right_x-small">
              {hhmm(h * 60)}
            </div>
          ))}
        </div>
        {days.map((d) => (
          <TimeColumn
            key={d} day={d} from={from} hours={hours} records={byDay[d] ?? []} whenOf={whenOf}
            fields={fields} entity={entity} canDrag={canDrag} canCreate={canCreate} onOpen={onOpen} onCreate={onCreate}
          />
        ))}
      </div>
    </div>
  );
}

function TimeColumn({
  day, from, hours, records, whenOf, fields, entity, canDrag, canCreate, onOpen, onCreate,
}: CellProps & {
  day: string;
  from: number;
  hours: number[];
  records: ApiRecord[];
  onCreate: (day: string, minutes: number) => void;
}) {
  const timed = records.flatMap((r) => {
    const m = whenOf(r)?.minutes;
    return m == null ? [] : [{ r, id: r.id, minutes: m }];
  });
  const lanes = assignLanes(timed);
  return (
    <div className="relative slds-border_left">
      {hours.map((h) => (
        <TimeSlot key={h} day={day} minutes={h * 60} canDrag={canDrag} canCreate={canCreate} onCreate={onCreate} />
      ))}
      {/* Events sit above the slots; only the pills take the pointer, the gaps fall through to the slot. */}
      <div className="absolute inset-0 pointer-events-none">
        {timed.map(({ r, id, minutes }) => {
          const { lane, lanes: n } = lanes.get(id)!;
          return (
            <div
              key={id}
              className="absolute pointer-events-auto overflow-hidden"
              style={{
                top: ((minutes - from * 60) / 60) * ROW_H,
                height: (EVENT_MINUTES / 60) * ROW_H - 2,
                left: `${(lane / n) * 100}%`,
                width: `${100 / n}%`,
              }}
            >
              <CalendarPill record={r} fields={fields} href={`/${entity}/${r.id}`} canDrag={canDrag} onOpen={onOpen} minutes={minutes} />
            </div>
          );
        })}
      </div>
    </div>
  );
}

function TimeSlot({
  day, minutes, canDrag, canCreate, onCreate,
}: {
  day: string;
  minutes: number;
  canDrag: boolean;
  canCreate: boolean;
  onCreate: (day: string, minutes: number) => void;
}) {
  const { setNodeRef, isOver } = useDroppable({ id: `${day}${SLOT_SEP}${minutes}`, disabled: !canDrag });
  return (
    <div
      ref={setNodeRef}
      style={{ height: ROW_H }}
      className={clsx("slds-border_bottom", canCreate && "cursor-pointer", isOver && "slds-drop-zone slds-drop-zone_drag")}
      title={canCreate ? t("page.list.calendar.slotHint") : undefined}
      onClick={canCreate ? () => onCreate(day, minutes) : undefined}
    />
  );
}

// A linked pill (pills blueprint `slds-pill_link`). Pointer: a press anywhere on the pill that moves
// ≥5px drags it, a plain click on the link opens the record. Keyboard: the link is a normal tab stop
// (Enter opens), and the drag handle is its own button in the pill's trailing-button slot
// (`slds-pill__remove`, where the blueprint puts the pill's one icon button) — the grip, like the
// board cards'. No role=button wraps the link. `minutes` (datetime only) prefixes the clock.
function CalendarPill({
  record, fields, href, canDrag, onOpen, minutes,
}: {
  record: ApiRecord;
  fields: FieldMeta[];
  href: string;
  canDrag: boolean;
  onOpen?: (id: string) => void;
  minutes: number | null;
}) {
  const { attributes, listeners, setNodeRef, setActivatorNodeRef, isDragging } = useDraggable({ id: record.id, disabled: !canDrag });
  const label = recordLabel(record, fields);
  const text = minutes == null ? label : `${hhmm(minutes)} ${label}`;
  const open = (e: MouseEvent<HTMLAnchorElement>) => {
    if (!onOpen || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    onOpen(record.id);
  };
  return (
    <span
      ref={setNodeRef}
      onPointerDown={canDrag ? (listeners?.onPointerDown as PointerEventHandler | undefined) : undefined}
      className={clsx("slds-pill slds-pill_link max-w-full", canDrag && "cursor-grab touch-none", isDragging && "slds-hidden")}
    >
      <a href={href} className="slds-pill__action" title={text} onClick={open}>
        <span className="slds-pill__label">{text}</span>
      </a>
      {canDrag && (
        <ButtonIcon
          ref={setActivatorNodeRef}
          {...attributes}
          {...listeners}
          icon={GripVertical}
          hint
          label={fill("page.list.calendar.drag", { record: label })}
          className="slds-pill__remove"
        />
      )}
    </span>
  );
}
