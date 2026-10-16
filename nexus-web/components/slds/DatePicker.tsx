"use client";

// SLDS date pickers (blueprint `datepickers` base + range, SLDS 2.264.1):
//   div.slds-form-element.slds-dropdown-trigger.slds-dropdown-trigger_click[.slds-is-open] > label
//     + div.slds-form-element__control.slds-input-has-icon.slds-input-has-icon_right > input.slds-input + button (calendar)
//     + div.slds-form-element__help.slds-assistive-text (format)
//     + div.slds-datepicker.slds-dropdown.slds-dropdown_left[role=dialog] >
//         div.slds-datepicker__filter (prev · h2 month · next | year select)
//         + table.slds-datepicker__month[role=grid] > thead th > abbr · tbody tr > td[role=gridcell] > span.slds-day
//         + button.slds-text-link "Hôm nay"
// react-day-picker supplies the calendar maths and keyboard (arrows, Home/End, PageUp/PageDown, Shift =
// year); every part it renders is swapped for the blueprint element. The blueprint puts focus on the
// td itself (SLDS styles `td:focus > .slds-day`), so the custom Day lifts DayButton's tabindex,
// label and handlers onto the td and adds Enter/Space (a td does not click natively).
// Radix Popover places the calendar (not portaled, like Menu), loops Tab inside it and dismisses it.
// Docs rules kept: a click on the input opens it without moving focus; the button (Enter/Space) opens
// it with focus on the selected day, else today; closing returns focus to the input, never the button.
// Values are ISO "yyyy-MM-dd", built from local dates (never `new Date(iso)`, which is UTC midnight).
// Deviation: aria-multiselectable is only set on the range grid (the base blueprint sets it on a
// single-date grid too, which tells screen readers several days can be picked).
import * as PopoverPrimitive from "@radix-ui/react-popover";
import clsx from "clsx";
import { format, isSameDay, isSameWeek, startOfDay } from "date-fns";
import { CalendarDays, ChevronLeft, ChevronRight } from "lucide-react";
import {
  createContext,
  isValidElement,
  useContext,
  useEffect,
  useId,
  useRef,
  useState,
  type HTMLAttributes,
  type KeyboardEvent,
  type MouseEvent,
  type ReactNode,
  type Ref,
} from "react";
import {
  DayPicker,
  useDayPicker,
  type DayProps,
  type Matcher,
  type MonthCaptionProps,
  type MonthGridProps,
  type WeekProps,
  type WeekdayProps,
} from "react-day-picker";
import { t } from "@/lib/i18n";
import { ButtonIcon } from "./ButtonIcon";
import { RequiredMark, describedBy, useFieldIds, type FieldProps } from "./FormElement";

// ── dates ────────────────────────────────────────────────────────────────────────────────────────
function makeDate(y: number, m: number, d: number) {
  const date = new Date(y, m - 1, d);
  return date.getFullYear() === y && date.getMonth() === m - 1 && date.getDate() === d ? date : undefined;
}
/** Local date for "yyyy-MM-dd"; undefined for anything else. */
export function parseISODate(iso?: string | null) {
  const m = iso ? /^(\d{4})-(\d{2})-(\d{2})$/.exec(iso) : null;
  return m ? makeDate(+m[1], +m[2], +m[3]) : undefined;
}
export const toISODate = (d: Date) => format(d, "yyyy-MM-dd");
export const formatDisplayDate = (d: Date) => format(d, "dd/MM/yyyy");
/** dd/MM/yyyy as typed: "24/06/2026", "3/7/2026", "03-07-2026", "03.07.2026", "03072026". */
export function parseDisplayDate(text: string) {
  const s = text.trim();
  const m = /^(\d{1,2})[/.-](\d{1,2})[/.-](\d{4})$/.exec(s) ?? /^(\d{2})(\d{2})(\d{4})$/.exec(s);
  return m ? makeDate(+m[3], +m[2], +m[1]) : undefined;
}

const monthName = (d: Date) => t("slds.datepicker.month").replace("{n}", String(d.getMonth() + 1));
const weekdayName = (d: Date, key: "slds.datepicker.weekdaysShort" | "slds.datepicker.weekdaysLong") => t(key).split(",")[d.getDay()];
const dayLabel = (d: Date) =>
  t("slds.datepicker.dayLabel").replace("{d}", String(d.getDate())).replace("{m}", String(d.getMonth() + 1)).replace("{y}", String(d.getFullYear()));

interface Span {
  from?: Date;
  to?: Date;
}
/** A lone end (only "to" picked so far, like a lone start) is a one-day span. */
function inSpan(d: Date, r: Span) {
  const start = r.from ?? r.to;
  const end = r.to ?? r.from;
  return start && end ? d >= startOfDay(start) && d <= startOfDay(end) : false;
}

