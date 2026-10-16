"use client";

// SLDS data table (blueprint `data-tables` advanced + inline-edit, SLDS 2.264.1) on
// @tanstack/react-table. Markup is the blueprint's:
//   table.slds-table.slds-table_bordered.slds-table_fixed-layout[role=grid]
//     thead > tr.slds-line-height_reset > th (select-all: role=cell · column: scope=col · actions)
//     tbody > tr.slds-hint-parent[aria-selected] > td[role=gridcell] (select) · th[scope=row] · td[role=gridcell]…
// Sort glyph is always an ArrowDown with `slds-is-sortable__icon`: SLDS itself rotates it 180° under
// `slds-is-sorted_asc`. The row action trigger is the blueprint's `down` glyph (ChevronDown), not ⋮.
// Inline edit adds div.slds-table_edit_container, the errors/row-number column, td.slds-cell-edit with
// a pencil (or a lock for read-only columns) and the section.slds-popover_edit editor. A row with errors
// shows its error button, which opens section.slds-popover.slds-popover_error listing them all
// (inline-edit `row-error-focused`; body per popovers/error: one message a <p>, more a ul.slds-list_dotted).
// Resizable columns: the blueprint's .slds-resizable range input (keyboard) + handle (pointer drag).
// Deviations: the pinned header is CSS `position: sticky` on thead th instead of SLDS's
// slds-table_header-fixed (which duplicates every header into an absolutely placed .slds-cell-fixed);
// past 100 rows the body is windowed (react-virtual) with spacer rows.
// Deviations (additive): aria-rowcount/aria-rowindex/aria-colindex so a screen reader keeps its place
// when rows are virtualised; `tabular-nums` + right alignment on numeric columns (meta.numeric);
// aria-describedby on edited ("Chưa lưu") and errored (the message) cells, which SLDS leaves silent.
import * as PopoverPrimitive from "@radix-ui/react-popover";
import {
  flexRender,
  functionalUpdate,
  getCoreRowModel,
  getSortedRowModel,
  useReactTable,
  type Column,
  type ColumnDef,
  type ColumnSizingState,
  type Header,
  type Row,
  type RowData,
  type RowSelectionState,
  type SortingState,
  type Updater,
} from "@tanstack/react-table";
import { defaultRangeExtractor, useVirtualizer } from "@tanstack/react-virtual";
import clsx from "clsx";
import { ArrowDown, ChevronDown, CircleX, Lock, Pencil, X } from "lucide-react";
import {
  Fragment,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  type FocusEvent,
  type KeyboardEvent,
  type MouseEvent,
  type ReactNode,
} from "react";
import { t } from "@/lib/i18n";
import { ButtonIcon } from "./ButtonIcon";
import { Menu } from "./Menu";
import { focusFirst } from "./focus";
import { Nubbed } from "./Popover";
import { Spinner } from "./Spinner";

declare module "@tanstack/react-table" {
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  interface ColumnMeta<TData extends RowData, TValue> {
    /** Number / currency / percent: right-aligned with tabular figures. */
    numeric?: boolean;
    /** Inline edit (DataTable `editable`): the cell gets the pencil; otherwise it is locked. */
    editable?: boolean;
    /** Inline edit: the value may not be empty. */
    required?: boolean;
    /** Long text that must be read in full (blueprint data-tables/base "Cell content wrapped"):
     *  `td.slds-cell-wrap` (pre-line + break-word) around the text, full text also in `title`.
     *  Deviation: the blueprint adds `slds-line-clamp` (3 lines); not here — a clamp hides the tail,
     *  and in a verbatim error message the tail (the receiver's HTTP status) is the point.
     *  Read-only tables only; an editable cell keeps the truncated line under its pencil. */
    wrap?: boolean;
  }
}

export interface DataTableProps<TData> {
  columns: ColumnDef<TData, any>[]; // eslint-disable-line @typescript-eslint/no-explicit-any
  data: TData[];
  /** Accessible name of the grid (aria-label). */
  label: string;
  /** Stable row id — keys selection. Default: `row.id` when present, else the index. */
  getRowId?: (row: TData, index: number) => string;
  /** Column rendered as the row header (`th scope=row`). Default: the first column. */
  rowHeader?: string;
  bordered?: boolean;

  /** Clickable headers (`slds-is-sortable`). A column opts out with `enableSorting: false`. */
  sortable?: boolean;
  sorting?: SortingState;
  defaultSorting?: SortingState;
  onSortingChange?: (sorting: SortingState) => void;
  /** Rows arrive already sorted (server-side sort): the table only reports the new sort. */
  manualSorting?: boolean;

  /** Checkbox column + select-all (`aria-multiselectable`). */
  selectable?: boolean;
  /** Keyed by row id (see getRowId). */
  rowSelection?: RowSelectionState;
  defaultRowSelection?: RowSelectionState;
  onRowSelectionChange?: (selection: RowSelectionState) => void;

  /** Drag / keyboard column resize (`.slds-resizable` handle + range per header); widths in px keyed by column id. */
  resizable?: boolean;
  columnSizing?: ColumnSizingState;
  defaultColumnSizing?: ColumnSizingState;
  onColumnSizingChange?: (sizing: ColumnSizingState) => void;

  /** Scroll the body inside this height with the header pinned. Past 100 rows the body is
   *  virtualised and a height is required: default 40rem. */
  maxHeight?: string | number;

  /** Menu items for the row action menu (MenuItem …); adds the trailing actions column. */
  rowActions?: (row: TData) => ReactNode;

