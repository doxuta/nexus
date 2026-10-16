"use client";

// DataGrid — headless virtualized grid primitive (W9a, plan §4.2 "DataGrid" row).
// TanStack Table v8 (column model + resize state machine) + TanStack Virtual (row window).
// HEADLESS on purpose: every visual is a Nexus token class carried over VERBATIM from the
// hand-rolled LayoutList table (header bg-panel2 / uppercase 11px, row border-bd2 + hover
// bg-panel2, selection bg-acd) — deliberately NOT the AG-Grid look (no vertical cell
// rules, no dense gray chrome). Anti-slop dials: VARIANCE 4 · MOTION 2 · DENSITY 7.
//
//   • Virtualized rows: only the viewport window (+overscan) exists in the DOM, so a
//     10k-row list scrolls at the cost of ~20-30 row nodes. Dynamic measurement
//     (measureElement) keeps taller wrapped rows honest.
//   • Column resize: 8px hit-area on each header edge (TanStack resize handler),
//     min width per column, double-click resets. Widths persist per-user+grid in
//     localStorage (lib/dataGrid) — only user-touched columns are stored, untouched
//     columns keep following layout metadata.
//   • Sorting is NOT re-implemented: the grid renders indicators from the caller's
//     existing sort state and calls onSortToggle — LayoutList keeps syncing with the
//     backend exactly as before (D29 soft-revert included).
//   • Keyboard: ↑/↓ move the focused row, Enter opens, Space toggles selection,
//     Home/End jump. The scroll container is the single tab stop (roving focus ring).
//   • Density: rowHeight accepts the existing --row-compact/-cozy/-comfy tokens; the
//     px estimate for the virtualizer is READ from the computed var (token stays boss).
//   • Loading: skeleton rows that mirror the real columns (no spinner); empty state is
//     a passthrough node so callers keep their EmptyState wiring.

import {
  useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode,
} from "react";
import { clsx } from "clsx";
import {
  useReactTable,
  getCoreRowModel,
  flexRender,
  type ColumnDef,
  type ColumnSizingState,
  type Updater,
} from "@tanstack/react-table";
import { useVirtualizer } from "@tanstack/react-virtual";
import { ArrowUp, ArrowDown, ChevronsUpDown, CheckSquare, Square } from "lucide-react";
import { buildGridTemplate, resolveRowHeightPx, loadColWidths, saveColWidths } from "@/lib/dataGrid";

export interface DataGridSort { field: string; order: "asc" | "desc" }

export interface DataGridColumn<Row> {
  id: string;
  header: ReactNode;
  /** Declared width (px) from layout metadata; first column stays fluid until resized. */
  width?: number;
  minWidth?: number;
  sortable?: boolean;
  /** Right-align header + let the cell renderer right-align content (currency/number). */
  numeric?: boolean;
  cell: (row: Row) => ReactNode;
}

export interface DataGridProps<Row extends { id: string }> {
  columns: DataGridColumn<Row>[];
  rows: Row[];
  /** CSS length — pass the density token, e.g. `var(--row-compact)`. Default cozy. */
  rowHeight?: string;
  loading?: boolean;
  skeletonRows?: number;
  /** Rendered when !loading && rows.length === 0 (EmptyState passthrough). */
  empty?: ReactNode;
  /** Existing server-sort state — indicators only, no client sorting. */
  sort?: DataGridSort | null;
  onSortToggle?: (field: string) => void;
  selectable?: boolean;
  selected?: Set<string>;
  allSelected?: boolean;
  onToggleRow?: (id: string) => void;
  onToggleAll?: () => void;
  onRowOpen?: (id: string) => void;
  /** Enables per-user column-width persistence, e.g. `list:crm_accounts`. */
  storageKey?: string;
  /**
   * Optional per-row action, rendered in a FIXED trailing cell OUTSIDE the row-open button (so an
   * interactive control — e.g. the recycle-bin "Khôi phục" button — never nests inside a button).
   * Absent → no trailing column, live-list rendering is byte-identical. Put a real focusable
   * <button> here: the roving keyboard model does not cover it.
   */
  rowAction?: (row: Row) => ReactNode;
  rowActionHeader?: ReactNode;
  rowActionWidth?: number;
  className?: string;
  "aria-label"?: string;
}