// ── calendar parts (react-day-picker custom components) ──────────────────────────────────────────
interface CalendarCtx {
  monthId: string;
  fromYear: number;
  toYear: number;
  /** Range pickers: both ends, so either field's calendar shows the whole span. */
  range?: Span;
}
const Ctx = createContext<CalendarCtx>({ monthId: "", fromYear: 0, toYear: 0 });

const Pass = ({ children }: { children?: ReactNode }) => <>{children}</>;

function Caption({ calendarMonth }: MonthCaptionProps) {
  const { goToMonth, previousMonth, nextMonth } = useDayPicker();
  const { monthId, fromYear, toYear } = useContext(Ctx);
  const month = calendarMonth.date;
  const years: number[] = [];
  for (let y = fromYear; y <= toYear; y++) years.push(y);
  return (
    <div className="slds-datepicker__filter slds-grid">
      <div className="slds-datepicker__filter_month slds-grid slds-grid_align-spread slds-grow">
        <div className="slds-align-middle">
          <ButtonIcon
            icon={ChevronLeft}
            variant="container"
            label={t("slds.datepicker.prevMonth")}
            disabled={!previousMonth}
            onClick={() => previousMonth && goToMonth(previousMonth)}
          />
        </div>
        <h2 aria-atomic="false" aria-live="polite" className="slds-align-middle" id={monthId}>
          {monthName(month)}
        </h2>
        <div className="slds-align-middle">
          <ButtonIcon
            icon={ChevronRight}
            variant="container"
            label={t("slds.datepicker.nextMonth")}
            disabled={!nextMonth}
            onClick={() => nextMonth && goToMonth(nextMonth)}
          />
        </div>
      </div>
      <div className="slds-shrink-none">
        <label className="slds-assistive-text" htmlFor={`${monthId}-year`}>
          {t("slds.datepicker.pickYear")}
        </label>
        <div className="slds-select_container">
          <select
            className="slds-select"
            id={`${monthId}-year`}
            value={month.getFullYear()}
            onChange={(e) => goToMonth(new Date(Number(e.target.value), month.getMonth(), 1))}
          >
            {years.map((y) => (
              <option key={y} value={y}>
                {y}
              </option>
            ))}
          </select>
        </div>
      </div>
    </div>
  );
}

function Grid({ children }: MonthGridProps) {
  const { monthId, range } = useContext(Ctx);
  return (
    <table className="slds-datepicker__month" role="grid" aria-labelledby={monthId} aria-multiselectable={range ? true : undefined}>
      {children}
    </table>
  );
}

const Weekdays = ({ children }: { children?: ReactNode }) => (
  <thead>
    <tr>{children}</tr>
  </thead>
);

// react-day-picker names the column with the full weekday (labels.labelWeekday): it becomes the abbr title.
const Weekday = ({ children, "aria-label": full }: WeekdayProps) => (
  <th scope="col">
    <abbr title={full}>{children}</abbr>
  </th>
);

const Weeks = ({ children }: { children?: ReactNode }) => <tbody>{children}</tbody>;

function Week({ week, children }: WeekProps) {
  const { range } = useContext(Ctx);
  let rowClass: string | undefined;
  if (range && week.days.some((d) => inSpan(d.date, range)))
    rowClass = range.from && range.to && !isSameWeek(range.from, range.to, { weekStartsOn: 1 }) ? "slds-has-multi-row-selection" : "slds-has-multi-selection";
  return <tr className={rowClass}>{children}</tr>;
}

