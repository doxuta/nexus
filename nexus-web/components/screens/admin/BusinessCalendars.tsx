"use client";

// Lịch làm việc — business-hours calendars (CALENDAR-1, api-conventions §17).
// BE contract (nexus-platform/internal/handlers/admin_calendar_handler.go — đã đối chiếu):
//   GET/POST /admin/business-calendars · PUT/DELETE /admin/business-calendars/:id
// Một lịch = timezone IANA + khoảng giờ làm việc từng thứ trong tuần (nhiều khoảng/ngày —
// nghỉ trưa) + danh sách ngày lễ. Đây là NỀN của SLA/timer engine (lát kế): deadline tính
// theo GIỜ LÀM VIỆC thay vì giờ đồng hồ treo tường. Tenant chưa có lịch default → engine
// dùng wall-clock (ghi rõ trong copy màn hình để admin hiểu hệ quả của cờ "mặc định").
//
// SLDS (specs/038 T048, RESEARCH §5 /admin/business-calendars): list = page-headers/base + one card
// per calendar (badge "Mặc định", edit button, delete button-icon → prompt); form = the
// RecordFormPage template (page header, expandable sections, docked Huỷ/Lưu footer) with input +
// combobox (time zone) + checkbox-toggle (default), the week as an slds-grid of timepicker pairs,
// holidays as pills + a datepicker.

import { useEffect, useRef, useState, type FormEvent } from "react";
import { flushSync } from "react-dom";
import { CalendarClock, Plus, Trash2, X } from "lucide-react";
import {
  listBusinessCalendars, createBusinessCalendar, updateBusinessCalendar, deleteBusinessCalendar,
  ensureAuth, ApiError,
  type BusinessCalendar, type BusinessCalendarInput, type CalendarHours, type CalendarInterval,
} from "@/lib/admin";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { toast } from "@/components/ui/Toaster";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Card } from "@/components/slds/Card";
import { Combobox } from "@/components/slds/Combobox";
import { DatePicker, formatDisplayDate, parseISODate } from "@/components/slds/DatePicker";
import { EmptyState } from "@/components/slds/EmptyState";
import { Input } from "@/components/slds/Input";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { PageHeader } from "@/components/slds/PageHeader";
import { Pill, PillContainer } from "@/components/slds/Pill";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { TimePicker } from "@/components/slds/TimePicker";
import { Toggle } from "@/components/slds/Toggle";
import { RecordFormPage } from "@/components/slds/templates/RecordFormPage";

// Thứ trong tuần theo key hours_json của BE (mon..sun) — thứ tự hiển thị VN.
export const WEEKDAYS: { key: string; label: string }[] = ["mon", "tue", "wed", "thu", "fri", "sat", "sun"].map(
  (key) => ({ key, label: t(`page.calendars.day.${key}` as I18nKey) }),
);

// Danh sách IANA curated (đủ dùng cho khách Nexus; BE validate mọi tên IANA hợp lệ).
export const TIMEZONES = [
  "Asia/Ho_Chi_Minh", "Asia/Bangkok", "Asia/Singapore", "Asia/Jakarta", "Asia/Manila",
  "Asia/Tokyo", "Asia/Seoul", "Asia/Shanghai", "Asia/Kolkata", "Asia/Dubai",
  "Europe/London", "Europe/Paris", "Europe/Berlin", "America/New_York", "America/Chicago",
  "America/Denver", "America/Los_Angeles", "Australia/Sydney", "UTC",
];

const CAL_ICON = { glyph: CalendarClock, name: "business-hours" };

// Giờ hành chính VN — mẫu khởi tạo form (admin sửa tiếp).
function defaultHours(): CalendarHours {
  const day = (): CalendarInterval[] => [
    { start: "08:00", end: "12:00" },
    { start: "13:00", end: "17:00" },
  ];
  return { mon: day(), tue: day(), wed: day(), thu: day(), fri: day(), sat: [], sun: [] };
}

