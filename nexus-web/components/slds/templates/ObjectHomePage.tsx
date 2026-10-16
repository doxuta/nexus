"use client";

// List-view page template (specs/038 contracts/shell.md "Template trang", RESEARCH §5.5 #1).
// Presentation only: no fetching, no metadata — the L2 list renderer (LayoutList) plugs its state
// and body in here during the Phase 2 migration. Layout, all SLDS 2.264.1:
//   article.slds-card > PageHeader object-home + div.slds-grid > div.slds-col (body | empty) + filterPanel
// The card is the blueprint's own "Inside a card" example (page-headers/object-home, examples:
// card): `.slds-card .slds-page-header` drops the header's border and shadow, so the only line
// between header and list is the body's: the `slds-table_bordered` top of the data table (1px), or
// `slds-border_top` on the column for the empty state (same colour token). The blueprint ends at the
// header; the list row under it is our composition. Not the layout magnets: a bottom-magnet header
// keeps its 1px bottom border and a top-magnet card adds its own, a 2px seam (3px over a bordered
// table). The filter panel slot sits in the same flex row so a docked `slds-panel`
// (components/slds/Panel) stretches to the list height.
// Controls row, in Lightning's order: search (input "Left Icon & Clear Button"), density and
// "display as" menus (button-icon-more + menus/dropdown, like the blueprint's "List View
// Controls"/"Change view"), extra `controls`, then the stateful filter toggle (button-icons/stateful).
import clsx from "clsx";
import { Calendar, ChevronDown, Filter, Rows3, Search, SquareKanban, Table, X, type LucideIcon } from "lucide-react";
import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { t } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { useEmbedded } from "../EmbeddedSurface";
import { Menu, MenuCheckboxItem } from "../Menu";
import { PageHeader } from "../PageHeader";
import { Popover } from "../Popover";
import { ActionGroup, type PageAction } from "./ActionGroup";

export type ListViewMode = "table" | "kanban" | "calendar";
export type ListDensity = "compact" | "normal" | "relaxed";

const VIEWS: Record<ListViewMode, { glyph: LucideIcon; label: I18nKey }> = {
  table: { glyph: Table, label: "slds.templates.view.table" },
  kanban: { glyph: SquareKanban, label: "slds.templates.view.kanban" },
  calendar: { glyph: Calendar, label: "slds.templates.view.calendar" },
};
const DENSITIES: Record<ListDensity, I18nKey> = {
  compact: "slds.templates.density.compact",
  normal: "slds.templates.density.normal",
  relaxed: "slds.templates.density.relaxed",
};

/** specs/078 FR-052: below this width of the work surface the filter panel is a Popover, not a right column. */
export const FILTER_COLUMN_MIN_WIDTH = 1040;