  /** Inline edit (data-tables/inline-edit): errors + row-number column, pencil per `meta.editable`
   *  column, edits kept as drafts (`slds-is-edited`) until the caller saves them. */
  editable?: boolean;
  drafts?: CellDrafts;
  defaultDrafts?: CellDrafts;
  onDraftsChange?: (drafts: CellDrafts) => void;
  /** Per-cell error messages (`slds-has-error`, shown in the edit popover). */
  cellErrors?: CellErrors;

  /** Below 768px each row becomes a card, one `label: value` line per cell (`slds-max-medium-table_stacked`,
   *  labels from the column headers via `data-label`). Off by default. */
  stacked?: boolean;
  /** Extra class per body row (e.g. `nx-proposed` for a row an AI proposal touches). */
  rowClassName?: (row: TData) => string | undefined;
  /** Whole-row action (navigate): click, or Enter on a cell that holds no control. Clicks on a control
   *  inside the row (link, button, checkbox...) stay with that control. */
  onRowClick?: (row: TData) => void;
  /** Full-width content in a row of its own right under a body row (an in-place confirm, a one-time secret).
   *  Return null for the rows that have none. Not for virtualised (>100 rows) tables. */
  rowDetail?: (row: TData) => ReactNode;

  loading?: boolean;
  /** Shown instead of the body when there are no rows (e.g. an illustration). */
  empty?: ReactNode;
  className?: string;
}

/** Value + setter that follows the prop when given, else its own state (tanstack-style updaters). */
export function useControlled<T>(value: T | undefined, onChange: ((next: T) => void) | undefined, initial: T) {
  const [inner, setInner] = useState(initial);
  const current = value ?? inner;
  const set = (updater: Updater<T>) => {
    const next = functionalUpdate(updater, current);
    setInner(next);
    onChange?.(next);
  };
  return [current, set] as const;
}

export const fill = (template: string, values: Record<string, string | number>) =>
  template.replace(/\{(\w+)\}/g, (m, k: string) => (k in values ? String(values[k]) : m));

/** Plain text of a column header, for titles and assistive labels. */
export function headerText<TData>(column: Column<TData, unknown>) {
  const h = column.columnDef.header;
  return typeof h === "string" ? h : column.id;
}

/** Plain text of a cell value (title attribute / row names), or undefined for rich values. */
export function cellText(value: unknown) {
  return typeof value === "string" || typeof value === "number" ? String(value) : undefined;
}

// ── Grid keyboard model (SLDS data-tables docs "Navigation mode" / "Actionable mode") ────────────
// Navigation mode: ONE tab stop (the active cell), arrow keys move it, nothing inside cells is
// tabbable. Enter: actionable mode — every control in the grid becomes tabbable and arrows land on the
// first control of the next cell. Esc: back to navigation mode on the same cell. Focus leaving the grid
// also resets to navigation mode, so Tab back in returns to the last cell.
// Position is (row, col) coordinates, not a DOM ref: virtualised rows unmount off-screen (all but the
// active one, which DataTable keeps mounted so the tab stop and any focus inside it survive scrolling).
// Row 0 is the header row. With `rowFocus` (tree grid) col -1 is the row itself — body rows only: the
// header row has no row focus, so arrows into it (and Ctrl+Home) land on its cells.

/** Controls inside cells whose tab stop the grid owns. */
const INNER = "a[href], button, input, select, textarea, [role='button']";
/** Where keys belong to a widget, not to the grid: menus, popovers (the inline editor is one), listboxes.
 *  Only such widgets INSIDE the grid count — the grid itself may sit in a modal (role=dialog). */
const OWN_KEYS = "[role='menu'], [role='dialog'], [role='listbox']";
/** Controls that keep arrows/Home/End for themselves (Esc still leaves actionable mode). */
const TYPING = "input:not([type='checkbox']):not([type='radio']), textarea, select, [contenteditable='true']";

export type GridMode = "nav" | "action";
export interface GridPos {
  r: number;
  c: number;
}
export interface CellProps {
  "aria-colindex": number;
  tabIndex: number;
  className?: string;
}

/** Is `el` inside a widget (OWN_KEYS) that is itself inside `grid`? */
const inWidget = (el: HTMLElement, grid: HTMLElement) => {
  const owner = el.closest(OWN_KEYS);
  return owner !== null && grid.contains(owner);
};
/** Controls the grid manages: not inside a menu/popover, and not hidden from AT (the tree grid's
 *  expand chevron and the inline-edit error button without errors are aria-hidden, tabindex -1). */
const managed = (el: HTMLElement, grid: HTMLElement) => !inWidget(el, grid) && el.getAttribute("aria-hidden") !== "true";
const firstInner = (el: HTMLElement, grid: HTMLElement) =>
  Array.from(el.querySelectorAll<HTMLElement>(INNER)).find((c) => managed(c, grid) && !c.hasAttribute("disabled"));

