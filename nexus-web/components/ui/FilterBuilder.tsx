"use client";

// FilterBuilder — Nexus Kit primitive (W9, playbook §5.3). ONE filter UI shared by every consumer
// that needs "narrow a set by field conditions": the record list (LayoutList) today; report /
// dashboard widget config, workflow-condition editor, and approval entry-criteria in later waves.
//
// Headless-ish & controlled: the caller owns the FilterRow[] state (and any debounce), passes the
// filterable `fields` + current `value`, and receives the full next array on every edit. The
// primitive renders the row editor (panel) and, separately, a compact removable-chips summary.
//
// All filter *logic* lives in lib/filterModel.ts (pure, unit-tested): the 11-operator per-field-type
// map, the ?field[op]=value serializer, the drill-through parser, and the chip model. This file is
// pure presentation on top of that contract — so the data behaviour is identical everywhere.
//
// Value inputs are TYPED per field type, but every widget emits the SAME normalised string the old
// plain <input> produced, so the serialised query is byte-for-byte unchanged:
//   • text/rich-ish → text input          • number/currency/rating → number input(s)
//   • date/datetime → native date input; two date inputs for `between`
//   • picklist/multi_select → Combobox (single) / checkbox popover (`in`, comma-joined)
//   • boolean → Có/Không toggle (emits "true"/"false")
//
// NOTE (date between): we use two native date inputs rather than the analytics DateRangePicker —
// that component bakes in "Lọc theo ngày tạo bản ghi (created_at)" copy + created_at presets, which
// would be factually wrong inside an arbitrary date-field filter row. Two date bounds are honest and
// consistent with the numeric `between` visual.
//
// AND semantics only (rows combine with AND). OR is a documented future extension — see
// lib/filterModel.ts FilterRow doc.
//
// Design: teal+beige, token-only (VARIANCE 4 · MOTION 2 · DENSITY 7). Motion comes from the kit
// Popover (respects prefers-reduced-motion via globals.css); no bespoke animation here.

import { useState } from "react";
import { clsx } from "clsx";
import { Filter, Plus, X, Check, ChevronsUpDown } from "lucide-react";
import { Combobox, type ComboOption } from "./Combobox";
import { Popover, PopoverContent, PopoverTrigger } from "./Popover";
import {
  type FilterRow,
  OP_LABELS,
  opsForType,
  filterTypeOf,
  DEFAULT_OPS,
  isNullOp,
  isFilterableType,
  newFilterRow,
  filterChips,
} from "@/lib/filterModel";

export type { FilterRow } from "@/lib/filterModel";

/** Minimal field descriptor the builder needs (a superset-compatible subset of FieldMeta). */
export interface FilterField {
  name: string;
  label?: string;
  fieldType: string;
  options?: Record<string, unknown>;
}

function picklistValues(field: FilterField | undefined): string[] {
  const v = field?.options?.values;
  return Array.isArray(v) ? (v as unknown[]).map((x) => String(x)) : [];
}

// Display label per picklist CODE (options.valueLabels, "New" → "Mới"). Filters COMPARE and EMIT
// codes — only what the human reads changes; no label → the code itself.
function picklistValueLabel(field: FilterField | undefined, code: string): string {
  const m = field?.options?.valueLabels as Record<string, string> | undefined;
  return m?.[code] ?? code;
}

const labelOf = (f: FilterField) => f.label ?? f.name;

// Wave 3.6 — relative-date presets. MUST mirror the backend token set
// (nexus-platform/internal/services/relative_dates.go relativeDateTokens): the value sent on the
// wire IS the token; the server resolves it against the app timezone at query time, so a saved
// view with "Hôm nay" stays correct forever.
const RELATIVE_DATE_LABELS: Record<string, string> = {
  today: "Hôm nay",
  yesterday: "Hôm qua",
  tomorrow: "Ngày mai",
  this_week: "Tuần này",
  last_week: "Tuần trước",
  next_week: "Tuần sau",
  this_month: "Tháng này",
  last_month: "Tháng trước",
  this_quarter: "Quý này",
  this_year: "Năm nay",
  last_7_days: "7 ngày qua",
  last_30_days: "30 ngày qua",
  last_90_days: "90 ngày qua",
  next_7_days: "7 ngày tới",
  next_30_days: "30 ngày tới",
};
// Ops the backend accepts a token for (eq/gt/gte/lt/lte; "" = eq default). between/in/neq → 422.
const RELATIVE_OK_OPS = new Set(["", "eq", "gt", "gte", "lt", "lte"]);