const SELECT_COL_PX = 40;
const HEADER_PX = 40;
const ROW_ACTION_PX = 132;

export function DataGrid<Row extends { id: string }>({
  columns,
  rows,
  rowHeight = "var(--row-cozy)",
  loading = false,
  skeletonRows = 6,
  empty,
  sort,
  onSortToggle,
  selectable = false,
  selected,
  allSelected = false,
  onToggleRow,
  onToggleAll,
  onRowOpen,
  storageKey,
  rowAction,
  rowActionHeader,
  rowActionWidth = ROW_ACTION_PX,
  className,
  "aria-label": ariaLabel,
}: DataGridProps<Row>) {
  const scrollRef = useRef<HTMLDivElement>(null);

  // ── Column sizing — controlled state, persisted per-user+grid ────────────────
  const [columnSizing, setColumnSizing] = useState<ColumnSizingState>({});
  // Load AFTER mount (client effect) → SSR/hydration-safe, same discipline as
  // LayoutList's parseUrlFilters.
  useEffect(() => {
    if (!storageKey) return;
    setColumnSizing(loadColWidths(storageKey));
  }, [storageKey]);

  const saveTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const handleSizingChange = useCallback(
    (updater: Updater<ColumnSizingState>) => {
      setColumnSizing((prev) => {
        const next = typeof updater === "function" ? updater(prev) : updater;
        if (storageKey) {
          if (saveTimer.current) clearTimeout(saveTimer.current);
          saveTimer.current = setTimeout(() => saveColWidths(storageKey, next), 300);
        }
        return next;
      });
    },
    [storageKey],
  );
  useEffect(() => () => { if (saveTimer.current) clearTimeout(saveTimer.current); }, []);

  const resetColumn = useCallback(
    (colId: string) => {
      setColumnSizing((prev) => {
        const next = { ...prev };
        delete next[colId];
        if (storageKey) saveColWidths(storageKey, next);
        return next;
      });
    },
    [storageKey],
  );

  // ── TanStack Table (headless — column model + resize only, manual everything) ──
  const columnDefs = useMemo<ColumnDef<Row>[]>(
    () =>
      columns.map((c) => ({
        id: c.id,
        size: c.width ?? 140,
        minSize: c.minWidth ?? 64,
        maxSize: 800,
        enableResizing: true,
        header: () => c.header,
        cell: (ctx) => c.cell(ctx.row.original),
      })),
    [columns],
  );

  const table = useReactTable({
    data: rows,
    columns: columnDefs,
    getCoreRowModel: getCoreRowModel(),
    getRowId: (r) => r.id,
    columnResizeMode: "onChange",
    enableColumnResizing: true,
    state: { columnSizing },
    onColumnSizingChange: handleSizingChange,
  });

  const gridTemplate = useMemo(
    () =>
      buildGridTemplate(
        columns.map((c) => ({ id: c.id, width: c.width })),
        columnSizing,
        selectable ? SELECT_COL_PX : undefined,
        rowAction ? rowActionWidth : undefined,
      ),
    [columns, columnSizing, selectable, rowAction, rowActionWidth],
  );

  // ── Row virtualization ────────────────────────────────────────────────────────
  // Estimate row height by READING the computed density token (token stays boss).
  const [rowPx, setRowPx] = useState(48);
  useEffect(() => {
    const el = scrollRef.current;
    const readVar = (name: string) =>
      el ? getComputedStyle(el).getPropertyValue(name) : "";
    setRowPx(resolveRowHeightPx(rowHeight, readVar));
  }, [rowHeight]);

  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => rowPx,
    overscan: 10,
  });
  // Density change → drop cached measurements so row positions recompute.
  useEffect(() => { virtualizer.measure(); }, [rowPx, virtualizer]);

  const tableRows = table.getRowModel().rows;
  const virtualItems = virtualizer.getVirtualItems();

  // ── Keyboard row navigation (↑ ↓ Enter Space Home End) ───────────────────────
  const [focusIdx, setFocusIdx] = useState(-1);
  useEffect(() => {
    if (focusIdx >= rows.length) setFocusIdx(rows.length - 1);
  }, [rows.length, focusIdx]);

  const moveFocus = useCallback(
    (idx: number) => {
      const clamped = Math.max(0, Math.min(idx, rows.length - 1));
      setFocusIdx(clamped);
      virtualizer.scrollToIndex(clamped);
    },
    [rows.length, virtualizer],
  );

  const onKeyDown = useCallback(
    (e: KeyboardEvent<HTMLDivElement>) => {
      if (rows.length === 0) return;
      switch (e.key) {
        case "ArrowDown":
          e.preventDefault();
          moveFocus(focusIdx < 0 ? 0 : focusIdx + 1);
          break;
        case "ArrowUp":
          e.preventDefault();
          moveFocus(focusIdx < 0 ? 0 : focusIdx - 1);
          break;
        case "Home":
          e.preventDefault();
          moveFocus(0);
          break;
        case "End":
          e.preventDefault();
          moveFocus(rows.length - 1);
          break;
        case "Enter":
          if (focusIdx >= 0 && rows[focusIdx]) onRowOpen?.(rows[focusIdx].id);
          break;
        case " ":
          if (selectable && focusIdx >= 0 && rows[focusIdx]) {
            e.preventDefault(); // don't page-scroll
            onToggleRow?.(rows[focusIdx].id);
          }
          break;
        case "Escape":
          setFocusIdx(-1);
          break;
      }
    },
    [rows, focusIdx, moveFocus, onRowOpen, onToggleRow, selectable],
  );

  const headers = table.getFlatHeaders();
  const showHeaderSkeleton = columns.length === 0 && loading;
  const skeletonCols: { id: string }[] =
    columns.length > 0 ? columns : [{ id: "_a" }, { id: "_b" }];

  return (
    <div
      ref={scrollRef}
      role="grid"
      aria-label={ariaLabel}
      aria-rowcount={rows.length + 1}
      aria-busy={loading || undefined}
      tabIndex={0}
      onKeyDown={onKeyDown}
      className={clsx(
        "overflow-auto focus:outline-none focus-visible:ring-1 focus-visible:ring-inset focus-visible:ring-ac",
        className,
      )}
    >
      {/* Header row — sticky inside the scroll container (unchanged from LayoutList) */}
      <div
        role="row"
        aria-rowindex={1}
        className="sticky top-0 z-10 grid items-center border-b border-bd bg-panel2 px-0 text-caption font-semibold uppercase tracking-wide text-fnt"
        style={{ gridTemplateColumns: gridTemplate, height: HEADER_PX }}
      >
        {selectable && (
          <div role="columnheader" className="flex items-center justify-center">
            <button
              type="button"
              onClick={onToggleAll}
              aria-label={allSelected ? "Bỏ chọn tất cả" : "Chọn tất cả"}
              className="grid size-4 place-items-center"
            >
              {allSelected
                ? <CheckSquare className="size-4 text-ac" strokeWidth={2} />
                : <Square className="size-4 text-bd3" strokeWidth={2} />}
            </button>
          </div>
        )}
        {showHeaderSkeleton ? (
          <div className="h-3 w-24 animate-pulse rounded bg-bd2" />
        ) : (
          headers.map((header) => {
            const col = columns.find((c) => c.id === header.id);
            if (!col) return null;
            const isSorted = sort?.field === col.id;
            const isResizing = header.column.getIsResizing();
            return (
              <div key={header.id} role="columnheader" className="relative flex h-full min-w-0 items-center">
                <button
                  type="button"
                  onClick={() => onSortToggle?.(col.id)}
                  disabled={!col.sortable}
                  className={clsx(
                    "flex min-w-0 flex-1 items-center gap-1 truncate px-3 text-left",
                    col.numeric && "justify-end",
                    col.sortable ? "cursor-pointer hover:text-fg" : "cursor-default",
                    isSorted && "text-fg",
                  )}
                >
                  <span className="truncate">
                    {flexRender(header.column.columnDef.header, header.getContext())}
                  </span>
                  {col.sortable && (
                    isSorted ? (
                      sort?.order === "asc"
                        ? <ArrowUp className="size-3 shrink-0" strokeWidth={2} />
                        : <ArrowDown className="size-3 shrink-0" strokeWidth={2} />
                    ) : (
                      <ChevronsUpDown className="size-3 shrink-0 text-bd3" strokeWidth={2} />
                    )
                  )}
                </button>
                {/* Resize handle — 8px hit area, 1px visual line; double-click resets */}
                <div
                  role="separator"
                  aria-orientation="vertical"
                  aria-label="Kéo để đổi độ rộng cột, nhấp đúp để đặt lại"
                  title="Kéo để đổi độ rộng cột · nhấp đúp đặt lại"
                  onMouseDown={header.getResizeHandler()}
                  onTouchStart={header.getResizeHandler()}
                  onDoubleClick={() => resetColumn(col.id)}
                  className="group/rz absolute -right-1 top-0 z-10 h-full w-2 cursor-col-resize touch-none select-none"
                >
                  <span
                    className={clsx(
                      "absolute right-[3px] top-1/2 w-px -translate-y-1/2 transition-colors",
                      isResizing
                        ? "h-full bg-ac"
                        : "h-4 bg-bd3 opacity-0 group-hover/rz:opacity-100",
                    )}
                  />
                </div>
              </div>
            );
          })
        )}
        {rowAction && (
          <div role="columnheader" className="flex h-full items-center justify-end px-3">
            <span className="truncate">{rowActionHeader}</span>
          </div>
        )}
      </div>

      {/* Loading skeleton rows — mirror the real columns (no spinner) */}
      {loading &&
        Array.from({ length: skeletonRows }).map((_, i) => (
          <div
            key={i}
            className="grid items-center border-b border-bd2 last:border-0"
            style={{ gridTemplateColumns: gridTemplate, minHeight: rowHeight }}
          >
            {selectable && (
              <div className="flex justify-center">
                <div className="size-3 animate-pulse rounded-full bg-bd2" />
              </div>
            )}
            {skeletonCols.map((c) => (
              <div key={c.id} className="px-3">
                <span className="block h-3 w-24 animate-pulse rounded bg-bd2" />
              </div>
            ))}
            {rowAction && (
              <div className="px-3">
                <span className="ml-auto block h-6 w-20 animate-pulse rounded-input bg-bd2" />
              </div>
            )}
          </div>
        ))}

      {/* Virtualized body — only the viewport window exists in the DOM */}
      {!loading && rows.length > 0 && (
        <div style={{ height: virtualizer.getTotalSize(), position: "relative" }}>
          {virtualItems.map((vi) => {
            const row = tableRows[vi.index];
            if (!row) return null;
            const isSelected = selected?.has(row.id) ?? false;
            const isFocused = focusIdx === vi.index;
            return (
              <div
                key={row.id}
                role="row"
                aria-rowindex={vi.index + 2}
                aria-selected={selectable ? isSelected : undefined}
                data-index={vi.index}
                ref={virtualizer.measureElement}
                className={clsx(
                  "group grid w-full items-center text-left text-body transition-colors",
                  vi.index !== rows.length - 1 && "border-b border-bd2",
                  isSelected ? "bg-acd" : "hover:bg-panel2",
                  isFocused && "shadow-[inset_0_0_0_1px_var(--ac)]",
                )}
                style={{
                  gridTemplateColumns: gridTemplate,
                  minHeight: rowHeight,
                  position: "absolute",
                  top: 0,
                  left: 0,
                  width: "100%",
                  transform: `translateY(${vi.start}px)`,
                }}
              >
                {selectable && (
                  <div role="gridcell" className="flex items-center justify-center">
                    <button
                      type="button"
                      onClick={() => onToggleRow?.(row.id)}
                      aria-label={isSelected ? "Bỏ chọn dòng" : "Chọn dòng"}
                      className="grid size-4 place-items-center"
                    >
                      {isSelected
                        ? <CheckSquare className="size-4 text-ac" strokeWidth={2} />
                        : <Square className="size-4 text-bd3 group-hover:text-fnt" strokeWidth={2} />}
                    </button>
                  </div>
                )}
                {row.getVisibleCells().map((cell) => (
                  <button
                    key={cell.id}
                    role="gridcell"
                    type="button"
                    tabIndex={-1}
                    onClick={() => {
                      setFocusIdx(vi.index);
                      onRowOpen?.(row.id);
                    }}
                    className="min-w-0 px-3 text-left"
                  >
                    {flexRender(cell.column.columnDef.cell, cell.getContext())}
                  </button>
                ))}
                {rowAction && (
                  <div role="gridcell" className="flex min-w-0 items-center justify-end px-3">
                    {rowAction(row.original)}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}

      {/* Empty state passthrough */}
      {!loading && rows.length === 0 && empty}
    </div>
  );
}