export function useGridKeyboard({
  rowCount,
  colCount,
  initial,
  rowFocus = false,
  onKey,
}: {
  /** Data rows (the header row is row 0). */
  rowCount: number;
  colCount: number;
  /** Where the tab stop sits before the user moves it. */
  initial: GridPos;
  rowFocus?: boolean;
  /** Extra keys (tree grid expand/collapse): return true when handled, or a position to move to. */
  onKey?: (e: KeyboardEvent<HTMLElement>, pos: GridPos, mode: GridMode) => GridPos | boolean | undefined;
}) {
  const ref = useRef<HTMLTableElement>(null);
  const [pos, setPos] = useState<GridPos | null>(null);
  const [mode, setMode] = useState<GridMode>("nav");
  const [focusWithin, setFocusWithin] = useState(false);
  const pending = useRef(false);
  const minCol = (r: number) => (rowFocus && r > 0 ? -1 : 0);
  const p = pos ?? initial;
  const r0 = Math.max(0, Math.min(p.r, rowCount));
  const act = { r: r0, c: Math.max(minCol(r0), Math.min(p.c, colCount - 1)) };

  const find = (at: GridPos) => {
    const tr = ref.current?.querySelector<HTMLElement>(`tr[aria-rowindex="${at.r + 1}"]`);
    if (!tr || at.c < 0) return tr;
    return Array.from(tr.children).find((el) => el.getAttribute("aria-colindex") === String(at.c + 1)) as HTMLElement | undefined;
  };
  const moveTo = (at: GridPos, next: GridMode = mode) => {
    setPos(at);
    setMode(next);
    pending.current = true;
  };

  // The grid owns the tab stop of every control rendered inside it (caller cells included).
  useLayoutEffect(() => {
    const grid = ref.current;
    if (!grid) return;
    for (const el of grid.querySelectorAll<HTMLElement>(INNER)) {
      if (managed(el, grid)) el.setAttribute("tabindex", mode === "action" ? "0" : "-1");
    }
  });

  useEffect(() => {
    if (!pending.current) return;
    const el = find(act); // a virtualised grid keeps the active row mounted (DataTable rangeExtractor)
    if (!el) return;
    pending.current = false;
    ((mode === "action" && act.c >= 0 && firstInner(el, ref.current!)) || el).focus();
  });

  const onKeyDown = (e: KeyboardEvent<HTMLElement>) => {
    const target = e.target as HTMLElement;
    const grid = e.currentTarget;
    // Portaled content bubbles here through the React tree; its keys are not the grid's.
    if (!grid.contains(target) || inWidget(target, grid)) return;
    if (target.matches(TYPING) && e.key !== "Escape") return;
    const handled = onKey?.(e, act, mode);
    if (handled) {
      e.preventDefault();
      if (handled !== true) moveTo(handled);
      return;
    }
    const here = find(act);
    const onCell = target === here;
    let { r, c } = act;
    switch (e.key) {
      case "ArrowUp":
        r = Math.max(0, r - 1);
        break;
      case "ArrowDown":
        r = Math.min(rowCount, r + 1);
        break;
      case "ArrowLeft":
        c = Math.max(minCol(r), c - 1);
        break;
      case "ArrowRight":
        c = Math.min(colCount - 1, c + 1);
        break;
      case "Home":
        if (e.ctrlKey) r = 0;
        c = minCol(r);
        break;
      case "End":
        c = colCount - 1;
        if (e.ctrlKey) r = rowCount;
        break;
      case "Enter":
        if (mode === "nav" && onCell) {
          e.preventDefault();
          moveTo(act, "action");
        }
        return;
      case "Escape":
        if (mode === "action") {
          e.preventDefault();
          moveTo(act, "nav");
        }
        return;
      case " ": {
        // Space on a selection cell (or a tree row) toggles its checkbox.
        const box = mode === "nav" && onCell ? here?.querySelector<HTMLInputElement>("input[type='checkbox']") : null;
        if (box) {
          e.preventDefault();
          box.click();
        }
        return;
      }
      default:
        return;
    }
    e.preventDefault();
    moveTo({ r, c });
  };

  const onFocus = (e: FocusEvent<HTMLElement>) => {
    setFocusWithin(true);
    const target = e.target as HTMLElement;
    const tr = target.closest<HTMLElement>("tr[aria-rowindex]");
    if (!tr) return;
    const cell = target.closest<HTMLElement>("[aria-colindex]");
    const at = {
      r: Number(tr.getAttribute("aria-rowindex")) - 1,
      c: cell && tr.contains(cell) ? Number(cell.getAttribute("aria-colindex")) - 1 : -1,
    };
    if (at.r !== act.r || at.c !== act.c) setPos(at);
    // Focus on a control inside a cell (mouse click, Tab) means the user is interacting.
    if (target !== cell && target !== tr) setMode("action");
  };

  const onBlur = (e: FocusEvent<HTMLElement>) => {
    if (e.relatedTarget instanceof Node && e.currentTarget.contains(e.relatedTarget)) return;
    setFocusWithin(false);
    setMode("nav");
  };

  const isActive = (r: number, c: number) => act.r === r && act.c === c;
  const cellProps = (r: number, c: number, className?: string): CellProps => ({
    "aria-colindex": c + 1,
    tabIndex: mode === "nav" && isActive(r, c) ? 0 : -1,
    className:
      clsx(className, focusWithin && isActive(r, c) && "slds-has-focus", mode === "action" && "slds-cell_action-mode") || undefined,
  });
  const rowTabIndex = (r: number) => (mode === "nav" && isActive(r, -1) ? 0 : -1);

  return { gridProps: { ref, onKeyDown, onFocus, onBlur }, act, mode, cellProps, rowTabIndex, moveTo };
}