/** Số ngày có giờ làm việc trong tuần — dòng tóm tắt của list. */
export function workingDayCount(hours: CalendarHours): number {
  return WEEKDAYS.filter((d) => (hours[d.key] ?? []).length > 0).length;
}

// BE error codes with a friendly Vietnamese line (page.calendars.err.<CODE>); fallback = server message.
const KNOWN_ERRORS = new Set([
  "CALENDAR_NAME_REQUIRED", "CALENDAR_FIELD_TOO_LONG", "CALENDAR_TIMEZONE_INVALID", "CALENDAR_HOURS_INVALID",
  "CALENDAR_HOURS_EMPTY", "CALENDAR_HOLIDAYS_INVALID", "CALENDAR_ALREADY_EXISTS", "CALENDAR_NOT_FOUND",
]);

function errMsg(e: unknown, fallback: string): string {
  if (e instanceof ApiError) {
    return KNOWN_ERRORS.has(e.code) ? t(`page.calendars.err.${e.code}` as I18nKey) : (e.message || fallback);
  }
  return fallback;
}

/** "2026-09-02" → "02/09/2026" (the date pickers' display format); anything else as-is. */
function displayDate(iso: string): string {
  const d = parseISODate(iso);
  return d ? formatDisplayDate(d) : iso;
}

type View = "list" | "form";

/** `gate` (Studio): wraps a save so the page can ask in place first; absent = save at once. */
export function BusinessCalendars({ gate }: { gate?: (save: () => void) => void } = {}) {
  const [view, setView] = useState<View>("list");
  const [editing, setEditing] = useState<BusinessCalendar | null>(null);
  const [calendars, setCalendars] = useState<BusinessCalendar[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  // The effect sets state only after its first await (react-hooks/set-state-in-effect); a reload
  // shows the spinner first, then bumps the tick that re-runs it.
  const [reloadTick, setReloadTick] = useState(0);
  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        await ensureAuth();
        const cals = await listBusinessCalendars();
        if (alive) setCalendars(cals);
      } catch (e) {
        if (alive) setErr(errMsg(e, t("page.calendars.loadError")));
      } finally {
        if (alive) setLoading(false);
      }
    })();
    return () => { alive = false; };
  }, [reloadTick]);

  const reload = () => {
    setLoading(true);
    setErr(null);
    setReloadTick((n) => n + 1);
  };

  if (view === "form") {
    return (
      <CalendarForm
        calendar={editing}
        gate={gate}
        onBack={() => { setView("list"); setEditing(null); reload(); }}
      />
    );
  }

  let body: React.ReactNode;
  if (loading) {
    body = <Card title={t("page.calendars.loading")} hideHeader loading />;
  } else if (err) {
    body = <ScopedNotification theme="error">{err}</ScopedNotification>;
  } else if (calendars.length === 0) {
    body = (
      <Card title={t("page.calendars.title")} hideHeader>
        <EmptyState
          className="slds-p-vertical_large"
          icon={CalendarClock}
          title={t("page.calendars.empty.title")}
          description={t("page.calendars.empty.desc")}
        />
      </Card>
    );
  } else {
    body = (
      <>
        {/* Each card in its own grid cell: sibling cards would pick up SLDS `.slds-card+.slds-card`
            margin-top on top of the grid gap and sit out of line. */}
        <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
          {calendars.map((cal) => (
            <div key={cal.id}>
              <CalendarCard
                calendar={cal}
                onEdit={() => { setEditing(cal); setView("form"); }}
                onChanged={reload}
              />
            </div>
          ))}
        </div>
        <p className="slds-text-body_small slds-text-color_weak">{t("page.calendars.footnote")}</p>
      </>
    );
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
      <div className="flex flex-col gap-4 p-4">
        <PageHeader
          icon={{ ...CAL_ICON, label: t("page.calendars.title") }}
          title={t("page.calendars.title")}
          meta={tf("page.calendars.meta", { n: calendars.length })}
          actions={[
            <Button key="create" variant="brand" iconLeft={Plus} onClick={() => { setEditing(null); setView("form"); }}>
              {t("page.calendars.create")}
            </Button>,
          ]}
        />
        <p className="slds-text-body_small slds-text-color_weak">{t("studio.build.automation.calendar.shared")}</p>
        {body}
      </div>
    </div>
  );
}