// ── FilterBuilder (panel / row editor) ─────────────────────────────────────────────
export interface FilterBuilderProps {
  /** Candidate fields; non-filterable types (composite/rich_text/file/image/formula) are dropped. */
  fields: FilterField[];
  /** Current filter rows (controlled). */
  value: FilterRow[];
  /** Called with the full next array on any add / remove / edit / clear. */
  onChange: (rows: FilterRow[]) => void;
  className?: string;
  /** Show the "Bộ lọc" header + "Xoá tất cả" affordance (default true). */
  showHeader?: boolean;
  /** Per-field-type operator list override (default: opsForType — the record-list 11-op map).
      The approval-criteria consumer passes approvalOpsForType (workflow 9-op vocabulary,
      no in/between) so it never offers an operator its backend rejects. */
  opsFor?: (fieldType: string) => readonly string[];
}

export function FilterBuilder({ fields, value, onChange, className, showHeader = true, opsFor = opsForType }: FilterBuilderProps) {
  const visibleFields = fields.filter((f) => isFilterableType(f.fieldType));
  const byName = new Map(visibleFields.map((f) => [f.name, f]));
  const [addField, setAddField] = useState(visibleFields[0]?.name ?? "");

  const fieldOptions: ComboOption[] = visibleFields.map((f) => ({
    value: f.name,
    label: labelOf(f),
    hint: f.name,
  }));

  const patchRow = (id: string, patch: Partial<FilterRow>) =>
    onChange(value.map((f) => (f.id === id ? { ...f, ...patch } : f)));
  const removeRow = (id: string) => onChange(value.filter((f) => f.id !== id));
  const addRow = (field: string) => onChange([...value, newFilterRow(field, "eq")]);
  const clearAll = () => onChange([]);

  return (
    <div className={clsx("rounded-card border border-bd bg-panel p-4 shadow-card", className)}>
      {showHeader && (
        <div className="mb-3 flex items-center gap-2">
          <Filter className="size-3.5 text-fnt" strokeWidth={1.8} />
          <span className="text-12-5px font-medium text-fg">Bộ lọc</span>
          {value.length > 0 && (
            <button
              type="button"
              onClick={clearAll}
              className="ml-auto text-11-5px text-mut hover:text-dgf"
            >
              Xoá tất cả
            </button>
          )}
        </div>
      )}

      <div className="space-y-2">
        {value.map((f) => {
          const meta = byName.get(f.field);
          const ops = meta ? opsFor(filterTypeOf(meta)) : DEFAULT_OPS;
          return (
            <div key={f.id} className="flex items-center gap-2">
              {/* Field picker — Combobox (searchable; field lists can be long) */}
              <div className="w-44 shrink-0">
                <Combobox
                  value={f.field}
                  onChange={(field) => patchRow(f.id, { field, op: "eq", value: "" })}
                  options={fieldOptions}
                  placeholder="Chọn trường…"
                  searchPlaceholder="Tìm trường…"
                />
              </div>
              {/* Operator select */}
              <select
                value={f.op}
                onChange={(e) => patchRow(f.id, { op: e.target.value, value: "" })}
                aria-label="Toán tử"
                className="h-8 shrink-0 rounded-input border border-bd bg-panel px-2 text-12-5px text-fg focus-visible:border-ac focus-visible:outline-none"
              >
                {ops.map((op) => (
                  <option key={op} value={op}>{OP_LABELS[op] ?? op}</option>
                ))}
              </select>
              {/* Value — typed per field type + operator */}
              {!isNullOp(f.op) && (
                <FilterValueInput
                  field={meta}
                  op={f.op}
                  value={f.value}
                  onChange={(v) => patchRow(f.id, { value: v })}
                />
              )}
              <button
                type="button"
                onClick={() => removeRow(f.id)}
                aria-label="Xoá điều kiện"
                className="ml-auto grid size-8 shrink-0 place-items-center rounded-input text-fnt hover:bg-dgd hover:text-dgf"
              >
                <X className="size-3.5" strokeWidth={2} />
              </button>
            </div>
          );
        })}
      </div>

      {/* Add filter row */}
      <div className="mt-3 flex items-center gap-2 border-t border-bd2 pt-3">
        <div className="w-44 shrink-0">
          <Combobox
            value={addField}
            onChange={setAddField}
            options={fieldOptions}
            placeholder="Chọn trường…"
            searchPlaceholder="Tìm trường…"
          />
        </div>
        <button
          type="button"
          onClick={() => { if (addField) addRow(addField); }}
          disabled={!addField}
          className="flex h-8 items-center gap-1.5 rounded-input border border-bd bg-panel2 px-3 text-12-5px font-medium text-fg hover:bg-panel disabled:cursor-not-allowed disabled:opacity-50"
        >
          <Plus className="size-3.5" strokeWidth={2} /> Thêm điều kiện
        </button>
      </div>
    </div>
  );
}