/** Bare blueprint checkbox for grid cells: no form-element wrapper, label hidden from AT. */
export function SelectBox({
  label,
  checked,
  indeterminate = false,
  disabled,
  onChange,
}: {
  label: string;
  checked: boolean;
  indeterminate?: boolean;
  disabled?: boolean;
  onChange: (checked: boolean) => void;
}) {
  const id = useId();
  const ref = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (ref.current) ref.current.indeterminate = indeterminate;
  }, [indeterminate]);
  return (
    <div className="slds-checkbox">
      <input
        ref={ref}
        type="checkbox"
        id={id}
        aria-label={label}
        checked={checked}
        disabled={disabled}
        onChange={(e) => onChange(e.target.checked)}
      />
      <label className="slds-checkbox__label" htmlFor={id} aria-hidden="true">
        <span className="slds-checkbox_faux" />
        <span className="slds-form-element__label slds-assistive-text">{label}</span>
      </label>
    </div>
  );
}

/** Column header cell (`th scope=col`): sortable link button or plain action area. */
const MIN_WIDTH = 20; // blueprint range input min/max
const MAX_WIDTH = 1000;

export function ColumnHeader<TData>({
  header,
  cell,
  className,
}: {
  header: Header<TData, unknown>;
  cell: CellProps;
  className?: string;
}) {
  const { column } = header;
  const table = header.getContext().table;
  const text = headerText(column);
  const canResize = column.getCanResize();
  const width: number | undefined = table.getState().columnSizing[column.id] ?? column.columnDef.size;
  const drag = useRef<{ x: number; w: number } | null>(null);
  const resize = (w: number) =>
    table.setColumnSizing((old) => ({ ...old, [column.id]: Math.round(Math.min(MAX_WIDTH, Math.max(MIN_WIDTH, w))) }));
  // Rendered width (fixed layout shares space among unsized columns); happy-dom has none.
  const measured = (el: Element) => el.closest("th")?.getBoundingClientRect().width || width || column.getSize();
  const numeric = column.columnDef.meta?.numeric;
  const canSort = column.getCanSort();
  const sorted = column.getIsSorted();
  const content = (
    <div className={clsx("slds-grid slds-grid_vertical-align-center slds-has-flexi-truncate", numeric && "slds-grid_align-end")}>
      <span className="slds-truncate" title={text}>
        {flexRender(column.columnDef.header, header.getContext())}
      </span>
      {canSort && (
        <span className="slds-icon_container slds-icon-utility-arrowdown">
          <ArrowDown aria-hidden="true" className="slds-icon slds-icon-text-default slds-is-sortable__icon nx-lucide" />
        </span>
      )}
    </div>
  );
  return (
    <th
      scope="col"
      aria-sort={canSort ? (sorted === "asc" ? "ascending" : sorted === "desc" ? "descending" : "none") : undefined}
      {...cell}
      style={width !== undefined ? { width } : undefined}
      className={clsx(
        cell.className,
        className,
        canResize && "slds-is-resizable",
        canSort && "slds-is-sortable",
        sorted && `slds-is-sorted slds-is-sorted_${sorted}`,
        numeric && "slds-text-align_right",
      )}
    >
      {canSort ? (
        <a
          className="slds-th__action slds-text-link_reset"
          href="#"
          role="button"
          onClick={(e) => {
            e.preventDefault();
            column.toggleSorting(undefined, false);
          }}
          onKeyDown={(e) => {
            // Anchors don't activate on Space; a role=button must.
            if (e.key !== " ") return;
            e.preventDefault();
            e.currentTarget.click();
          }}
        >
          <span className="slds-assistive-text">{t("slds.dataTable.sortBy")}</span>
          {content}
        </a>
      ) : (
        <div className="slds-th__action">{content}</div>
      )}
      {sorted && (
        <span className="slds-assistive-text" aria-live="polite" aria-atomic="true">
          {t(sorted === "asc" ? "slds.dataTable.sortedAsc" : "slds.dataTable.sortedDesc")}
        </span>
      )}
      {canResize && (
        <div className="slds-resizable">
          <input
            type="range"
            className="slds-resizable__input slds-assistive-text"
            aria-label={fill(t("slds.dataTable.columnWidth"), { col: text })}
            min={MIN_WIDTH}
            max={MAX_WIDTH}
            value={width ?? column.getSize()}
            // Pin the rendered width first so arrow keys step from what the user sees.
            onFocus={(e) => width === undefined && resize(measured(e.currentTarget))}
            onChange={(e) => resize(Number(e.target.value))}
          />
          <span
            className="slds-resizable__handle"
            onPointerDown={(e) => {
              e.preventDefault();
              e.currentTarget.setPointerCapture?.(e.pointerId);
              drag.current = { x: e.clientX, w: measured(e.currentTarget) };
            }}
            onPointerMove={(e) => drag.current && resize(drag.current.w + e.clientX - drag.current.x)}
            onPointerUp={() => (drag.current = null)}
            onPointerCancel={() => (drag.current = null)}
          >
            <span className="slds-resizable__divider" />
          </span>
        </div>
      )}
    </th>
  );
}

/** Trailing actions header: fixed 3.25rem, label for AT only. */
export function ActionsHeader({ cell, className }: { cell: CellProps; className?: string }) {
  return (
    <th scope="col" {...cell} className={clsx(cell.className, className) || undefined} style={{ width: "3.25rem" }}>
      <div className="slds-truncate slds-assistive-text" title={t("slds.dataTable.actions")}>
        {t("slds.dataTable.actions")}
      </div>
    </th>
  );
}

/** Row action menu: the blueprint's border-filled x-small `down` button icon. */
export function RowActions({ rowName, children }: { rowName: string; children: ReactNode }) {
  return (
    <Menu
      align="right"
      trigger={
        <ButtonIcon
          icon={ChevronDown}
          label={fill(t("slds.dataTable.rowActions"), { row: rowName })}
          variant="border-filled"
          size="x-small"
          iconSize="small"
          hint
        />
      }
    >
      {children}
    </Menu>
  );
}