function Day({ day, modifiers, children }: DayProps) {
  const { range } = useContext(Ctx);
  const ref = useRef<HTMLTableCellElement>(null);
  // DayButton's own focus effect, moved to the td (react-day-picker drives it through modifiers.focused).
  // preventScroll: on open Radix still has the calendar parked off-screen, and a scrolling focus
  // would jump the page and flip the calendar above the field.
  useEffect(() => {
    if (modifiers.focused) ref.current?.focus({ preventScroll: true });
  }, [modifiers.focused]);
  const inRange = range ? inSpan(day.date, range) : false;
  const selected = range ? inRange : Boolean(modifiers.selected);
  const className =
    clsx(
      day.outside && "slds-day_adjacent-month",
      modifiers.today && "slds-is-today",
      selected && "slds-is-selected",
      inRange && range?.from && range.to && "slds-is-selected-multi",
      modifiers.disabled && "slds-disabled-text",
    ) || undefined;
  const button = isValidElement<HTMLAttributes<HTMLTableCellElement>>(children) ? children.props : undefined;
  if (!button)
    return (
      <td role="gridcell" aria-selected={selected} className={className}>
        <span className="slds-day">{children}</span>
      </td>
    );
  const { tabIndex, onClick, onKeyDown, onFocus, onBlur, onMouseEnter, onMouseLeave } = button;
  return (
    <td
      ref={ref}
      role="gridcell"
      aria-selected={selected}
      aria-current={modifiers.today ? "date" : undefined}
      aria-label={button["aria-label"]}
      aria-disabled={modifiers.disabled || undefined}
      tabIndex={tabIndex}
      className={className}
      onClick={onClick}
      onFocus={onFocus}
      onBlur={onBlur}
      onMouseEnter={onMouseEnter}
      onMouseLeave={onMouseLeave}
      onKeyDown={(e: KeyboardEvent<HTMLTableCellElement>) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onClick?.(e as unknown as MouseEvent<HTMLTableCellElement>);
        } else onKeyDown?.(e);
      }}
    >
      <span className="slds-day">{button.children}</span>
    </td>
  );
}

const COMPONENTS = { Root: Pass, Months: Pass, Month: Pass, MonthCaption: Caption, MonthGrid: Grid, Weekdays, Weekday, Weeks, Week, Day };
const FORMATTERS = { formatWeekdayName: (d: Date) => weekdayName(d, "slds.datepicker.weekdaysShort") };
const LABELS = {
  labelDayButton: (d: Date) => dayLabel(d),
  labelGridcell: (d: Date) => dayLabel(d),
  labelWeekday: (d: Date) => weekdayName(d, "slds.datepicker.weekdaysLong"),
};

interface CalendarProps extends HTMLAttributes<HTMLDivElement> {
  selected?: Date;
  onPick: (date: Date) => void;
  min?: Date;
  max?: Date;
  range?: Span;
  /** Opened from the button: focus the selected day (else today). */
  focusGrid: boolean;
  ref?: Ref<HTMLDivElement>;
}

/** div.slds-datepicker — the Radix Popover content element (it receives Radix's props and ref). */
function Calendar({ selected, onPick, min, max, range, focusGrid, className, ...rest }: CalendarProps) {
  const monthId = useId();
  const today = startOfDay(new Date());
  const [month, setMonth] = useState(() => selected ?? range?.from ?? range?.to ?? today);
  const disabled: Matcher[] = [];
  if (min) disabled.push({ before: min });
  if (max) disabled.push({ after: max });
  const fromYear = min?.getFullYear() ?? today.getFullYear() - 100;
  const toYear = max?.getFullYear() ?? today.getFullYear() + 20;
  const todayOff = (min && today < min) || (max && today > max);
  return (
    <div
      {...rest}
      role="dialog"
      aria-label={t("slds.datepicker.dialog").replace("{month}", monthName(month))}
      className={clsx("slds-datepicker slds-dropdown slds-dropdown_left static", className)}
    >
      <Ctx.Provider value={{ monthId, fromYear, toYear, range }}>
        <DayPicker
          mode="single"
          required
          selected={selected}
          onDayClick={(date, m) => !m.disabled && onPick(date)}
          month={month}
          onMonthChange={setMonth}
          startMonth={new Date(fromYear, 0, 1)}
          endMonth={new Date(toYear, 11, 1)}
          weekStartsOn={1}
          showOutsideDays
          hideNavigation
          autoFocus={focusGrid}
          disabled={disabled}
          components={COMPONENTS}
          formatters={FORMATTERS}
          labels={LABELS}
        />
      </Ctx.Provider>
      <button type="button" className="slds-button slds-align_absolute-center slds-text-link" disabled={todayOff} onClick={() => onPick(today)}>
        {t("slds.datepicker.today")}
      </button>
    </div>
  );
}

// ── the field ────────────────────────────────────────────────────────────────────────────────────
export interface DateFieldProps extends FieldProps {
  id?: string;
  value?: Date;
  onChange: (date: Date | null) => void;
  min?: Date;
  max?: Date;
  /** Range pickers: the whole span, highlighted in this field's calendar. */
  range?: Span;
  required?: boolean;
  disabled?: boolean;
  readOnly?: boolean;
  /** Default " " like the blueprint (the format is announced, not shown). */
  placeholder?: string;
  /** Show the format line under the field (blueprint "date format visible"). */
  formatVisible?: boolean;
  /** false = `required` on the input only: a compound fieldset's legend carries the mark. */
  requiredMark?: boolean;
  /** Error line drawn by the compound fieldset (blueprint datetime-picker error): the field turns
   *  invalid and its input is described by that line; the field draws no message of its own. */
  errorId?: string;
  className?: string;
}

