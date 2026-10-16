"use client";

// DateRangePicker — the run-overlay date filter (dashboards/[id], reports/[id], admin audit).
// A Lightning dashboard filter: a neutral button naming the filter and its value ("Khoảng ngày:
// 7 ngày qua") opens an SLDS popover (blueprint `popovers`) holding a preset select (blueprint
// `select`) and the SLDS range datepicker (blueprint `datepickers/range`, components/slds/DatePicker)
// for a custom span. A preset or a date applies at once; the popover stays open until dismissed
// (Esc, outside click, close button) — a keyboard user arrowing through the select must not have
// it close under them. Values are the run filters' ISO "yyyy-MM-dd" pair.

import { useState } from "react";
import { CalendarRange, ChevronDown } from "lucide-react";
import { t, tf } from "@/lib/i18n";
import { DATE_PRESETS, formatDateVN, matchPreset, presetRange, type DatePresetId, type RunFilters } from "@/lib/runFilters";
import { Button } from "@/components/slds/Button";
import { DateRangePicker as RangeFields } from "@/components/slds/DatePicker";
import { Popover } from "@/components/slds/Popover";
import { Select } from "@/components/slds/Select";

/** "7 ngày qua" / "01/09/2026 – 29/09/2026" / "Từ 01/09/2026" — undefined when no range is set. */
function rangeLabel(value: RunFilters, today: Date): string | undefined {
  const preset = matchPreset(value, today);
  if (preset) return DATE_PRESETS.find((p) => p.id === preset)?.label;
  if (value.dateFrom && value.dateTo) {
    return value.dateFrom === value.dateTo
      ? formatDateVN(value.dateFrom)
      : `${formatDateVN(value.dateFrom)} – ${formatDateVN(value.dateTo)}`;
  }
  if (value.dateFrom) return tf("page.runFilter.from", { date: formatDateVN(value.dateFrom) });
  if (value.dateTo) return tf("page.runFilter.to", { date: formatDateVN(value.dateTo) });
  return undefined;
}

export function DateRangePicker({
  value,
  onChange,
  help,
}: {
  /** Current filters — only dateFrom/dateTo are read here. */
  value: RunFilters;
  /** Called with the new date pair (ownerId untouched by this control). */
  onChange: (next: { dateFrom?: string; dateTo?: string }) => void;
  /** What the range filters on; default: record creation (the run overlay's created_at). */
  help?: string;
}) {
  const [open, setOpen] = useState(false);
  const today = new Date();
  const active = matchPreset(value, today);
  const hasRange = Boolean(value.dateFrom || value.dateTo);
  const current = rangeLabel(value, today);

  return (
    <Popover
      open={open}
      onOpenChange={setOpen}
      title={t("page.runFilter.dateRange")}
      side="bottom"
      align="start"
      size="large"
      trigger={
        <Button variant={hasRange ? "outline-brand" : "neutral"} iconLeft={CalendarRange} iconRight={ChevronDown}>
          {current ? tf("page.runFilter.dateRangeValue", { value: current }) : t("page.runFilter.dateRange")}
        </Button>
      }
      footer={
        <div className="slds-grid slds-grid_vertical-align-center">
          <Button
            className="slds-col_bump-left"
            disabled={!hasRange}
            onClick={() => {
              onChange({ dateFrom: undefined, dateTo: undefined });
              setOpen(false);
            }}
          >
            {t("page.runFilter.clearRange")}
          </Button>
        </div>
      }
    >
      <div className="flex flex-col gap-3">
        <Select
          label={t("page.runFilter.presets")}
          placeholder={t("page.runFilter.custom")}
          value={active ?? ""}
          options={DATE_PRESETS.map((p) => ({ value: p.id, label: p.label }))}
          onChange={(e) => {
            const id = e.target.value as DatePresetId | "";
            if (id) onChange(presetRange(id, today));
          }}
        />
        <RangeFields
          label={t("page.runFilter.customRange")}
          value={{ from: value.dateFrom ?? null, to: value.dateTo ?? null }}
          onChange={(r) => onChange({ dateFrom: r.from ?? undefined, dateTo: r.to ?? undefined })}
        />
        <p className="slds-text-body_small slds-text-color_weak">{help ?? t("page.runFilter.dateHelp")}</p>
      </div>
    </Popover>
  );
}
