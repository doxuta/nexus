"use client";

// SLDS time picker (blueprint `timepicker`, SLDS 2.264.1): the autocomplete combobox markup with
// `slds-timepicker` on div.slds-combobox and a clock icon. 24-hour "HH:mm" values, a list every
// `step` minutes (default 15). Typing narrows the list; free text is parsed on Enter/blur ("9h30",
// "930", "9:30" → "09:30"), so any minute can be entered, not just the listed ones. Text that is not a
// time keeps the previous value and shows an error rather than silently dropping what was typed.
import { Clock } from "lucide-react";
import { useMemo, useState } from "react";
import { t } from "@/lib/i18n";
import { ComboboxField, matches, type ComboboxFieldProps, type ComboboxOption } from "./Combobox";

export interface TimePickerProps
  extends Omit<
    ComboboxFieldProps,
    "mode" | "options" | "selected" | "multiple" | "onSelect" | "onDeselect" | "inputValue" | "onInputChange" | "lockSelection" | "onCommit" | "icon" | "comboboxClassName" | "noResultsText"
  > {
  /** "HH:mm" (24h); null = empty. */
  value?: string | null;
  defaultValue?: string | null;
  onChange?: (value: string | null) => void;
  /** Minutes between listed times (default 15). */
  step?: number;
}

const pad = (n: number) => String(n).padStart(2, "0");

/** "HH:mm" for "9", "09:30", "9h30", "9g", "930", "2359"; null for empty text; undefined if not a time. */
export function parseTime(text: string): string | null | undefined {
  const s = text.trim().toLowerCase();
  if (!s) return null;
  const m = /^(\d{1,2})(?:\s*[:hg.]\s*(\d{2})?)?$/.exec(s) ?? /^(\d{1,2})(\d{2})$/.exec(s);
  if (!m) return undefined;
  const h = Number(m[1]);
  const min = Number(m[2] ?? 0);
  return h > 23 || min > 59 ? undefined : `${pad(h)}:${pad(min)}`;
}

export function TimePicker({ value, defaultValue = null, onChange, step = 15, placeholder, error, ...field }: TimePickerProps) {
  const [inner, setInner] = useState<string | null>(defaultValue);
  const current = value !== undefined ? value : inner;
  const [text, setText] = useState(current ?? "");
  const [shown, setShown] = useState(current);
  const [invalid, setInvalid] = useState(false);
  // A new value from outside replaces whatever is in the box (render-time sync, no effect).
  if (current !== shown) {
    setShown(current);
    setText(current ?? "");
  }

  const options = useMemo<ComboboxOption[]>(() => {
    const list: ComboboxOption[] = [];
    for (let m = 0; m < 24 * 60; m += Math.max(1, step)) {
      const label = `${pad(Math.floor(m / 60))}:${pad(m % 60)}`;
      list.push({ value: label, label });
    }
    return list;
  }, [step]);

  const set = (next: string | null) => {
    setInvalid(false);
    setText(next ?? "");
    if (next === current) return;
    setInner(next);
    onChange?.(next);
  };
  const commit = (raw: string) => {
    const parsed = parseTime(raw);
    if (parsed === undefined) setInvalid(true);
    else set(parsed);
  };

  // Filter only while the text differs from the value, so reopening shows the whole day.
  const filtering = text.trim() !== "" && text !== (current ?? "");
  const chosen = options.find((o) => o.value === current);
  return (
    <ComboboxField
      {...field}
      mode="autocomplete"
      options={filtering ? options.filter((o) => matches(o.label, text)) : options}
      selected={chosen ? [chosen] : []}
      lockSelection={false}
      onSelect={(o) => set(o.value)}
      onDeselect={() => set(null)}
      inputValue={text}
      onInputChange={(next) => {
        setText(next);
        setInvalid(false);
      }}
      onCommit={commit}
      noResultsText={false}
      placeholder={placeholder ?? t("slds.timepicker.placeholder")}
      icon={{ glyph: Clock, name: "clock" }}
      comboboxClassName="slds-timepicker"
      error={error ?? (invalid ? t("slds.timepicker.invalid") : undefined)}
    />
  );
}
