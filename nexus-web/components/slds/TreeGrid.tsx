"use client";

// SLDS tree grid (blueprint `tree-grid` base, SLDS 2.264.1) on @tanstack/react-table getSubRows +
// getExpandedRowModel. Markup is the blueprint's:
//   table.slds-table.slds-table_bordered.slds-table_edit.slds-table_fixed-layout.slds-tree.slds-table_tree[role=treegrid][aria-readonly]
//     tbody > tr.slds-hint-parent[aria-level][aria-posinset][aria-setsize][aria-expanded?][aria-selected]
//       > td (select) · th.slds-tree__item[scope=row] (chevron button + .slds-truncate) · td[role=gridcell][data-label]…
// SLDS indents by aria-level and rotates the chevronright under [aria-expanded=true]; leaves keep the
// chevron slot with `slds-is-disabled` (visibility hidden). The chevron is aria-hidden: keyboard users
// expand with the arrow keys (tree-grid docs): focus starts on the row; Right expands a collapsed row,
// otherwise enters its cells; Left in the first cell returns to the row, on an expanded row collapses
// it, on a collapsed/leaf row climbs to the parent. Up/Down move rows (Up from the first row, or
// Ctrl+Home, reaches the header cells: sort, select-all, column width), Space selects, Enter/Esc toggle
// actionable mode like DataTable. Header, selection and row action cells are DataTable's.
import {
  flexRender,
  getCoreRowModel,
  getExpandedRowModel,
  getSortedRowModel,
  useReactTable,
  type ColumnDef,
  type ColumnSizingState,
  type ExpandedState,
  type Row,
  type RowSelectionState,
  type SortingState,
} from "@tanstack/react-table";
import clsx from "clsx";
import { ChevronRight } from "lucide-react";
import type { ReactNode } from "react";
import { t } from "@/lib/i18n";
import { Spinner } from "./Spinner";
import {
  ActionsHeader,
  ColumnHeader,
  RowActions,
  SelectBox,
  cellText,
  fill,
  headerText,
  useControlled,
  useGridKeyboard,
  type GridPos,
} from "./DataTable";

export interface TreeGridProps<TData> {
  columns: ColumnDef<TData, any>[]; // eslint-disable-line @typescript-eslint/no-explicit-any
  data: TData[];
  /** Accessible name of the tree grid (aria-label). */
  label: string;
  /** Child rows of a row (undefined / empty = leaf). */
  getSubRows: (row: TData, index: number) => TData[] | undefined;
  /** Stable row id — keys selection and expansion. Default: `row.id` when present, else the path. */
  getRowId?: (row: TData, index: number, parent?: Row<TData>) => string;

  /** Expanded rows keyed by row id (`true` = all). */
  expanded?: ExpandedState;
  defaultExpanded?: ExpandedState;
  onExpandedChange?: (expanded: ExpandedState) => void;

  sortable?: boolean;
  sorting?: SortingState;
  defaultSorting?: SortingState;
  onSortingChange?: (sorting: SortingState) => void;

  /** Checkbox column; selecting a parent does not select its children (blueprint "selected row"). */
  selectable?: boolean;
  rowSelection?: RowSelectionState;
  defaultRowSelection?: RowSelectionState;
  onRowSelectionChange?: (selection: RowSelectionState) => void;

  resizable?: boolean;
  columnSizing?: ColumnSizingState;
  defaultColumnSizing?: ColumnSizingState;
  onColumnSizingChange?: (sizing: ColumnSizingState) => void;

  rowActions?: (row: TData) => ReactNode;
  loading?: boolean;
  empty?: ReactNode;
  className?: string;
}