// ── FilterChips (compact removable summary) ─────────────────────────────────────────
export interface FilterChipsProps {
  fields: FilterField[];
  value: FilterRow[];
  /** Called with the next array when a chip's × is clicked (that row removed). */
  onChange: (rows: FilterRow[]) => void;
  className?: string;
}

export function FilterChips({ fields, value, onChange, className }: FilterChipsProps) {
  const labelFor = (name: string) => fields.find((f) => f.name === name)?.label ?? name;
  // Value labels too: this is the surface a deep-link enters through (the home card's
  // ?status[eq]=New never touches the option pickers above), so without it the chip still read
  // "Trạng thái = New" after every picker was translated.
  const valueLabelFor = (name: string, v: string) => picklistValueLabel(fields.find((f) => f.name === name), v);
  const chips = filterChips(value, labelFor, valueLabelFor);
  if (chips.length === 0) return null;
  return (
    <div className={clsx("flex flex-wrap items-center gap-1.5", className)}>
      {chips.map((c) => (
        <span
          key={c.id}
          className="flex items-center gap-1 rounded-pill border border-ac/25 bg-acd px-2 py-0.5 text-11-5px font-medium text-ac-hi"
        >
          {c.label}
          <button
            type="button"
            onClick={() => onChange(value.filter((f) => f.id !== c.id))}
            aria-label={`Bỏ lọc ${c.label}`}
            className="grid size-3.5 place-items-center rounded-full text-ac-hi/70 hover:bg-ac/15 hover:text-ac-hi"
          >
            <X className="size-3" strokeWidth={2.5} />
          </button>
        </span>
      ))}
    </div>
  );
}

// ── Value input (typed per field type + operator) ──────────────────────────────────
// Emits a single normalised string into FilterRow.value; multi-value operators (in/between) join
// members with "," to match the backend's strings.Split(rawVal, ",") contract.
function FilterValueInput({
  field, op, value, onChange,
}: {
  field: FilterField | undefined;
  op: string;
  value: string;
  onChange: (v: string) => void;
}) {
  const ft = field?.fieldType ?? "text";
  const numeric = ft === "number" || ft === "currency" || ft === "rating";
  const isDate = ft === "date" || ft === "datetime";
  const picklist = ft === "picklist" || ft === "multi_select";
  const inputBase =
    "h-8 min-w-0 rounded-input border border-bd bg-panel px-2.5 text-12-5px text-fg placeholder:text-fnt focus-visible:border-ac focus-visible:outline-none";

  // between → two bounds joined "lo,hi" (date bounds are native date inputs)
  if (op === "between") {
    const [lo = "", hi = ""] = value.split(",");
    const emit = (a: string, b: string) => onChange(`${a},${b}`);
    const kind = isDate ? "date" : numeric ? "number" : "text";
    return (
      <div className="flex flex-1 items-center gap-1.5">
        <input type={kind} value={lo} onChange={(e) => emit(e.target.value, hi)}
          placeholder="Từ…" aria-label="Từ" className={clsx(inputBase, "flex-1")} />
        <span className="text-sub text-fnt">–</span>
        <input type={kind} value={hi} onChange={(e) => emit(lo, e.target.value)}
          placeholder="Đến…" aria-label="Đến" className={clsx(inputBase, "flex-1")} />
      </div>
    );
  }

  // in → multi-select of picklist members (comma-joined), or free comma text as fallback
  if (op === "in") {
    const opts = picklistValues(field);
    if (opts.length > 0) {
      return <MultiSelectValue options={opts} labelFor={(v) => picklistValueLabel(field, v)} value={value} onChange={onChange} />;
    }
    return (
      <input value={value} onChange={(e) => onChange(e.target.value)}
        placeholder="Giá trị, phân tách bằng dấu phẩy…" aria-label="Giá trị"
        className={clsx(inputBase, "flex-1")} />
    );
  }

  // single-value operators
  if (picklist) {
    const options: ComboOption[] = picklistValues(field).map((v) => ({ value: v, label: picklistValueLabel(field, v) }));
    return (
      <div className="flex-1">
        <Combobox value={value} onChange={onChange} options={options}
          placeholder="Chọn giá trị…" searchPlaceholder="Tìm…" />
      </div>
    );
  }
  if (ft === "boolean") {
    return <BooleanToggle value={value} onChange={onChange} />;
  }
  if (isDate) {
    // Wave 3.6: relative-date presets, server-resolved (BE resolveDateFilters — same token set).
    // Only the ops the BE accepts a token for get the preset select; between keeps two bounds.
    if (RELATIVE_OK_OPS.has(op)) {
      const isToken = value in RELATIVE_DATE_LABELS;
      return (
        <div className="flex flex-1 items-center gap-1.5">
          <select
            value={isToken ? value : ""}
            onChange={(e) => onChange(e.target.value)} // "" = custom → clears to empty date input
            aria-label="Chọn mốc thời gian"
            className={clsx(inputBase, isToken ? "flex-1" : "w-[132px] shrink-0")}
          >
            <option value="">Ngày cụ thể…</option>
            {Object.entries(RELATIVE_DATE_LABELS).map(([tok, label]) => (
              <option key={tok} value={tok}>{label}</option>
            ))}
          </select>
          {!isToken && (
            <input type="date" value={value} onChange={(e) => onChange(e.target.value)}
              aria-label="Giá trị" className={clsx(inputBase, "flex-1")} />
          )}
        </div>
      );
    }
    return (
      <input type="date" value={value} onChange={(e) => onChange(e.target.value)}
        aria-label="Giá trị" className={clsx(inputBase, "flex-1")} />
    );
  }
  return (
    <input type={numeric ? "number" : "text"} value={value} onChange={(e) => onChange(e.target.value)}
      placeholder="Giá trị…" aria-label="Giá trị" className={clsx(inputBase, "flex-1")} />
  );
}