// ── Một thẻ lịch: tên + tz + tóm tắt tuần + số ngày lễ + sửa/xoá ───────────────────────────────
function CalendarCard({ calendar: cal, onEdit, onChanged }: {
  calendar: BusinessCalendar;
  onEdit: () => void;
  onChanged: () => void;
}) {
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [deleting, setDeleting] = useState(false);
  // Kept in the dialog (not a toast): a CALENDAR_IN_USE refusal names the rules to unpin, and an
  // admin needs that sentence to stay readable while they go fix them (DAILY-QUEUE #326).
  const [deleteErr, setDeleteErr] = useState<string | null>(null);
  const days = workingDayCount(cal.hours ?? {});
  const holidayCount = (cal.holidays ?? []).length;

  async function handleDelete() {
    setDeleting(true);
    setDeleteErr(null);
    try {
      await deleteBusinessCalendar(cal.id);
      toast.success(t("page.calendars.deleted"));
      setConfirmOpen(false);
      onChanged();
    } catch (e) {
      setDeleteErr(errMsg(e, t("page.calendars.deleteFailed")));
    } finally {
      setDeleting(false);
    }
  }

  return (
    <Card
      className="h-full"
      title={cal.name}
      icon={CAL_ICON}
      actions={
        <div className="flex items-center gap-2">
          <Button onClick={onEdit} aria-label={tf("page.calendars.editLabel", { name: cal.name })}>
            {t("page.calendars.edit")}
          </Button>
          <ButtonIcon
            icon={Trash2}
            variant="border-filled"
            label={tf("page.calendars.deleteLabel", { name: cal.name })}
            onClick={() => setConfirmOpen(true)}
          />
        </div>
      }
    >
      <div className="flex flex-wrap items-center gap-2">
        {cal.isDefault && <Badge theme="success">{t("page.calendars.default")}</Badge>}
        <span className="slds-text-color_weak">
          {tf("page.calendars.summary", { tz: cal.timezone, days, holidays: holidayCount })}
        </span>
      </div>

      {confirmOpen && (
        <div className="slds-m-top_small">
          <InlineConfirm
            tone="error"
            message={`${tf("page.calendars.delete.title", { name: cal.name })} ${t(cal.isDefault ? "page.calendars.delete.default" : "page.calendars.delete.plain")}`}
            confirmLabel={deleting ? t("action.deleting") : t("studio.build.automation.calendar.deleteGo")}
            busy={deleting}
            onConfirm={handleDelete}
            onCancel={() => { setConfirmOpen(false); setDeleteErr(null); }}
          >
            {deleteErr && <ScopedNotification theme="error">{deleteErr}</ScopedNotification>}
          </InlineConfirm>
        </div>
      )}
    </Card>
  );
}