/** True when the element is measured narrower than the threshold; unmeasured (0 / no ResizeObserver) counts as wide. */
function useNarrow(ref: React.RefObject<HTMLElement | null>, on: boolean): boolean {
  const [narrow, setNarrow] = useState(false);
  useEffect(() => {
    const el = ref.current;
    if (!on || !el || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver((entries) => {
      const w = entries[0]?.contentRect.width ?? 0;
      setNarrow(w > 0 && w < FILTER_COLUMN_MIN_WIDTH);
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, [ref, on]);
  return on && narrow;
}

export interface ObjectHomePageProps {
  icon: { glyph: LucideIcon; name: string };
  /** Object plural above the title, e.g. "Khách hàng". */
  label: string;
  /** Current list view name, e.g. "Tất cả khách hàng". */
  title: string;
  /** List-view switcher (saved views) next to the title. */
  titleMenu?: ReactNode;
  /** Page actions: the first `maxVisibleActions` are buttons, the rest go into the overflow menu. */
  actions?: PageAction[];
  maxVisibleActions?: number;
  /** "12 mục · cập nhật 3 phút trước". */
  meta?: ReactNode;
  search?: { value: string; onChange: (value: string) => void; placeholder?: string };
  view?: { value: ListViewMode; onChange: (value: ListViewMode) => void; options?: ListViewMode[] };
  density?: { value: ListDensity; onChange: (value: ListDensity) => void };
  /** Filter toggle; `panelId` is the id of the filter panel it opens (aria-controls). */
  filter?: { open: boolean; onToggle: () => void; panelId?: string };
  /** Extra list controls (refresh, export…), placed before the filter toggle. */
  controls?: ReactNode[];
  /** Right-docked filter panel (FilterPanel / Panel). */
  filterPanel?: ReactNode;
  /** Replaces the body when given (EmptyState). */
  empty?: ReactNode;
  /** DataTable / Kanban / Calendar. */
  children?: ReactNode;
  className?: string;
}

function SearchControl({ value, onChange, placeholder }: NonNullable<ObjectHomePageProps["search"]>) {
  const id = useId();
  const input = useRef<HTMLInputElement>(null);
  const clear = t("slds.templates.clearSearch");
  return (
    <div className="slds-form-element">
      <label className="slds-form-element__label slds-assistive-text" htmlFor={id}>
        {t("slds.templates.searchList")}
      </label>
      <div
        className={clsx(
          "slds-form-element__control slds-input-has-icon",
          value ? "slds-input-has-icon_left-right" : "slds-input-has-icon_left",
        )}
      >
        <Search aria-hidden="true" className="slds-icon slds-input__icon slds-input__icon_left slds-icon-text-default nx-lucide" />
        <input
          ref={input}
          id={id}
          type="text"
          className="slds-input"
          placeholder={placeholder ?? t("slds.templates.searchPlaceholder")}
          value={value}
          onChange={(e) => onChange(e.target.value)}
        />
        {value && (
          <button
            type="button"
            className="slds-button slds-button_icon slds-input__icon slds-input__icon_right"
            title={clear}
            onClick={() => {
              onChange("");
              input.current?.focus();
            }}
          >
            <X aria-hidden="true" className="slds-button__icon slds-icon-text-light nx-lucide" />
            <span className="slds-assistive-text">{clear}</span>
          </button>
        )}
      </div>
    </div>
  );
}

/**
 * Blueprint "button-icon-more": glyph + small down arrow, opening a menus/dropdown. A plain element,
 * not a component: Radix's Trigger (asChild) must put its ref and handlers on the <button> itself.
 */
function moreTrigger(Glyph: LucideIcon, label: string) {
  return (
    <button type="button" className="slds-button slds-button_icon slds-button_icon-more" title={label}>
      <Glyph aria-hidden="true" className="slds-button__icon nx-lucide" />
      <ChevronDown aria-hidden="true" className="slds-button__icon slds-button__icon_x-small nx-lucide" />
      <span className="slds-assistive-text">{label}</span>
    </button>
  );
}

export function ObjectHomePage({
  icon,
  label,
  title,
  titleMenu,
  actions,
  maxVisibleActions = 1,
  meta,
  search,
  view,
  density,
  filter,
  controls,
  filterPanel,
  empty,
  children,
  className,
}: ObjectHomePageProps) {
  const embedded = useEmbedded();
  const articleRef = useRef<HTMLElement>(null);
  const popoverFilter = useNarrow(articleRef, embedded && !!filterPanel);
  const row: ReactNode[] = [];
  if (search) row.push(<SearchControl key="search" {...search} />);
  if (density) {
    row.push(
      <Menu key="density" align="right" trigger={moreTrigger(Rows3, t("slds.templates.density"))}>
        {(Object.keys(DENSITIES) as ListDensity[]).map((d) => (
          <MenuCheckboxItem key={d} label={t(DENSITIES[d])} checked={d === density.value} onCheckedChange={() => density.onChange(d)} />
        ))}
      </Menu>,
    );
  }
  if (view) {
    const current = VIEWS[view.value];
    row.push(
      <Menu
        key="view"
        align="right"
        trigger={moreTrigger(current.glyph, `${t("slds.templates.viewAs")} ${t(current.label)}`)}
      >
        {(view.options ?? (Object.keys(VIEWS) as ListViewMode[])).map((v) => (
          <MenuCheckboxItem key={v} label={t(VIEWS[v].label)} checked={v === view.value} onCheckedChange={() => view.onChange(v)} />
        ))}
      </Menu>,
    );
  }
  if (controls) row.push(...controls);
  if (filter) {
    const name = t("slds.templates.filters");
    const toggle = (
      <button
        key="filter"
        type="button"
        className={clsx("slds-button slds-button_icon slds-button_icon-border-filled", filter.open && "slds-is-selected")}
        aria-pressed={filter.open}
        aria-controls={filter.panelId}
        title={name}
        onClick={popoverFilter ? undefined : filter.onToggle}
      >
        <Filter aria-hidden="true" className="slds-button__icon nx-lucide" />
        <span className="slds-assistive-text">{name}</span>
      </button>
    );
    // Narrow work surface: the panel opens under the toggle instead of squeezing the table (the Popover owns open/close).
    row.push(
      popoverFilter ? (
        <Popover
          key="filter"
          trigger={toggle}
          label={name}
          closeButton={false}
          side="bottom"
          align="end"
          bodyClassName="slds-p-around_none"
          headerOutside
          open={filter.open}
          onOpenChange={(o) => o !== filter.open && filter.onToggle()}
        >
          {filterPanel}
        </Popover>
      ) : (
        toggle
      ),
    );
  }

  // Embedded (specs/073 FR-005): the work surface is the one card; this list is a flat section of it.
  return (
    <article ref={articleRef} className={clsx(!embedded && "slds-card", className)}>
      <PageHeader
        variant="object-home"
        icon={icon}
        label={label}
        title={title}
        titleMenu={titleMenu}
        meta={meta}
        actions={actions && actions.length > 0 ? [<ActionGroup key="actions" actions={actions} maxVisible={maxVisibleActions} />] : undefined}
        controls={row}
      />
      <div className="slds-grid">
        {/* Truthiness, not `??`: callers write `empty={rows.length === 0 && <EmptyState/>}`.
            The empty state has no table to draw the seam, so the column draws it (same 1px token). */}
        <div className={clsx("slds-col min-w-0", empty && "slds-border_top")}>{empty ? empty : children}</div>
        {!popoverFilter && filterPanel}
      </div>
    </article>
  );
}