// Có / Không segmented toggle → emits "true" / "false".
function BooleanToggle({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <div className="flex flex-1 items-center rounded-input border border-bd bg-panel p-0.5">
      {([["true", "Có"], ["false", "Không"]] as [string, string][]).map(([v, lbl]) => (
        <button
          key={v}
          type="button"
          onClick={() => onChange(v)}
          aria-pressed={value === v}
          className={clsx(
            "h-7 flex-1 rounded-[5px] text-sub font-medium transition-colors",
            value === v ? "bg-acd text-ac-hi" : "text-fnt hover:text-fg",
          )}
        >
          {lbl}
        </button>
      ))}
    </div>
  );
}

// Multi-select over picklist members → comma-joined value string.
function MultiSelectValue({
  options, labelFor, value, onChange,
}: {
  options: string[];
  /** Display text per option CODE — the emitted comma-joined value stays codes. */
  labelFor?: (v: string) => string;
  value: string;
  onChange: (v: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const show = (v: string) => labelFor?.(v) ?? v;
  const selected = value.split(",").map((s) => s.trim()).filter(Boolean);
  const toggle = (opt: string) => {
    const next = selected.includes(opt) ? selected.filter((s) => s !== opt) : [...selected, opt];
    onChange(next.join(","));
  };
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <button
          type="button"
          aria-expanded={open}
          className="flex h-8 min-w-0 flex-1 items-center gap-2 rounded-input border border-bd bg-panel px-2.5 text-left text-12-5px text-fg hover:border-ac focus-visible:border-ac focus-visible:outline-none"
        >
          <span className={clsx("min-w-0 flex-1 truncate", selected.length === 0 && "text-fnt")}>
            {selected.length === 0 ? "Chọn giá trị…" : selected.map(show).join(", ")}
          </span>
          <ChevronsUpDown className="size-3.5 shrink-0 text-fnt" strokeWidth={1.8} />
        </button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-[var(--radix-popover-trigger-width)] min-w-48 p-1.5">
        <div className="max-h-56 overflow-y-auto">
          {options.map((opt) => {
            const on = selected.includes(opt);
            return (
              <button
                key={opt}
                type="button"
                onClick={() => toggle(opt)}
                className="flex w-full cursor-pointer items-center gap-2 rounded-[6px] px-2.5 py-1.5 text-left text-12-5px text-fg hover:bg-panel2"
              >
                <Check className={clsx("size-3.5 shrink-0", on ? "text-ac" : "opacity-0")} strokeWidth={2.5} />
                <span className="min-w-0 flex-1 truncate">{show(opt)}</span>
              </button>
            );
          })}
        </div>
      </PopoverContent>
    </Popover>
  );
}
