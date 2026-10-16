"use client";

// The record list's filters on SLDS 2.264.1 (specs/038 T018/T022) — glue between LayoutList and the
// kit, no markup of its own:
//   • ListFilterDock: the kit FilterPanel (blueprint panels/filtering) docked right of the list, in
//     ObjectHomePage's list row, hidden (slds-hide) until the header's filter toggle opens it. It stays
//     mounted so the toggle's aria-controls always names it. Focus follows the kit Panel rule (panels
//     docs): opening from the toggle moves focus to the first filter ("Thêm bộ lọc" when there is
//     none), closing returns it to the toggle; a panel already open on load does not take focus.
//   • ListFilterPills: one removable pill per ACTIVE condition above the list
//     (pills/listbox-of-pill-options), worded with the panel's own conditionText/valueLabel, so a deep
//     link's picklist codes and relative-date tokens read as labels.
// Data contract is lib/filterModel, untouched: controlled FilterRow[], AND semantics, the per-type
// operator map, ?field[op]=value on the wire.

import clsx from "clsx";
import { useEffect, useRef } from "react";
import { t } from "@/lib/i18n";
import { activeFilters, type FilterField, type FilterRow } from "@/lib/filterModel";
import { conditionText, fieldLabel } from "@/components/slds/ExpressionBuilder";
import { FilterPanel } from "@/components/slds/FilterPanel";
import { Pill, PillContainer } from "@/components/slds/Pill";

export interface ListFiltersProps {
  fields: FilterField[];
  value: FilterRow[];
  onChange: (rows: FilterRow[]) => void;
}

// The header's filter toggle (ObjectHomePage: aria-controls = the dock id). Matched by attribute
// value rather than a selector: useId ids carry characters a selector would have to escape.
const filterToggle = (panelId: string) =>
  Array.from(document.querySelectorAll<HTMLElement>("[aria-controls]")).find((el) => el.getAttribute("aria-controls") === panelId);

// ── Docked filter panel ─────────────────────────────────────────────────────────────
export function ListFilterDock({ id, open, onClose, ...panel }: ListFiltersProps & { id: string; open: boolean; onClose: () => void }) {
  const ref = useRef<HTMLDivElement>(null);
  const wasOpen = useRef(open);
  useEffect(() => {
    if (open === wasOpen.current) return;
    wasOpen.current = open;
    const dock = ref.current;
    const toggle = filterToggle(id);
    const active = document.activeElement;
    if (open) {
      // Only when the user opened it from the toggle: a deep link or an applied view that opens
      // the panel leaves focus where it is.
      if (active && active === toggle) dock?.querySelector<HTMLElement>(".slds-filters__body button, .slds-filters__footer button")?.focus();
      return;
    }
    if (!active || active === document.body || dock?.contains(active)) toggle?.focus();
  }, [open, id]);
  return (
    <div id={id} ref={ref} className={clsx("slds-grid slds-shrink-none", !open && "slds-hide")}>
      <FilterPanel {...panel} onClose={onClose} className="slds-size_medium" />
    </div>
  );
}

// ── Active-filter pills ───────────────────────────────────────────────────────────
export function ListFilterPills({ fields, value, onChange, panelId, className }: ListFiltersProps & { panelId: string; className?: string }) {
  const active = activeFilters(value);
  // Removing the last pill unmounts the list, and the kit PillContainer's "focus the neighbour"
  // step with it: focus goes to the filter toggle instead of falling to <body>.
  const removedLast = useRef(false);
  useEffect(() => {
    if (!removedLast.current) return;
    removedLast.current = false;
    const a = document.activeElement;
    if (!a || a === document.body) filterToggle(panelId)?.focus();
  });
  if (active.length === 0) return null;
  const byName = new Map(fields.map((f) => [f.name, f]));
  return (
    // "selection" = pills/listbox-of-pill-options without the input-like border of a plain pill
    // container; slds-is-expanded lifts its one-row clip so every condition shows.
    <PillContainer variant="selection" label={t("page.list.filters.active")} className={clsx("slds-is-expanded", className)}>
      {active.map((f) => {
        const field = byName.get(f.field);
        return (
          <Pill
            key={f.id}
            label={`${field ? fieldLabel(field) : f.field} ${conditionText(f, field)}`}
            removeLabel={t("page.list.filters.removeChip")}
            onRemove={() => {
              removedLast.current = active.length === 1;
              onChange(value.filter((x) => x.id !== f.id));
            }}
          />
        );
      })}
    </PillContainer>
  );
}