/** Inline-edit popover (`section.slds-popover.slds-popover_edit[role=dialog]`) anchored in its cell.
 *  Enter saves, Esc cancels (both hand focus back via the callbacks); leaving it saves too. */
function EditPopover({
  label,
  value,
  required,
  error,
  onCommit,
  onCancel,
}: {
  label: string;
  value: string;
  required?: boolean;
  error?: string;
  onCommit: (value: string, refocus: boolean) => void;
  onCancel: (refocus: boolean) => void;
}) {
  const id = useId();
  const [text, setText] = useState(value);
  const [missing, setMissing] = useState(false);
  const done = useRef(false);
  const input = useRef<HTMLInputElement>(null);
  useEffect(() => input.current?.focus(), []);
  const empty = required && !text.trim();
  const message = missing ? t("slds.dataTable.required") : error;
  const finish = (save: boolean, refocus: boolean) => {
    done.current = true;
    if (save) onCommit(text, refocus);
    else onCancel(refocus);
  };
  return (
    <section
      role="dialog"
      aria-describedby={`${id}-body`}
      className="slds-popover slds-popover_edit"
      style={{ position: "absolute", top: 0, left: "0.0625rem" }}
      onBlur={(e) => {
        if (done.current || (e.relatedTarget instanceof Node && e.currentTarget.contains(e.relatedTarget))) return;
        finish(!empty, false); // an empty required value is dropped, never saved
      }}
    >
      <div className="slds-popover__body" id={`${id}-body`}>
        <div className="slds-media">
          <div className="slds-media__body">
            <div className={clsx("slds-form-element slds-grid slds-wrap", message && "slds-has-error")}>
              <label className="slds-form-element__label slds-form-element__label_edit slds-no-flex" htmlFor={`${id}-input`}>
                {required && (
                  <abbr className="slds-required" title={t("slds.formElement.required")} aria-hidden="true">
                    *
                  </abbr>
                )}
                <span className="slds-assistive-text">{label}</span>
              </label>
              <div className="slds-form-element__control slds-grow">
                <input
                  ref={input}
                  id={`${id}-input`}
                  className="slds-input"
                  type="text"
                  required={required}
                  value={text}
                  aria-describedby={message ? `${id}-help` : undefined}
                  onChange={(e) => {
                    setText(e.target.value);
                    setMissing(false);
                  }}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") {
                      e.preventDefault();
                      if (empty) setMissing(true);
                      else finish(true, true);
                    } else if (e.key === "Escape") {
                      e.preventDefault();
                      finish(false, true);
                    }
                  }}
                />
              </div>
              {message && (
                <div id={`${id}-help`} className="slds-form-element__help">
                  {message}
                </div>
              )}
            </div>
          </div>
        </div>
      </div>
    </section>
  );
}

/** Errors column cell content (inline-edit `row-error-focused`): the error button opens
 *  section.slds-popover.slds-popover_error with every error of the row, on @radix-ui/react-popover
 *  (Esc, click-outside, focus in and back to the button), content asChild and not portaled like
 *  Popover.tsx; top/start puts the nubbin (bottom-left) on the button. Without errors the button keeps
 *  its slot hidden from everyone (slds-hidden, aria-hidden), as in the blueprint. */
function RowErrors({ n, errors }: { n: number; errors: string[] }) {
  const id = useId();
  const ref = useRef<HTMLDivElement>(null);
  const label = fill(t("slds.dataTable.rowHasErrors"), { n });
  const button = (
    <button
      type="button"
      className={clsx("slds-button slds-button_icon slds-button_icon-error slds-m-horizontal_xxx-small", !errors.length && "slds-hidden")}
      aria-hidden={errors.length ? undefined : "true"}
      tabIndex={errors.length ? undefined : -1}
      title={label}
    >
      <CircleX aria-hidden="true" className="slds-button__icon nx-lucide" />
      <span className="slds-assistive-text">{label}</span>
    </button>
  );
  if (!errors.length) return button;
  const title = t("slds.dataTable.resolveErrors");
  const close = t("slds.popover.close");
  return (
    <PopoverPrimitive.Root>
      <PopoverPrimitive.Trigger asChild>{button}</PopoverPrimitive.Trigger>
      <PopoverPrimitive.Content
        asChild
        ref={ref}
        side="top"
        align="start"
        sideOffset={12}
        onOpenAutoFocus={(e) => {
          // Like Popover.tsx: SLDS focuses the first control that isn't the close button, else the close button.
          e.preventDefault();
          focusFirst(ref.current, "slds-popover__close");
        }}
      >
        <Nubbed
          as="section"
          sizeVar="--radix-popover-trigger"
          aria-labelledby={`${id}-heading`}
          aria-describedby={`${id}-body`}
          className="slds-popover slds-popover_error"
        >
          <PopoverPrimitive.Close asChild>
            <button type="button" className="slds-button slds-button_icon slds-button_icon-small slds-float_right slds-popover__close slds-button_icon-inverse" title={close}>
              <X aria-hidden="true" className="slds-button__icon nx-lucide" />
              <span className="slds-assistive-text">{close}</span>
            </button>
          </PopoverPrimitive.Close>
          <header className="slds-popover__header">
            <div className="slds-media slds-media_center slds-has-flexi-truncate">
              <div className="slds-media__figure">
                <span className="slds-icon_container slds-icon-utility-error">
                  <CircleX aria-hidden="true" className="slds-icon slds-icon_x-small nx-lucide" />
                </span>
              </div>
              <div className="slds-media__body">
                <h2 className="slds-truncate slds-text-heading_medium" id={`${id}-heading`} title={title}>
                  {title}
                </h2>
              </div>
            </div>
          </header>
          <div className="slds-popover__body" id={`${id}-body`}>
            {errors.length === 1 ? (
              <p>{errors[0]}</p>
            ) : (
              <ul className="slds-list_dotted">
                {errors.map((message) => (
                  <li key={message}>{message}</li>
                ))}
              </ul>
            )}
          </div>
        </Nubbed>
      </PopoverPrimitive.Content>
    </PopoverPrimitive.Root>
  );
}