/** One date input + calendar; the building block of DatePicker, DateRangePicker and DateTimePicker. */
export function DateField({
  id,
  value,
  onChange,
  min,
  max,
  range,
  required,
  disabled,
  readOnly,
  placeholder,
  formatVisible,
  requiredMark = true,
  errorId,
  className,
  label,
  labelHidden,
  error,
  help,
  fieldLevelHelp,
}: DateFieldProps) {
  const { controlId, labelId, helpId } = useFieldIds(id);
  const formatId = `${controlId}-format`;
  const iso = value ? toISODate(value) : null;
  const [open, setOpen] = useState(false);
  const [focusGrid, setFocusGrid] = useState(false);
  const [text, setText] = useState(value ? formatDisplayDate(value) : "");
  const [invalid, setInvalid] = useState<string | null>(null);
  const [shown, setShown] = useState(iso);
  // A new value from outside replaces whatever is in the box (render-time sync, no effect).
  if (iso !== shown) {
    setShown(iso);
    setText(value ? formatDisplayDate(value) : "");
    setInvalid(null);
  }
  const rootRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const returnFocus = useRef(true);
  const editable = !disabled && !readOnly;

  const accept = (d: Date | null) => {
    setInvalid(null);
    setText(d ? formatDisplayDate(d) : "");
    if (d ? !value || !isSameDay(d, value) : value) onChange(d);
  };
  const pick = (d: Date) => {
    returnFocus.current = true;
    setOpen(false);
    accept(d);
  };
  // Typed text: parsed on blur/Enter. Bad text keeps the value and says why, instead of vanishing.
  const commit = () => {
    if (!text.trim()) return accept(null);
    const d = parseDisplayDate(text);
    if (!d) return setInvalid(t("slds.datepicker.invalid"));
    if ((min && d < min) || (max && d > max)) return setInvalid(t("slds.datepicker.outOfRange"));
    accept(d);
  };
  const outside = (e: { target: EventTarget | null; preventDefault: () => void }) => {
    if (rootRef.current?.contains(e.target as Node)) e.preventDefault();
    else returnFocus.current = false; // the user went elsewhere: leave focus there
  };

  const message = error ?? invalid ?? help;
  const isOpen = open && editable;
  return (
    <PopoverPrimitive.Root
      open={isOpen}
      onOpenChange={(next) => {
        // Only the calendar button opens through Radix: that path moves focus into the grid. A close
        // keeps returnFocus as is: `outside` cleared it when the user clicked into another field.
        if (next) {
          setFocusGrid(true);
          returnFocus.current = true;
        }
        setOpen(next);
      }}
    >
      <div
        ref={rootRef}
        className={clsx(
          "slds-form-element slds-dropdown-trigger slds-dropdown-trigger_click",
          (error || invalid || errorId) && "slds-has-error",
          isOpen && "slds-is-open",
          className,
        )}
      >
        <label className={clsx("slds-form-element__label", labelHidden && "slds-assistive-text")} htmlFor={controlId} id={labelId}>
          {required && requiredMark && <RequiredMark />}
          {label}
        </label>
        {fieldLevelHelp && <div className="slds-form-element__icon">{fieldLevelHelp}</div>}
        <PopoverPrimitive.Anchor asChild>
          <div className="slds-form-element__control slds-input-has-icon slds-input-has-icon_right">
            <input
              ref={inputRef}
              id={controlId}
              type="text"
              autoComplete="off"
              className="slds-input"
              placeholder={placeholder ?? " "}
              value={text}
              required={required}
              disabled={disabled}
              readOnly={readOnly}
              aria-invalid={error || invalid || errorId ? true : undefined}
              aria-describedby={describedBy(formatId, message != null && message !== "" && helpId, errorId)}
              onChange={(e) => setText(e.target.value)}
              onBlur={commit}
              onKeyDown={(e) => e.key === "Enter" && commit()}
              onClick={() => {
                if (!editable || open) return;
                setFocusGrid(false);
                returnFocus.current = true;
                setOpen(true);
              }}
            />
            <PopoverPrimitive.Trigger asChild>
              <ButtonIcon icon={CalendarDays} label={t("slds.datepicker.selectDate")} className="slds-input__icon slds-input__icon_right" disabled={!editable} />
            </PopoverPrimitive.Trigger>
          </div>
        </PopoverPrimitive.Anchor>
        <div className={clsx("slds-form-element__help", !formatVisible && "slds-assistive-text")} id={formatId}>
          {t("slds.datepicker.format")}
        </div>
        {message != null && message !== "" && (
          <div className="slds-form-element__help" id={helpId}>
            {message}
          </div>
        )}
        <PopoverPrimitive.Content
          asChild
          side="bottom"
          align="start"
          sideOffset={0}
          onOpenAutoFocus={(e) => e.preventDefault()}
          onCloseAutoFocus={(e) => {
            e.preventDefault();
            if (returnFocus.current) inputRef.current?.focus();
          }}
          onPointerDownOutside={outside}
          onFocusOutside={outside}
        >
          <Calendar selected={value} onPick={pick} min={min} max={max} range={range} focusGrid={focusGrid} />
        </PopoverPrimitive.Content>
      </div>
    </PopoverPrimitive.Root>
  );
}

