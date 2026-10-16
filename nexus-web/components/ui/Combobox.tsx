"use client";

// Combobox — Nexus Kit primitive (W5, plan §3.2). Searchable single-select built on the kit's
// Popover + cmdk (both already deps; CommandPalette uses the same Command engine). Replaces the
// native <select> in Studio metadata pickers where lists show LABELS (entity labels, field
// labels) and can grow long — type-to-filter beats scanning a 40-option dropdown.
//
// Deliberately narrow API: flat option list ({ value, label, hint }), controlled value, no
// multi-select / no async loading here (callers fetch, this renders). Token-only styling —
// trigger matches the builder's 32px input row; popover matches PopoverContent.

import { useState } from "react";
import { Command } from "cmdk";
import { Check, ChevronsUpDown } from "lucide-react";
import { clsx } from "clsx";
import { Popover, PopoverContent, PopoverTrigger } from "./Popover";

export interface ComboOption {
  value: string;
  label: string;
  /** Muted right-side hint (mono) — e.g. the machine name next to a label. */
  hint?: string;
}

export function Combobox({
  value,
  onChange,
  options,
  placeholder = "Chọn…",
  searchPlaceholder = "Tìm…",
  emptyText = "Không có kết quả.",
  disabled = false,
  className,
}: {
  value: string;
  onChange: (v: string) => void;
  options: ComboOption[];
  placeholder?: string;
  searchPlaceholder?: string;
  emptyText?: string;
  disabled?: boolean;
  className?: string;
}) {
  const [open, setOpen] = useState(false);
  const selected = options.find((o) => o.value === value);

  return (
    <Popover open={open} onOpenChange={disabled ? () => {} : setOpen}>
      <PopoverTrigger asChild>
        <button
          type="button"
          disabled={disabled}
          aria-expanded={open}
          className={clsx(
            "flex h-8 w-full items-center gap-2 rounded-[6px] border border-bd bg-panel px-2 text-left text-sub transition-colors",
            "hover:border-ac focus-visible:border-ac focus-visible:outline-none",
            "disabled:cursor-not-allowed disabled:opacity-50",
            className,
          )}
        >
          <span className={clsx("min-w-0 flex-1 truncate", selected ? "text-fg" : "text-fnt")}>
            {selected ? selected.label : placeholder}
          </span>
          {selected?.hint && <span className="shrink-0 font-mono text-10px text-fnt">{selected.hint}</span>}
          <ChevronsUpDown className="size-3.5 shrink-0 text-fnt" strokeWidth={1.8} />
        </button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-[var(--radix-popover-trigger-width)] min-w-56 p-1.5">
        <Command
          // cmdk filters on the item's `value` string — we bake label + hint in so the search
          // matches both the human label ("Khách hàng") and the machine name ("crm_accounts").
          label={placeholder}
        >
          <Command.Input
            placeholder={searchPlaceholder}
            className="mb-1.5 h-8 w-full rounded-[6px] border border-bd bg-bg px-2.5 text-sub text-fg placeholder:text-fnt focus:border-ac focus:outline-none"
          />
          <Command.List className="max-h-56 overflow-y-auto">
            <Command.Empty className="px-2.5 py-3 text-center text-sub text-fnt">{emptyText}</Command.Empty>
            {options.map((o) => (
              <Command.Item
                key={o.value}
                value={`${o.label} ${o.hint ?? ""} ${o.value}`}
                onSelect={() => {
                  onChange(o.value);
                  setOpen(false);
                }}
                className="flex cursor-pointer items-center gap-2 rounded-[6px] px-2.5 py-1.5 text-12-5px text-fg data-[selected=true]:bg-acd data-[selected=true]:text-ac-hi"
              >
                <Check className={clsx("size-3.5 shrink-0", o.value === value ? "text-ac" : "opacity-0")} strokeWidth={2.5} />
                <span className="min-w-0 flex-1 truncate">{o.label}</span>
                {o.hint && <span className="shrink-0 font-mono text-10px text-fnt">{o.hint}</span>}
              </Command.Item>
            ))}
          </Command.List>
        </Command>
      </PopoverContent>
    </Popover>
  );
}