const VIRTUALIZE_AFTER = 100;
const ROW_HEIGHT = 33; // .slds-table row with an x-small button icon; measured per row once rendered

/** Draft values keyed rowId → columnId (unsaved inline edits). */
export type CellDrafts = Record<string, Record<string, string>>;
/** Error messages keyed rowId → columnId (e.g. a 422 from the save). */
export type CellErrors = Record<string, Record<string, string>>;

export function DataTable<TData>({
  columns,
  data,
  label,
  getRowId,
  rowHeader,
  bordered = true,
  sortable = false,
  sorting: sortingProp,
  defaultSorting = [],
  onSortingChange,
  manualSorting = false,
  selectable = false,
  rowSelection: selectionProp,
  defaultRowSelection = {},
  onRowSelectionChange,
  rowActions,
  resizable = false,
  columnSizing: sizingProp,
  defaultColumnSizing = {},
  onColumnSizingChange,
  maxHeight,
  editable = false,
  drafts: draftsProp,
  defaultDrafts = {},
  onDraftsChange,
  cellErrors = {},
  stacked = false,
  rowClassName,
  rowDetail,
  onRowClick,
  loading = false,
  empty,
  className,
}: DataTableProps<TData>) {
  const [sorting, setSorting] = useControlled(sortingProp, onSortingChange, defaultSorting);
  const [rowSelection, setRowSelection] = useControlled(selectionProp, onRowSelectionChange, defaultRowSelection);
  const [drafts, setDrafts] = useControlled(draftsProp, onDraftsChange, defaultDrafts);
  const [columnSizing, setColumnSizing] = useControlled(sizingProp, onColumnSizingChange, defaultColumnSizing);
  const [editing, setEditing] = useState<{ rowId: string; colId: string } | null>(null);
  const returnTo = useRef<HTMLElement | null>(null);
  const noteId = useId();

  // TanStack Table returns fresh functions each render; the React Compiler skips memoising this
  // component on its own, which is what we want — the lint warning only reports that.
  // eslint-disable-next-line react-hooks/incompatible-library
  const table = useReactTable({
    data,
    columns,
    getRowId: getRowId ?? ((row, i) => String((row as { id?: unknown }).id ?? i)),
    state: { sorting, rowSelection, columnSizing },
    onSortingChange: setSorting,
    onRowSelectionChange: setRowSelection,
    onColumnSizingChange: setColumnSizing,
    enableColumnResizing: resizable,
    // Drop tanstack's 150px default so only declared (`size`) or resized columns get a width;
    // the rest share the table the way the fixed-layout blueprint does.
    defaultColumn: { size: undefined },
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: manualSorting ? undefined : getSortedRowModel(),
    manualSorting,
    enableSorting: sortable,
    enableSortingRemoval: false,
    enableMultiSort: false,
    sortDescFirst: false,
    enableRowSelection: selectable,
  });

  const leafColumns = table.getVisibleLeafColumns();
  const rowHeaderId = rowHeader ?? leafColumns[0]?.id;
  // Column layout: [errors (inline edit)] [select] data… [actions]
  const errorCol = editable ? 0 : -1;
  const selectCol = selectable ? (editable ? 1 : 0) : -1;
  const lead = (editable ? 1 : 0) + (selectable ? 1 : 0);
  const colCount = lead + leafColumns.length + (rowActions ? 1 : 0);
  const rows = table.getRowModel().rows;

  const rowName = (row: Row<TData>) => cellText(rowHeaderId ? row.getValue(rowHeaderId) : undefined) ?? row.id;
  const rowHeaderCol = lead + Math.max(0, leafColumns.findIndex((col) => col.id === rowHeaderId));
  const { gridProps, cellProps, act: active } = useGridKeyboard({
    rowCount: rows.length,
    colCount,
    initial: { r: rows.length ? 1 : 0, c: rows.length ? rowHeaderCol : 0 },
  });

  const virtual = rows.length > VIRTUALIZE_AFTER;
  const scrollHeight = maxHeight ?? (virtual ? "40rem" : undefined);
  const scrollRef = useRef<HTMLDivElement>(null);
  // Rows kept mounted outside the window: the active one (the grid's only tab stop, maybe holding
  // focus) and the one under an open editor (its draft text lives in the popover's state).
  const keep = [active.r - 1, editing ? rows.findIndex((row) => row.id === editing.rowId) : -1].filter((i) => i >= 0);
  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => ROW_HEIGHT,
    overscan: 10,
    enabled: virtual,
    initialRect: { width: 0, height: 640 }, // before layout (and under happy-dom)
    rangeExtractor: (range) => [...new Set([...keep, ...defaultRangeExtractor(range)])].sort((a, b) => a - b),
  });
  const windowed = virtual ? virtualizer.getVirtualItems() : null;
  const visible = windowed ? windowed.map((v) => v.index) : rows.map((_, i) => i);
  // Spacer heights: before each rendered row (kept rows make the set non-contiguous) and after the last.
  const gapBefore = new Map(windowed?.map((v, i) => [v.index, v.start - (i ? windowed[i - 1].end : 0)]));
  const padBottom = windowed?.length ? virtualizer.getTotalSize() - windowed[windowed.length - 1].end : 0;
  const spacer = (height: number) => (
    <tr aria-hidden="true">
      <td colSpan={colCount} className="p-0" style={{ height }} />
    </tr>
  );
  const pinned = scrollHeight !== undefined ? "sticky top-0 z-[1]" : undefined;

  // Row click: only a click that landed in this row's own DOM on something that is not a control
  // (portaled popovers bubble through the React tree; links/buttons/checkboxes keep their own click).
  const CONTROLS = "a[href], button, input, select, textarea, [role='button']";
  const rowClick = (e: MouseEvent<HTMLTableRowElement>, data: TData) => {
    const target = e.target as HTMLElement;
    if (!e.currentTarget.contains(target) || target.closest(`${CONTROLS}, label`)) return;
    onRowClick?.(data);
  };
  // Enter on a bare cell opens the row; a cell with a control keeps the grid's "enter the cell" Enter.
  const rowEnter = (e: KeyboardEvent<HTMLTableRowElement>, data: TData) => {
    const target = e.target as HTMLElement;
    if (e.key !== "Enter" || !target.matches("td, th") || !e.currentTarget.contains(target)) return;
    if (target.querySelector(CONTROLS)) return;
    e.preventDefault();
    e.stopPropagation();
    onRowClick?.(data);
  };

  const closeEditor = (refocus: boolean) => {
    setEditing(null);
    if (refocus) returnTo.current?.focus();
  };
  const saveDraft = (rowId: string, colId: string, value: string, original: string) =>
    setDrafts((prev) => {
      const row = { ...prev[rowId] };
      if (value === original) delete row[colId];
      else row[colId] = value;
      const next = { ...prev, [rowId]: row };
      if (Object.keys(row).length === 0) delete next[rowId];
      return next;
    });

  const body =
    rows.length === 0 && !loading ? (
      <tr>
        <td role="gridcell" colSpan={colCount}>
          {empty ?? <div className="slds-p-around_medium slds-text-align_center slds-text-color_weak">{t("slds.dataTable.empty")}</div>}
        </td>
      </tr>
    ) : null;

  const grid = (
    <table
      {...gridProps}
      role="grid"
      aria-label={label}
      aria-multiselectable={selectable ? "true" : undefined}
      aria-rowcount={rows.length + 1}
      aria-colcount={colCount}
      className={clsx(
        "slds-table",
        bordered && "slds-table_bordered",
        editable && "slds-table_edit",
        stacked && "slds-max-medium-table_stacked",
        // Always on, as in every headered advanced blueprint: it zeroes `thead th` padding, which
        // .slds-th__action already carries (without it the header is padded twice).
        "slds-table_fixed-layout slds-table_resizable-cols",
        className,
      )}
    >
      <thead>
        <tr className="slds-line-height_reset" aria-rowindex={1}>
          {editable && (
            <th scope="col" {...cellProps(0, errorCol, pinned)} style={{ width: "3.75rem" }}>
              <div className="slds-truncate slds-assistive-text" title={t("slds.dataTable.errors")}>
                {t("slds.dataTable.errors")}
              </div>
            </th>
          )}
          {selectable && (
            <th
              role="cell"
              {...cellProps(0, selectCol, clsx(pinned, !editable && "slds-text-align_right"))}
              style={{ width: editable ? "2rem" : "3.25rem" }}
            >
              <div className="slds-th__action slds-th__action_form">
                <SelectBox
                  label={t("slds.dataTable.selectAll")}
                  checked={table.getIsAllRowsSelected()}
                  indeterminate={table.getIsSomeRowsSelected()}
                  onChange={(v) => table.toggleAllRowsSelected(v)}
                />
              </div>
            </th>
          )}
          {table.getFlatHeaders().map((header, i) => (
            <ColumnHeader key={header.id} header={header} cell={cellProps(0, lead + i)} className={pinned} />
          ))}
          {rowActions && <ActionsHeader cell={cellProps(0, colCount - 1)} className={pinned} />}
        </tr>
      </thead>
      <tbody>
        {body ??
          visible.map((r) => {
            const row = rows[r];
            const selected = row.getIsSelected();
            const gr = r + 1; // grid row: 0 is the header
            const name = rowName(row);
            const rowErrors = cellErrors[row.id] ?? {};
            const errorList = leafColumns
              .filter((col) => rowErrors[col.id])
              .map((col) => fill(t("slds.dataTable.cellError"), { col: headerText(col), message: rowErrors[col.id] }));
            const edit = editable ? "slds-cell-edit" : undefined;
            const gap = gapBefore.get(r) ?? 0;
            return (
              <Fragment key={row.id}>
                {gap > 0 && spacer(gap)}
                <tr
                  ref={virtual ? virtualizer.measureElement : undefined}
                  data-index={r}
                  aria-rowindex={gr + 1}
                  aria-selected={selectable ? selected : undefined}
                  className={clsx("slds-hint-parent", selected && "slds-is-selected", onRowClick && "cursor-pointer", rowClassName?.(row.original))}
                  onClick={onRowClick && ((e) => rowClick(e, row.original))}
                  onKeyDown={onRowClick && ((e) => rowEnter(e, row.original))}
                  // Row numbers are a CSS counter; pin it so windowed (virtualised) rows keep theirs.
                  style={editable ? { counterSet: `row-number ${gr}` } : undefined}
                >
                  {editable && (
                    <td role="gridcell" {...cellProps(gr, errorCol, "slds-cell-edit slds-cell-error")}>
                      <RowErrors n={gr} errors={errorList} />
                      <span className="slds-row-number slds-text-body_small slds-text-color_weak" />
                    </td>
                  )}
                  {selectable && (
                    <td role="gridcell" {...cellProps(gr, selectCol, edit ?? "slds-text-align_right")}>
                      <SelectBox
                        label={fill(t("slds.dataTable.selectItem"), { n: gr })}
                        checked={selected}
                        disabled={!row.getCanSelect()}
                        onChange={(v) => row.toggleSelected(v)}
                      />
                    </td>
                  )}
                  {row.getVisibleCells().map((cell, c) => {
                    const { column } = cell;
                    const meta = column.columnDef.meta;
                    const Tag = column.id === rowHeaderId ? "th" : "td";
                    const original = cellText(cell.getValue()) ?? "";
                    const draft = drafts[row.id]?.[column.id];
                    const error = rowErrors[column.id];
                    const isEditing = editing?.rowId === row.id && editing.colId === column.id;
                    const locked = editable && !meta?.editable;
                    const text = draft ?? cellText(cell.getValue());
                    const content = draft ?? flexRender(column.columnDef.cell, cell.getContext());
                    const truncate = clsx("slds-truncate", meta?.numeric && "tabular-nums");
                    const buttonLabel = fill(t("slds.dataTable.edit"), { col: headerText(column), row: name });
                    const note = `${noteId}-${row.id}-${column.id}`;
                    return (
                      <Tag
                        key={cell.id}
                        scope={Tag === "th" ? "row" : undefined}
                        role={Tag === "td" ? "gridcell" : undefined}
                        data-label={stacked && typeof column.columnDef.header === "string" ? column.columnDef.header : undefined}
                        aria-readonly={locked ? "true" : undefined}
                        aria-describedby={clsx(draft !== undefined && note, error && `${note}-error`) || undefined}
                        {...cellProps(
                          gr,
                          lead + c,
                          // Blueprint edit/error states: the cell under the open editor is plain slds-cell-edit.
                          clsx(
                            edit,
                            !isEditing && draft !== undefined && "slds-is-edited",
                            !isEditing && error && "slds-has-error",
                            meta?.numeric && "slds-text-align_right",
                            !editable && meta?.wrap && "slds-cell-wrap",
                          ),
                        )}
                      >
                        {editable ? (
                          <>
                            <span className={clsx("slds-grid", meta?.numeric ? "slds-grid_align-end" : "slds-grid_align-spread")}>
                              <span className={truncate} title={text}>
                                {content}
                              </span>
                              <button
                                type="button"
                                className="slds-button slds-button_icon slds-cell-edit__button slds-m-left_x-small"
                                title={buttonLabel}
                                disabled={locked}
                                onClick={(e) => {
                                  returnTo.current = e.currentTarget;
                                  setEditing({ rowId: row.id, colId: column.id });
                                }}
                              >
                                {locked ? (
                                  <Lock aria-hidden="true" className="slds-button__icon slds-button__icon_hint slds-button__icon_lock slds-button__icon_small nx-lucide" />
                                ) : (
                                  <Pencil aria-hidden="true" className="slds-button__icon slds-button__icon_hint slds-button__icon_edit nx-lucide" />
                                )}
                                <span className="slds-assistive-text">{buttonLabel}</span>
                              </button>
                            </span>
                            {draft !== undefined && (
                              <span id={note} className="slds-hide">
                                {t("slds.dataTable.unsaved")}
                              </span>
                            )}
                            {error && (
                              <span id={`${note}-error`} className="slds-hide">
                                {error}
                              </span>
                            )}
                            {isEditing && (
                              <EditPopover
                                label={headerText(column)}
                                value={draft ?? original}
                                required={meta?.required}
                                error={error}
                                onCommit={(value, refocus) => {
                                  saveDraft(row.id, column.id, value, original);
                                  closeEditor(refocus);
                                }}
                                onCancel={closeEditor}
                              />
                            )}
                          </>
                        ) : (
                          <div className={meta?.wrap ? undefined : truncate} title={text}>
                            {content}
                          </div>
                        )}
                      </Tag>
                    );
                  })}
                  {rowActions && (
                    <td role="gridcell" {...cellProps(gr, colCount - 1, edit)}>
                      <RowActions rowName={name}>{rowActions(row.original)}</RowActions>
                    </td>
                  )}
                </tr>
                {(() => {
                  const detail = rowDetail?.(row.original);
                  return detail == null ? null : (
                    <tr>
                      <td role="gridcell" colSpan={colCount}>{detail}</td>
                    </tr>
                  );
                })()}
              </Fragment>
            );
          })}
        {padBottom > 0 && spacer(padBottom)}
        {loading && (
          <tr>
            <td role="gridcell" colSpan={colCount}>
              <div className="slds-is-relative slds-p-around_large">
                <Spinner size="small" />
              </div>
            </td>
          </tr>
        )}
      </tbody>
    </table>
  );

  const framed = editable ? <div className="slds-table_edit_container slds-is-relative">{grid}</div> : grid;
  return scrollHeight === undefined ? (
    framed
  ) : (
    <div ref={scrollRef} className="slds-scrollable" style={{ maxHeight: scrollHeight }}>
      {framed}
    </div>
  );
}