// ── Form tạo/sửa: tên + tz + editor khoảng giờ theo thứ + ngày lễ + cờ mặc định ─────────────────
function CalendarForm({ calendar, onBack, gate }: { calendar: BusinessCalendar | null; onBack: () => void; gate?: (save: () => void) => void }) {
  const isEdit = calendar != null;
  const [name, setName] = useState(calendar?.name ?? "");
  const [nameErr, setNameErr] = useState<string | null>(null);
  const nameRef = useRef<HTMLInputElement>(null);
  const [timezone, setTimezone] = useState(calendar?.timezone ?? "Asia/Ho_Chi_Minh");
  const [isDefault, setIsDefault] = useState(calendar?.isDefault ?? false);
  const [hours, setHours] = useState<CalendarHours>(() => {
    if (!calendar) return defaultHours();
    const h: CalendarHours = {};
    for (const d of WEEKDAYS) h[d.key] = (calendar.hours?.[d.key] ?? []).map((iv) => ({ ...iv }));
    return h;
  });
  const [holidays, setHolidays] = useState<string[]>(calendar?.holidays ?? []);
  const [newHoliday, setNewHoliday] = useState("");
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  function setInterval(day: string, idx: number, patch: Partial<CalendarInterval>) {
    setHours((h) => ({
      ...h,
      [day]: (h[day] ?? []).map((iv, i) => (i === idx ? { ...iv, ...patch } : iv)),
    }));
  }
  function addInterval(day: string) {
    setHours((h) => {
      const cur = h[day] ?? [];
      const last = cur[cur.length - 1];
      // Khoảng mới nối sau khoảng cuối (hoặc giờ hành chính khi ngày đang nghỉ).
      const next: CalendarInterval = last ? { start: last.end, end: "17:00" } : { start: "08:00", end: "17:00" };
      return { ...h, [day]: [...cur, next] };
    });
  }
  function removeInterval(day: string, idx: number) {
    setHours((h) => ({ ...h, [day]: (h[day] ?? []).filter((_, i) => i !== idx) }));
  }
  function addHoliday() {
    const v = newHoliday.trim();
    if (!v) return;
    if (!holidays.includes(v)) setHolidays((hs) => [...hs, v].sort());
    setNewHoliday("");
  }

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    if (!name.trim()) {
      // Focus moves to the field so a screen reader reads its error (aria-describedby) instead of
      // silence on Lưu. flushSync first: a collapsed "Thông tin chung" reopens (hasError) before focus.
      flushSync(() => setNameErr(t("page.calendars.form.nameRequired")));
      nameRef.current?.focus();
      return;
    }
    (gate ?? ((go) => go()))(() => void doSave());
  }

  async function doSave() {
    setSaving(true);
    setErr(null);
    const input: BusinessCalendarInput = {
      name: name.trim(),
      timezone,
      hours,
      holidays,
      isDefault,
    };
    try {
      if (isEdit) {
        await updateBusinessCalendar(calendar.id, input);
        toast.success(t("page.calendars.saved"));
      } else {
        await createBusinessCalendar(input);
        toast.success(t("page.calendars.created"));
      }
      onBack();
    } catch (ex) {
      setErr(errMsg(ex, t(isEdit ? "page.calendars.saveFailed" : "page.calendars.createFailed")));
    } finally {
      setSaving(false);
    }
  }

  const general = (
    <div className="slds-form grid grid-cols-1 gap-4 slds-p-horizontal_small sm:grid-cols-2">
      <Input
        ref={nameRef}
        label={t("page.calendars.form.name")}
        required
        maxLength={80}
        value={name}
        placeholder={t("page.calendars.form.namePlaceholder")}
        error={nameErr ?? undefined}
        onChange={(e) => { setName(e.target.value); setNameErr(null); }}
      />
      <Combobox
        label={t("page.calendars.form.timezone")}
        required
        options={TIMEZONES.map((tz) => ({ value: tz, label: tz }))}
        value={timezone}
        onChange={(v) => { if (v) setTimezone(v); }}
      />
      <Toggle
        className="sm:col-span-2"
        label={t("page.calendars.form.isDefault")}
        help={t("page.calendars.form.isDefaultHelp")}
        checked={isDefault}
        onChange={(e) => setIsDefault(e.target.checked)}
      />
    </div>
  );

  const week = (
    <div className="flex flex-col slds-p-horizontal_small">
      {WEEKDAYS.map((d) => {
        const intervals = hours[d.key] ?? [];
        return (
          <div key={d.key} className="flex flex-wrap items-center gap-2 slds-border_bottom slds-p-vertical_x-small">
            <span className="w-20 shrink-0 slds-text-title_bold">{d.label}</span>
            {intervals.length === 0 && <span className="slds-text-color_weak">{t("page.calendars.form.dayOff")}</span>}
            {intervals.map((iv, i) => (
              <div key={i} className="flex items-center gap-1">
                <TimePicker
                  className="w-28"
                  label={tf("page.calendars.form.start", { day: d.label, n: i + 1 })}
                  labelHidden
                  value={iv.start || null}
                  onChange={(v) => setInterval(d.key, i, { start: v ?? "" })}
                />
                <span aria-hidden="true">–</span>
                <TimePicker
                  className="w-28"
                  label={tf("page.calendars.form.end", { day: d.label, n: i + 1 })}
                  labelHidden
                  // "24:00" (end of day, BE-valid) has no slot in a 24h picker: shown as 23:59 and
                  // kept as-is until the admin picks another time (same as the old native input).
                  value={iv.end === "24:00" ? "23:59" : (iv.end || null)}
                  onChange={(v) => setInterval(d.key, i, { end: v ?? "" })}
                />
                <ButtonIcon
                  icon={X}
                  label={tf("page.calendars.form.removeInterval", { n: i + 1, day: d.label })}
                  onClick={() => removeInterval(d.key, i)}
                />
              </div>
            ))}
            <Button
              variant="base"
              iconLeft={Plus}
              aria-label={tf("page.calendars.form.addIntervalFor", { day: d.label })}
              onClick={() => addInterval(d.key)}
            >
              {t("page.calendars.form.addInterval")}
            </Button>
          </div>
        );
      })}
      <p className="slds-form-element__help slds-text-color_weak slds-m-top_x-small">{t("page.calendars.form.weekHelp")}</p>
    </div>
  );

  const holidayBody = (
    <div className="flex flex-col gap-3 slds-p-horizontal_small">
      {holidays.length > 0 ? (
        <PillContainer variant="listbox" label={t("page.calendars.form.holidayList")}>
          {holidays.map((h) => (
            <Pill
              key={h}
              label={displayDate(h)}
              removeLabel={t("page.calendars.form.removeHoliday")}
              onRemove={() => setHolidays((hs) => hs.filter((x) => x !== h))}
            />
          ))}
        </PillContainer>
      ) : (
        <p className="slds-text-color_weak">{t("page.calendars.form.noHolidays")}</p>
      )}
      <div className="flex flex-wrap items-end gap-2">
        <DatePicker
          label={t("page.calendars.form.addHoliday")}
          value={newHoliday || null}
          onChange={(v) => setNewHoliday(v ?? "")}
        />
        <Button onClick={addHoliday} disabled={!newHoliday}>{t("page.calendars.form.add")}</Button>
      </div>
    </div>
  );

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
      <div className="p-4">
        <RecordFormPage
          icon={CAL_ICON}
          objectLabel={t("page.calendars.title")}
          title={isEdit ? tf("page.calendars.form.editTitle", { name: calendar.name }) : t("page.calendars.form.createTitle")}
          meta={t("page.calendars.form.meta")}
          notification={err ? {
            tone: "error",
            body: (
              <span className="flex items-start gap-2">
                <span className="min-w-0 flex-1">{err}</span>
                <ButtonIcon icon={X} variant="inverse" label={t("page.calendars.form.dismissError")} onClick={() => setErr(null)} />
              </span>
            ),
          } : undefined}
          sections={[
            { id: "general", title: t("page.calendars.form.general"), body: general, hasError: Boolean(nameErr) },
            { id: "week", title: t("page.calendars.form.week"), body: week },
            { id: "holidays", title: t("page.calendars.form.holidays"), body: holidayBody },
          ]}
          onSubmit={handleSubmit}
          onCancel={onBack}
          saving={saving}
          submitLabel={t(isEdit ? "page.calendars.form.submitEdit" : "page.calendars.form.submitCreate")}
        />
      </div>
    </div>
  );
}
