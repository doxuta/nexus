"use client";

// SLDS datetime picker (blueprint `datetime-picker`, SLDS 2.264.1):
//   div.slds-form > fieldset.slds-form-element.slds-form-element_compound > legend
//     + div.slds-form-element__control > div.slds-form-element__group > div.slds-form-element__row >
//         [date field + datepicker] [time field (timepicker combobox)]
//     + div.slds-form-element__help (error)
// The value is one ISO datetime with the browser's offset ("2026-09-24T14:30:00+07:00"), shown in
// local time. It is reported only when both parts are set; a half-filled pair reports null and, once
// focus leaves the pair, says so — a date on screen must never be silently saved as "no value".
import clsx from "clsx";
import { formatISO, isValid } from "date-fns";
import { useId, useState, type FocusEvent, type ReactNode } from "react";
import { t } from "@/lib/i18n";
import { DateField } from "./DatePicker";
import { RequiredMark } from "./FormElement";
import { TimePicker } from "./TimePicker";

export interface DateTimePickerProps {
  id?: string;
  /** Legend of the pair. */
  label: ReactNode;
  /** Labels of the two fields; default "Ngày" / "Giờ". */
  dateLabel?: string;
  timeLabel?: string;
  /** ISO 8601 datetime with offset; null = empty. */
  value?: string | null;
  defaultValue?: string | null;
  onChange?: (value: string | null) => void;
  /** Minutes between listed times (default 15). */
  step?: number;
  required?: boolean;
  disabled?: boolean;
  readOnly?: boolean;
  error?: ReactNode;
  className?: string;
}

interface Parts {
  date?: Date;
  time: string | null;
}

const pad = (n: number) => String(n).padStart(2, "0");
function split(iso: string | null): Parts {
  const d = iso ? new Date(iso) : undefined; // an offset makes the instant exact; parts are local
  if (!d || !isValid(d)) return { time: null };
  return { date: new Date(d.getFullYear(), d.getMonth(), d.getDate()), time: `${pad(d.getHours())}:${pad(d.getMinutes())}` };
}
function join({ date, time }: Parts) {
  if (!date || !time) return null;
  const [h, m] = time.split(":").map(Number);
  return formatISO(new Date(date.getFullYear(), date.getMonth(), date.getDate(), h, m));
}

export function DateTimePicker({
  id,
  label,
  dateLabel,
  timeLabel,
  value,
  defaultValue = null,
  onChange,
  step,
  required,
  disabled,
  readOnly,
  error,
  className,
}: DateTimePickerProps) {
  const auto = useId();
  const base = id ?? auto;
  const [inner, setInner] = useState<string | null>(defaultValue);
  const current = value !== undefined ? value : inner;
  const [parts, setParts] = useState<Parts>(() => split(current));
  const [shown, setShown] = useState(current);
  const [emitted, setEmitted] = useState<string | null | undefined>(undefined);
  const [touched, setTouched] = useState(false);
  // A value set from outside (not the null this picker reported for a half-filled pair) resets both parts.
  if (current !== shown) {
    setShown(current);
    if (current !== emitted) setParts(split(current));
  }

  const update = (next: Parts) => {
    setParts(next);
    const iso = join(next);
    if (iso === current) return;
    setInner(iso);
    setEmitted(iso);
    onChange?.(iso);
  };

  const incomplete = Boolean(parts.date) !== Boolean(parts.time);
  const message = error ?? (touched && incomplete ? t("slds.datetimepicker.incomplete") : undefined);
  const hasMessage = message != null && message !== "";
  // Blueprint error / required states: the legend has the only mark and the line sits under the
  // fieldset; each input carries required + aria-invalid + aria-describedby to that line (the blueprint
  // wires the date input; the time input too, since a half-filled pair is as often a missing time).
  const errorId = hasMessage ? `${base}-help` : undefined;
  return (
    <div className={clsx("slds-form", className)}>
      <fieldset
        className={clsx("slds-form-element slds-form-element_compound", hasMessage && "slds-has-error")}
        onBlur={(e: FocusEvent<HTMLFieldSetElement>) => {
          if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setTouched(true);
        }}
      >
        <legend className="slds-form-element__label slds-form-element__legend">
          {required && <RequiredMark />}
          {label}
        </legend>
        <div className="slds-form-element__control">
          <div className="slds-form-element__group">
            <div className="slds-form-element__row">
              <DateField
                id={`${base}-date`}
                label={dateLabel ?? t("slds.datetimepicker.date")}
                placeholder={t("slds.datepicker.placeholder")}
                value={parts.date}
                required={required}
                requiredMark={false}
                errorId={errorId}
                disabled={disabled}
                readOnly={readOnly}
                onChange={(d) => update({ ...parts, date: d ?? undefined })}
              />
              <TimePicker
                id={`${base}-time`}
                label={timeLabel ?? t("slds.datetimepicker.time")}
                value={parts.time}
                step={step}
                required={required}
                requiredMark={false}
                errorId={errorId}
                disabled={disabled}
                readOnly={readOnly}
                onChange={(time) => update({ ...parts, time })}
              />
            </div>
          </div>
        </div>
        {hasMessage && (
          <div className="slds-form-element__help" id={errorId}>
            {message}
          </div>
        )}
      </fieldset>
    </div>
  );
}