// ── public pickers ───────────────────────────────────────────────────────────────────────────────
export interface DatePickerProps extends Omit<DateFieldProps, "value" | "onChange" | "min" | "max" | "range"> {
  /** ISO "yyyy-MM-dd"; null = empty. */
  value?: string | null;
  defaultValue?: string | null;
  onChange?: (value: string | null) => void;
  /** Earliest / latest selectable day, ISO "yyyy-MM-dd". */
  min?: string;
  max?: string;
}

export function DatePicker({ value, defaultValue = null, onChange, min, max, ...field }: DatePickerProps) {
  const [inner, setInner] = useState<string | null>(defaultValue);
  const current = value !== undefined ? value : inner;
  return (
    <DateField
      {...field}
      value={parseISODate(current)}
      min={parseISODate(min)}
      max={parseISODate(max)}
      onChange={(d) => {
        const next = d ? toISODate(d) : null;
        setInner(next);
        onChange?.(next);
      }}
    />
  );
}

export interface DateRange {
  from: string | null;
  to: string | null;
}

export interface DateRangePickerProps {
  id?: string;
  /** Legend of the pair. */
  label: ReactNode;
  /** Labels of the two fields; default "Từ ngày" / "Đến ngày". */
  startLabel?: string;
  endLabel?: string;
  value?: DateRange;
  defaultValue?: DateRange;
  onChange?: (value: DateRange) => void;
  min?: string;
  max?: string;
  required?: boolean;
  disabled?: boolean;
  error?: ReactNode;
  className?: string;
}

/** Blueprint datepickers/range: two date fields in a compound row; each calendar shows the span. The
 *  end field cannot go before the start and vice versa. Two single-date calendars rather than
 *  react-day-picker's range mode, because the blueprint gives each end its own field and calendar. */
export function DateRangePicker({
  id,
  label,
  startLabel,
  endLabel,
  value,
  defaultValue = { from: null, to: null },
  onChange,
  min,
  max,
  required,
  disabled,
  error,
  className,
}: DateRangePickerProps) {
  const auto = useId();
  const base = id ?? auto;
  const [inner, setInner] = useState<DateRange>(defaultValue);
  const current = value ?? inner;
  const from = parseISODate(current.from);
  const to = parseISODate(current.to);
  const set = (next: DateRange) => {
    setInner(next);
    onChange?.(next);
  };
  const span = { from, to };
  const hasError = error != null && error !== "";
  const errorId = hasError ? `${base}-error` : undefined;
  return (
    <div className={clsx("slds-form slds-form_compound", className)}>
      <fieldset className={clsx("slds-form-element", hasError && "slds-has-error")}>
        <legend className="slds-form-element__label slds-form-element__legend">
          {required && <RequiredMark />}
          {label}
        </legend>
        <div className="slds-form-element__control">
          <div className="slds-form-element__group">
            <div className="slds-form-element__row">
              <DateField
                id={`${base}-from`}
                label={startLabel ?? t("slds.datepicker.start")}
                value={from}
                min={parseISODate(min)}
                max={to ?? parseISODate(max)}
                range={span}
                required={required}
                requiredMark={false}
                errorId={errorId}
                disabled={disabled}
                onChange={(d) => set({ ...current, from: d ? toISODate(d) : null })}
              />
              <DateField
                id={`${base}-to`}
                label={endLabel ?? t("slds.datepicker.end")}
                value={to}
                min={from ?? parseISODate(min)}
                max={parseISODate(max)}
                range={span}
                required={required}
                requiredMark={false}
                errorId={errorId}
                disabled={disabled}
                onChange={(d) => set({ ...current, to: d ? toISODate(d) : null })}
              />
            </div>
          </div>
        </div>
        {hasError && (
          <div className="slds-form-element__help" id={errorId}>
            {error}
          </div>
        )}
      </fieldset>
    </div>
  );
}