export function TreeGrid<TData>({
  columns,
  data,
  label,
  getSubRows,
  getRowId,
  expanded: expandedProp,
  defaultExpanded = {},
  onExpandedChange,
  sortable = false,
  sorting: sortingProp,
  defaultSorting = [],
  onSortingChange,
  selectable = false,
  rowSelection: selectionProp,
  defaultRowSelection = {},
  onRowSelectionChange,
  resizable = false,
  columnSizing: sizingProp,
  defaultColumnSizing = {},
  onColumnSizingChange,
  rowActions,
  loading = false,
  empty,
  className,
}: TreeGridProps<TData>) {
  const [expanded, setExpanded] = useControlled(expandedProp, onExpandedChange, defaultExpanded);
  const [sorting, setSorting] = useControlled(sortingProp, onSortingChange, defaultSorting);
  const [rowSelection, setRowSelection] = useControlled(selectionProp, onRowSelectionChange, defaultRowSelection);
  const [columnSizing, setColumnSizing] = useControlled(sizingProp, onColumnSizingChange, defaultColumnSizing);

  // TanStack Table returns fresh functions each render; the React Compiler skips memoising this
  // component on its own, which is what we want — the lint warning only reports that.
  // eslint-disable-next-line react-hooks/incompatible-library
  const table = useReactTable({
    data,
    columns,
    getSubRows,
    getRowId:
      getRowId ?? ((row, i, parent) => String((row as { id?: unknown }).id ?? (parent ? `${parent.id}.${i}` : i))),
    state: { expanded, sorting, rowSelection, columnSizing },
    onExpandedChange: setExpanded,
    onSortingChange: setSorting,
    onRowSelectionChange: setRowSelection,
    onColumnSizingChange: setColumnSizing,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getExpandedRowModel: getExpandedRowModel(),
    enableSorting: sortable,
    enableSortingRemoval: false,
    enableMultiSort: false,
    sortDescFirst: false,
    enableRowSelection: selectable,
    enableSubRowSelection: false,
    enableColumnResizing: resizable,
    defaultColumn: { size: undefined }, // see DataTable
  });

  const leafColumns = table.getVisibleLeafColumns();
  const lead = selectable ? 1 : 0;
  const colCount = lead + leafColumns.length + (rowActions ? 1 : 0);
  const rows = table.getRowModel().rows;
  const treeColumnId = leafColumns[0]?.id;

  // aria-posinset / aria-setsize follow the displayed (sorted) sibling order.
  const place = new Map<string, { pos: number; size: number }>();
  const walk = (siblings: Row<TData>[]) =>
    siblings.forEach((r, i) => {
      place.set(r.id, { pos: i + 1, size: siblings.length });
      walk(r.subRows);
    });
  walk(table.getPreExpandedRowModel().rows);

  const rowName = (row: Row<TData>) => cellText(treeColumnId ? row.getValue(treeColumnId) : undefined) ?? row.id;

  const { gridProps, cellProps, rowTabIndex } = useGridKeyboard({
    rowCount: rows.length,
    colCount,
    initial: { r: 1, c: -1 },
    rowFocus: true,
    onKey: (e, pos, mode): GridPos | boolean | undefined => {
      if (mode !== "nav" || pos.c !== -1) return undefined;
      const row = rows[pos.r - 1];
      if (!row) return undefined;
      if (e.key === "ArrowRight" && row.getCanExpand() && !row.getIsExpanded()) {
        row.toggleExpanded(true);
        return true;
      }
      if (e.key === "ArrowLeft") {
        if (row.getIsExpanded()) {
          row.toggleExpanded(false);
          return true;
        }
        const parent = rows.findIndex((r) => r.id === row.parentId);
        return parent >= 0 ? { r: parent + 1, c: -1 } : true;
      }
      return undefined;
    },
  });

  return (
    <table
      {...gridProps}
      role="treegrid"
      aria-label={label}
      aria-readonly="true"
      aria-multiselectable={selectable ? "true" : undefined}
      aria-rowcount={rows.length + 1}
      aria-colcount={colCount}
      className={clsx(
        "slds-table slds-table_bordered slds-table_edit slds-table_fixed-layout slds-table_resizable-cols slds-tree slds-table_tree",
        className,
      )}
    >
      <thead>
        <tr className="slds-line-height_reset" aria-rowindex={1}>
          {selectable && (
            <th role="cell" {...cellProps(0, 0, "slds-text-align_right")} style={{ width: "3.25rem" }}>
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
            <ColumnHeader key={header.id} header={header} cell={cellProps(0, lead + i)} />
          ))}
          {rowActions && <ActionsHeader cell={cellProps(0, colCount - 1)} />}
        </tr>
      </thead>
      <tbody>
        {rows.length === 0 && !loading && (
          <tr>
            <td role="gridcell" colSpan={colCount}>
              {empty ?? <div className="slds-p-around_medium slds-text-align_center slds-text-color_weak">{t("slds.dataTable.empty")}</div>}
            </td>
          </tr>
        )}
        {rows.map((row, r) => {
          const gr = r + 1;
          const name = rowName(row);
          const canExpand = row.getCanExpand();
          const isExpanded = row.getIsExpanded();
          const { pos, size } = place.get(row.id) ?? { pos: 1, size: 1 };
          const toggleLabel = fill(t(isExpanded ? "slds.treeGrid.collapse" : "slds.treeGrid.expand"), { row: name });
          return (
            <tr
              key={row.id}
              aria-rowindex={gr + 1}
              aria-level={row.depth + 1}
              aria-posinset={pos}
              aria-setsize={size}
              aria-expanded={canExpand ? isExpanded : undefined}
              aria-selected={selectable ? row.getIsSelected() : undefined}
              tabIndex={rowTabIndex(gr)}
              className={clsx("slds-hint-parent", row.getIsSelected() && "slds-is-selected")}
            >
              {selectable && (
                <td role="gridcell" {...cellProps(gr, 0, "slds-text-align_right")} style={{ width: "3.25rem" }}>
                  <SelectBox
                    label={fill(t("slds.dataTable.selectItem"), { n: gr })}
                    checked={row.getIsSelected()}
                    disabled={!row.getCanSelect()}
                    onChange={(v) => row.toggleSelected(v)}
                  />
                </td>
              )}
              {row.getVisibleCells().map((cell, c) => {
                const { column } = cell;
                const numeric = column.columnDef.meta?.numeric;
                const content = (
                  <div className={clsx("slds-truncate", numeric && "tabular-nums")} title={cellText(cell.getValue())}>
                    {flexRender(column.columnDef.cell, cell.getContext())}
                  </div>
                );
                if (column.id === treeColumnId)
                  return (
                    <th key={cell.id} scope="row" data-label={headerText(column)} {...cellProps(gr, lead + c, "slds-tree__item")}>
                      <button
                        type="button"
                        className={clsx("slds-button slds-button_icon slds-button_icon-x-small slds-m-right_x-small", !canExpand && "slds-is-disabled")}
                        aria-hidden="true"
                        tabIndex={-1}
                        title={toggleLabel}
                        onClick={() => canExpand && row.toggleExpanded()}
                      >
                        <ChevronRight aria-hidden="true" className="slds-button__icon slds-button__icon_small nx-lucide" />
                        <span className="slds-assistive-text">{toggleLabel}</span>
                      </button>
                      {content}
                    </th>
                  );
                return (
                  <td
                    key={cell.id}
                    role="gridcell"
                    data-label={headerText(column)}
                    {...cellProps(gr, lead + c, clsx(numeric && "slds-text-align_right"))}
                  >
                    {content}
                  </td>
                );
              })}
              {rowActions && (
                <td role="gridcell" {...cellProps(gr, colCount - 1)} style={{ width: "3.25rem" }}>
                  <RowActions rowName={name}>{rowActions(row.original)}</RowActions>
                </td>
              )}
            </tr>
          );
        })}
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
}
