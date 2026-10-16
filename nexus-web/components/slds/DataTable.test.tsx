// DataTable — blueprint data-tables/advanced + inline-edit (SLDS 2.264.1) on @tanstack/react-table.
import { describe, it, expect, vi } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ColumnDef, SortingState } from "@tanstack/react-table";
import { useState } from "react";
import { DataTable } from "./DataTable";
import { MenuItem } from "./Menu";
import { Popover } from "./Popover";

interface Opp {
  id: string;
  name: string;
  account: string;
  amount: number;
}

const DATA: Opp[] = [
  { id: "o1", name: "Minh Phát - 1.200 bộ", account: "Minh Phát", amount: 25000000 },
  { id: "o2", name: "An Khang - 200 bộ", account: "An Khang", amount: 5000000 },
  { id: "o3", name: "Bảo Tín - 1.000 bộ", account: "Bảo Tín", amount: 25000 },
];

const COLUMNS: ColumnDef<Opp>[] = [
  { accessorKey: "name", header: "Tên" },
  { accessorKey: "account", header: "Khách hàng" },
  { accessorKey: "amount", header: "Giá trị", meta: { numeric: true } },
];

const grid = () => screen.getByRole("grid");
const bodyRows = () => within(grid()).getAllByRole("row").slice(1);

describe("DataTable — advanced markup", () => {
  it("renders the blueprint table, header and cell roles", () => {
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={DATA} />);
    const table = grid();
    expect(table.tagName).toBe("TABLE");
    // slds-table_resizable-cols even without resize: it zeroes the thead th padding (blueprint).
    expect(table).toHaveClass("slds-table", "slds-table_bordered", "slds-table_fixed-layout", "slds-table_resizable-cols");
    expect(table).toHaveAttribute("aria-label", "Cơ hội");
    expect(table).toHaveAttribute("aria-rowcount", "4");
    expect(table.querySelector("thead tr")).toHaveClass("slds-line-height_reset");
    const headers = table.querySelectorAll("thead th");
    expect(headers).toHaveLength(3);
    for (const th of headers) expect(th).toHaveAttribute("scope", "col");
    // First data column is the row header; the rest are gridcells.
    const first = bodyRows()[0];
    expect(first).toHaveClass("slds-hint-parent");
    expect(first).toHaveAttribute("aria-rowindex", "2");
    const cells = first.children;
    expect(cells[0].tagName).toBe("TH");
    expect(cells[0]).toHaveAttribute("scope", "row");
    expect(cells[1]).toHaveAttribute("role", "gridcell");
    expect(cells[1]).toHaveAttribute("aria-colindex", "2");
    // Cell content is truncated with a title (Vietnamese runs long).
    const truncate = cells[1].querySelector(".slds-truncate");
    expect(truncate).toHaveAttribute("title", "Minh Phát");
    expect(truncate).toHaveTextContent("Minh Phát");
  });

  it("right-aligns numeric cells with tabular figures", () => {
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={DATA} />);
    const amount = bodyRows()[0].children[2];
    expect(amount).toHaveClass("slds-text-align_right");
    expect(amount.querySelector(".slds-truncate")).toHaveClass("tabular-nums");
    expect(grid().querySelectorAll("thead th")[2]).toHaveClass("slds-text-align_right");
  });

  it("wraps a long-text column in full per the blueprint 'Cell content wrapped' (no truncate, no clamp)", () => {
    const cols: ColumnDef<Opp>[] = [COLUMNS[0], { accessorKey: "account", header: "Khách hàng", meta: { wrap: true } }];
    render(<DataTable label="Cơ hội" columns={cols} data={DATA} />);
    const cell = bodyRows()[0].children[1];
    expect(cell).toHaveClass("slds-cell-wrap");
    const text = cell.querySelector("div[title]");
    expect(text).toHaveAttribute("title", "Minh Phát");
    expect(text).toHaveTextContent("Minh Phát");
    expect(cell.querySelector(".slds-truncate, .slds-line-clamp")).toBeNull();
    // Other columns keep the truncated line.
    expect(bodyRows()[0].children[0]).not.toHaveClass("slds-cell-wrap");
  });

  it("renders a sortable header per the blueprint: link button, assistive 'Sắp xếp theo', aria-sort none", () => {
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={DATA} sortable />);
    const th = grid().querySelectorAll("thead th")[0];
    expect(th).toHaveClass("slds-is-sortable");
    expect(th).toHaveAttribute("aria-sort", "none");
    const action = th.querySelector("a.slds-th__action.slds-text-link_reset")!;
    expect(action).toHaveAttribute("role", "button");
    expect(action.querySelector(".slds-assistive-text")).toHaveTextContent("Sắp xếp theo:");
    expect(action.querySelector(".slds-truncate")).toHaveAttribute("title", "Tên");
    expect(action.querySelector("svg")).toHaveClass("slds-icon", "slds-icon-text-default", "slds-is-sortable__icon");
    expect(th.querySelector("[aria-live]")).toBeNull();
  });

  it("sorts on click (asc then desc) and announces it", async () => {
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={DATA} sortable />);
    const th = () => grid().querySelectorAll("thead th")[1];
    await userEvent.click(th().querySelector("a")!);
    expect(th()).toHaveAttribute("aria-sort", "ascending");
    expect(th()).toHaveClass("slds-is-sorted", "slds-is-sorted_asc");
    expect(th().querySelector("[aria-live='polite']")).toHaveTextContent("Đã sắp xếp tăng dần");
    expect(bodyRows().map((r) => r.children[1].textContent)).toEqual(["An Khang", "Bảo Tín", "Minh Phát"]);
    await userEvent.click(th().querySelector("a")!);
    expect(th()).toHaveAttribute("aria-sort", "descending");
    expect(th()).toHaveClass("slds-is-sorted_desc");
    expect(bodyRows().map((r) => r.children[1].textContent)).toEqual(["Minh Phát", "Bảo Tín", "An Khang"]);
  });

  it("controlled + manual sorting: reports the new sort and keeps the server's row order", async () => {
    const onSortingChange = vi.fn();
    const sorting: SortingState = [{ id: "amount", desc: false }];
    render(
      <DataTable label="Cơ hội" columns={COLUMNS} data={DATA} sortable manualSorting sorting={sorting} onSortingChange={onSortingChange} />,
    );
    expect(grid().querySelectorAll("thead th")[2]).toHaveAttribute("aria-sort", "ascending");
    // manual: rows are rendered in the given order
    expect(bodyRows()[0].children[0]).toHaveTextContent("Minh Phát - 1.200 bộ");
    await userEvent.click(grid().querySelectorAll("thead th")[2].querySelector("a")!);
    expect(onSortingChange).toHaveBeenCalledWith([{ id: "amount", desc: true }]);
  });

  it("a column with enableSorting:false renders a plain header", () => {
    const cols: ColumnDef<Opp>[] = [...COLUMNS.slice(0, 2), { accessorKey: "amount", header: "Giá trị", enableSorting: false }];
    render(<DataTable label="Cơ hội" columns={cols} data={DATA} sortable />);
    const th = grid().querySelectorAll("thead th")[2];
    expect(th).not.toHaveAttribute("aria-sort");
    expect(th.querySelector("a")).toBeNull();
    expect(th.querySelector(".slds-th__action .slds-truncate")).toHaveAttribute("title", "Giá trị");
  });
});

describe("DataTable — row selection", () => {
  it("renders the select-all header and per-row checkboxes per the blueprint", () => {
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={DATA} selectable />);
    const table = grid();
    expect(table).toHaveAttribute("aria-multiselectable", "true");
    const th = table.querySelector("thead th")!;
    expect(th).toHaveAttribute("role", "cell");
    expect(th).toHaveClass("slds-text-align_right");
    const all = within(th as HTMLElement).getByRole("checkbox", { name: "Chọn tất cả" });
    expect(all.closest(".slds-th__action")).toHaveClass("slds-th__action_form");
    expect(all.nextElementSibling).toHaveAttribute("aria-hidden", "true");
    const row = bodyRows()[0];
    expect(row).toHaveAttribute("aria-selected", "false");
    expect(row.children[0]).toHaveAttribute("role", "gridcell");
    expect(within(row).getByRole("checkbox", { name: "Chọn mục 1" })).not.toBeChecked();
    // Selection column shifts the row header to the 2nd cell.
    expect(row.children[1].tagName).toBe("TH");
  });

  it("selecting rows marks them, and select-all goes indeterminate then all", async () => {
    const onRowSelectionChange = vi.fn();
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={DATA} selectable onRowSelectionChange={onRowSelectionChange} />);
    await userEvent.click(screen.getByRole("checkbox", { name: "Chọn mục 2" }));
    expect(onRowSelectionChange).toHaveBeenLastCalledWith({ o2: true });
    const row = bodyRows()[1];
    expect(row).toHaveAttribute("aria-selected", "true");
    expect(row).toHaveClass("slds-is-selected");
    const all = screen.getByRole("checkbox", { name: "Chọn tất cả" }) as HTMLInputElement;
    expect(all.indeterminate).toBe(true);
    await userEvent.click(all);
    expect(onRowSelectionChange).toHaveBeenLastCalledWith({ o1: true, o2: true, o3: true });
    expect(all).toBeChecked();
    expect(all.indeterminate).toBe(false);
  });

  it("honours controlled rowSelection", () => {
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={DATA} selectable rowSelection={{ o3: true }} />);
    expect(screen.getByRole("checkbox", { name: "Chọn mục 3" })).toBeChecked();
    expect(bodyRows()[2]).toHaveClass("slds-is-selected");
  });
});

describe("DataTable — row actions, loading, empty", () => {
  it("renders the row action menu trigger per the blueprint", async () => {
    const onSelect = vi.fn();
    render(
      <DataTable
        label="Cơ hội"
        columns={COLUMNS}
        data={DATA}
        rowActions={(row) => <MenuItem label="Sửa" onSelect={() => onSelect(row.id)} />}
      />,
    );
    const th = grid().querySelectorAll("thead th")[3];
    expect(th.getAttribute("style")).toContain("3.25rem");
    expect(th.querySelector(".slds-assistive-text")).toHaveTextContent("Thao tác");
    const btn = screen.getByRole("button", { name: "Thêm thao tác cho Minh Phát - 1.200 bộ" });
    expect(btn).toHaveClass("slds-button", "slds-button_icon", "slds-button_icon-border-filled", "slds-button_icon-x-small");
    expect(btn.querySelector("svg")).toHaveClass("slds-button__icon", "slds-button__icon_hint", "slds-button__icon_small");
    expect(btn.closest("td")).toHaveAttribute("role", "gridcell");
    btn.focus();
    await userEvent.keyboard("{Enter}");
    await userEvent.click(screen.getByRole("menuitem", { name: "Sửa" }));
    expect(onSelect).toHaveBeenCalledWith("o1");
  });

  it("shows a spinner row while loading and the empty slot when there is no data", () => {
    const { rerender } = render(<DataTable label="Cơ hội" columns={COLUMNS} data={[]} loading />);
    expect(within(grid()).getByRole("status")).toHaveClass("slds-spinner");
    rerender(<DataTable label="Cơ hội" columns={COLUMNS} data={[]} />);
    expect(grid()).toHaveTextContent("Không có dữ liệu để hiển thị");
    rerender(<DataTable label="Cơ hội" columns={COLUMNS} data={[]} empty={<p>Chưa có cơ hội nào</p>} />);
    const cell = grid().querySelector("tbody td")!;
    expect(cell).toHaveAttribute("colspan", "3");
    expect(cell).toHaveTextContent("Chưa có cơ hội nào");
  });
});

// Keeps the harness honest: a controlled parent that owns sort state round-trips through the table.
describe("DataTable — controlled sort round trip", () => {
  it("parent state drives aria-sort", async () => {
    function Host() {
      const [sorting, setSorting] = useState<SortingState>([]);
      return <DataTable label="Cơ hội" columns={COLUMNS} data={DATA} sortable sorting={sorting} onSortingChange={setSorting} />;
    }
    render(<Host />);
    fireEvent.click(grid().querySelectorAll("thead th")[0].querySelector("a")!);
    expect(grid().querySelectorAll("thead th")[0]).toHaveAttribute("aria-sort", "ascending");
  });
});

const cellAt = (r: number, c: number) =>
  grid().querySelector<HTMLElement>(`tr[aria-rowindex="${r + 1}"] > [aria-colindex="${c + 1}"]`)!;
const LINKED: ColumnDef<Opp>[] = [
  { accessorKey: "name", header: "Tên", cell: (c) => <a href="#">{c.getValue<string>()}</a> },
  ...COLUMNS.slice(1),
];

describe("DataTable — grid keyboard (navigation / actionable mode)", () => {
  it("has a single tab stop on the first data cell; inner controls are out of the tab order", () => {
    render(<DataTable label="Cơ hội" columns={LINKED} data={DATA} selectable sortable />);
    const stops = Array.from(grid().querySelectorAll("[tabindex='0']"));
    expect(stops).toEqual([cellAt(1, 1)]);
    expect(cellAt(1, 1).tagName).toBe("TH");
    for (const el of grid().querySelectorAll("a, button, input")) expect(el).toHaveAttribute("tabindex", "-1");
  });

  it("arrow keys move the focused cell, into the header row too, and clamp at the edges", async () => {
    render(<DataTable label="Cơ hội" columns={LINKED} data={DATA} selectable sortable />);
    cellAt(1, 1).focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(cellAt(1, 2)).toHaveFocus();
    expect(cellAt(1, 2)).toHaveClass("slds-has-focus");
    expect(cellAt(1, 2)).toHaveAttribute("tabindex", "0");
    expect(cellAt(1, 1)).toHaveAttribute("tabindex", "-1");
    await userEvent.keyboard("{ArrowDown}{ArrowDown}{ArrowDown}");
    expect(cellAt(3, 2)).toHaveFocus();
    await userEvent.keyboard("{ArrowUp}{ArrowUp}{ArrowUp}{ArrowUp}");
    expect(cellAt(0, 2)).toHaveFocus();
    expect(cellAt(0, 2).tagName).toBe("TH");
    await userEvent.keyboard("{Home}");
    expect(cellAt(0, 0)).toHaveFocus();
    await userEvent.keyboard("{ArrowLeft}");
    expect(cellAt(0, 0)).toHaveFocus();
    await userEvent.keyboard("{End}");
    expect(cellAt(0, 3)).toHaveFocus();
  });

  it("Enter enters actionable mode (inner controls tabbable, first one focused); Esc returns to the cell", async () => {
    render(<DataTable label="Cơ hội" columns={LINKED} data={DATA} selectable sortable />);
    cellAt(1, 1).focus();
    await userEvent.keyboard("{Enter}");
    const link = within(cellAt(1, 1)).getByRole("link");
    expect(link).toHaveFocus();
    expect(link).toHaveAttribute("tabindex", "0");
    expect(cellAt(2, 2)).toHaveClass("slds-cell_action-mode");
    expect(within(cellAt(0, 1)).getByRole("button")).toHaveAttribute("tabindex", "0");
    // Arrows in actionable mode land on the next cell's first control.
    await userEvent.keyboard("{ArrowLeft}");
    expect(within(cellAt(1, 0)).getByRole("checkbox")).toHaveFocus();
    await userEvent.keyboard("{Escape}");
    expect(cellAt(1, 0)).toHaveFocus();
    expect(cellAt(1, 0)).not.toHaveClass("slds-cell_action-mode");
    expect(link).toHaveAttribute("tabindex", "-1");
  });

  it("Space on a selection cell toggles the row; Enter on a sort control sorts", async () => {
    const onRowSelectionChange = vi.fn();
    render(<DataTable label="Cơ hội" columns={LINKED} data={DATA} selectable sortable onRowSelectionChange={onRowSelectionChange} />);
    cellAt(1, 1).focus();
    await userEvent.keyboard("{ArrowLeft} ");
    expect(onRowSelectionChange).toHaveBeenLastCalledWith({ o1: true });
    await userEvent.keyboard("{ArrowUp}{ArrowRight}{ArrowRight}{Enter}");
    expect(within(cellAt(0, 2)).getByRole("button")).toHaveFocus();
    await userEvent.keyboard("{Enter}");
    expect(cellAt(0, 2)).toHaveAttribute("aria-sort", "ascending");
    await userEvent.keyboard(" ");
    expect(cellAt(0, 2)).toHaveAttribute("aria-sort", "descending");
  });

  it("keys inside an open row menu stay in the menu; the second Esc leaves actionable mode", async () => {
    render(
      <DataTable
        label="Cơ hội"
        columns={LINKED}
        data={DATA}
        rowActions={() => (
          <>
            <MenuItem label="Sửa" />
            <MenuItem label="Xoá" />
          </>
        )}
      />,
    );
    cellAt(1, 0).focus();
    await userEvent.keyboard("{End}{Enter}");
    const trigger = within(cellAt(1, 3)).getByRole("button");
    expect(trigger).toHaveFocus();
    await userEvent.keyboard("{Enter}");
    const items = screen.getAllByRole("menuitem");
    expect(items[0]).toHaveFocus();
    await userEvent.keyboard("{ArrowDown}");
    expect(items[1]).toHaveFocus();
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("menu")).toBeNull();
    expect(trigger).toHaveFocus();
    expect(cellAt(1, 3)).toHaveClass("slds-cell_action-mode");
    await userEvent.keyboard("{Escape}");
    expect(cellAt(1, 3)).toHaveFocus();
    expect(cellAt(1, 3)).not.toHaveClass("slds-cell_action-mode");
  });

  it("Esc leaves actionable mode from the column width range too", async () => {
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={DATA} sortable resizable />);
    act(() => cellAt(0, 0).focus());
    await userEvent.keyboard("{Enter}");
    expect(within(cellAt(0, 0)).getByRole("button")).toHaveFocus();
    await userEvent.tab();
    const range = within(cellAt(0, 0)).getByRole("slider");
    expect(range).toHaveFocus();
    await userEvent.keyboard("{Escape}");
    expect(cellAt(0, 0)).toHaveFocus();
    expect(cellAt(0, 0)).not.toHaveClass("slds-cell_action-mode");
  });

  it("keys inside a popover in a cell stay there; Esc closes it, the second Esc leaves actionable mode", async () => {
    const cols: ColumnDef<Opp>[] = [
      ...COLUMNS,
      {
        id: "more",
        header: "Chi tiết",
        cell: () => (
          <Popover trigger={<button type="button">Xem</button>} title="Chi tiết cơ hội">
            <button type="button">Gọi</button>
          </Popover>
        ),
      },
    ];
    render(<DataTable label="Cơ hội" columns={cols} data={DATA} />);
    act(() => cellAt(1, 3).focus());
    await userEvent.keyboard("{Enter}");
    const trigger = within(cellAt(1, 3)).getByRole("button", { name: "Xem" });
    expect(trigger).toHaveFocus();
    await userEvent.keyboard("{Enter}");
    const inside = within(screen.getByRole("dialog", { name: "Chi tiết cơ hội" })).getByRole("button", { name: "Gọi" });
    expect(inside).toHaveFocus();
    await userEvent.keyboard("{ArrowRight}{ArrowLeft}");
    expect(inside).toHaveFocus();
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(trigger).toHaveFocus();
    expect(cellAt(1, 3)).toHaveClass("slds-cell_action-mode");
    await userEvent.keyboard("{Escape}");
    expect(cellAt(1, 3)).toHaveFocus();
    expect(cellAt(1, 3)).not.toHaveClass("slds-cell_action-mode");
  });

  it("keeps its keyboard inside a dialog (a DataTable in a modal)", async () => {
    render(
      <div role="dialog" aria-label="Chọn cơ hội">
        <DataTable label="Cơ hội" columns={LINKED} data={DATA} />
      </div>,
    );
    act(() => cellAt(1, 0).focus());
    expect(within(cellAt(1, 0)).getByRole("link")).toHaveAttribute("tabindex", "-1");
    await userEvent.keyboard("{ArrowRight}{ArrowDown}");
    expect(cellAt(2, 1)).toHaveFocus();
  });

  it("tabbing out of the grid drops back to navigation mode and keeps the last cell", async () => {
    render(
      <>
        <DataTable label="Cơ hội" columns={LINKED} data={DATA} />
        <button type="button">Sau bảng</button>
      </>,
    );
    act(() => cellAt(2, 0).focus()); // a click would do this; flush the state it sets
    await userEvent.keyboard("{Enter}");
    expect(within(cellAt(2, 0)).getByRole("link")).toHaveFocus();
    await userEvent.tab(); // actionable mode: the next control in the grid (row 3's link)
    expect(within(cellAt(3, 0)).getByRole("link")).toHaveFocus();
    await userEvent.tab();
    expect(screen.getByRole("button", { name: "Sau bảng" })).toHaveFocus();
    expect(cellAt(3, 0)).toHaveAttribute("tabindex", "0");
    expect(within(cellAt(3, 0)).getByRole("link")).toHaveAttribute("tabindex", "-1");
    await userEvent.tab({ shift: true });
    expect(cellAt(3, 0)).toHaveFocus();
  });
});

describe("DataTable — inline edit (data-tables/inline-edit)", () => {
  const EDIT_COLS: ColumnDef<Opp>[] = [
    { accessorKey: "name", header: "Tên", meta: { editable: true, required: true } },
    { accessorKey: "account", header: "Khách hàng", meta: { editable: true } },
    { accessorKey: "amount", header: "Giá trị", meta: { numeric: true } },
  ];
  const pencil = (col: string, row: string) => screen.getByRole("button", { name: `Sửa ${col} của ${row}` });

  it("renders the inline-edit blueprint: container, errors column, edit and lock buttons", () => {
    render(<DataTable label="Cơ hội" columns={EDIT_COLS} data={DATA} editable selectable />);
    const table = grid();
    expect(table.parentElement).toHaveClass("slds-table_edit_container", "slds-is-relative");
    expect(table).toHaveClass("slds-table", "slds-table_bordered", "slds-table_edit", "slds-table_fixed-layout");
    const heads = table.querySelectorAll("thead th");
    expect(heads[0]).toHaveAttribute("scope", "col");
    expect(heads[0].getAttribute("style")).toContain("3.75rem");
    expect(heads[0].querySelector(".slds-assistive-text")).toHaveTextContent("Lỗi");
    expect(heads[1].getAttribute("style")).toContain("2rem");
    const row = bodyRows()[0];
    expect(row.children[0]).toHaveClass("slds-cell-edit", "slds-cell-error");
    expect(row.children[0].querySelector(".slds-row-number")).toHaveClass("slds-text-body_small", "slds-text-color_weak");
    const errBtn = row.children[0].querySelector("button")!;
    expect(errBtn).toHaveClass("slds-button_icon-error", "slds-hidden");
    expect(errBtn).toHaveAttribute("aria-hidden", "true");
    expect(row.children[1]).toHaveClass("slds-cell-edit");
    expect(row.children[2].tagName).toBe("TH");
    expect(row.children[2]).toHaveClass("slds-cell-edit");
    const edit = pencil("Khách hàng", "Minh Phát - 1.200 bộ");
    expect(edit).toHaveClass("slds-button", "slds-button_icon", "slds-cell-edit__button", "slds-m-left_x-small");
    expect(edit.querySelector("svg")).toHaveClass("slds-button__icon", "slds-button__icon_hint", "slds-button__icon_edit");
    expect(edit.parentElement).toHaveClass("slds-grid", "slds-grid_align-spread");
    expect(edit.previousElementSibling).toHaveClass("slds-truncate");
    const locked = row.children[4];
    expect(locked).toHaveAttribute("aria-readonly", "true");
    const lock = within(locked as HTMLElement).getByRole("button");
    expect(lock).toBeDisabled();
    expect(lock.querySelector("svg")).toHaveClass("slds-button__icon_lock", "slds-button__icon_small");
  });

  it("the pencil opens the edit popover; Esc cancels and returns focus to the pencil", async () => {
    render(<DataTable label="Cơ hội" columns={EDIT_COLS} data={DATA} editable />);
    await userEvent.click(pencil("Khách hàng", "Minh Phát - 1.200 bộ"));
    const dialog = screen.getByRole("dialog");
    expect(dialog.tagName).toBe("SECTION");
    expect(dialog).toHaveClass("slds-popover", "slds-popover_edit");
    expect(dialog.closest("td")).toBe(bodyRows()[0].children[2]);
    const input = within(dialog).getByRole("textbox", { name: "Khách hàng" });
    expect(input).toHaveClass("slds-input");
    expect(input).toHaveValue("Minh Phát");
    expect(input).toHaveFocus();
    await userEvent.clear(input);
    await userEvent.type(input, "Đổi tên");
    await userEvent.keyboard("{ArrowLeft}{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(bodyRows()[0].children[2]).toHaveTextContent("Minh Phát");
    expect(bodyRows()[0].children[2]).not.toHaveClass("slds-is-edited");
    expect(pencil("Khách hàng", "Minh Phát - 1.200 bộ")).toHaveFocus();
  });

  it("Enter saves a draft: the cell shows it as edited (unsaved) and reports it", async () => {
    const onDraftsChange = vi.fn();
    render(<DataTable label="Cơ hội" columns={EDIT_COLS} data={DATA} editable onDraftsChange={onDraftsChange} />);
    await userEvent.click(pencil("Khách hàng", "An Khang - 200 bộ"));
    const input = screen.getByRole("textbox", { name: "Khách hàng" });
    await userEvent.clear(input);
    await userEvent.type(input, "An Khang Group{Enter}");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(onDraftsChange).toHaveBeenLastCalledWith({ o2: { account: "An Khang Group" } });
    const cell = bodyRows()[1].children[2];
    expect(cell).toHaveClass("slds-cell-edit", "slds-is-edited");
    expect(cell.querySelector(".slds-truncate")).toHaveAttribute("title", "An Khang Group");
    const note = document.getElementById(cell.getAttribute("aria-describedby")!);
    expect(note).toHaveClass("slds-hide");
    expect(note).toHaveTextContent("Chưa lưu");
    expect(pencil("Khách hàng", "An Khang - 200 bộ")).toHaveFocus();
    // Saving the original value back drops the draft.
    await userEvent.click(pencil("Khách hàng", "An Khang - 200 bộ"));
    const again = screen.getByRole("textbox", { name: "Khách hàng" });
    await userEvent.clear(again);
    await userEvent.type(again, "An Khang{Enter}");
    expect(onDraftsChange).toHaveBeenLastCalledWith({});
    expect(bodyRows()[1].children[2]).not.toHaveClass("slds-is-edited");
  });

  it("a required field refuses an empty value and says so", async () => {
    render(<DataTable label="Cơ hội" columns={EDIT_COLS} data={DATA} editable />);
    await userEvent.click(pencil("Tên", "Bảo Tín - 1.000 bộ"));
    const dialog = screen.getByRole("dialog");
    expect(dialog.querySelector("abbr.slds-required")).toHaveTextContent("*");
    const input = within(dialog).getByRole("textbox");
    expect(input).toBeRequired();
    await userEvent.clear(input);
    await userEvent.keyboard("{Enter}");
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(dialog.querySelector(".slds-form-element")).toHaveClass("slds-has-error");
    expect(input).toHaveAccessibleDescription("Trường này là bắt buộc");
  });

  it("the row's error button opens the blueprint error popover listing every error of the row", async () => {
    render(
      <DataTable
        label="Cơ hội"
        columns={EDIT_COLS}
        data={DATA}
        editable
        cellErrors={{ o1: { name: "Tên đã tồn tại", account: "Khách hàng không tồn tại" } }}
      />,
    );
    const cell = bodyRows()[0].children[2];
    expect(cell).toHaveClass("slds-has-error");
    // Each errored cell is described by its message (a screen reader hears it on the cell).
    expect(cell).toHaveAccessibleDescription("Khách hàng không tồn tại");
    const rowErr = screen.getByRole("button", { name: "Mục 1 có lỗi" });
    expect(rowErr).not.toHaveClass("slds-hidden");
    expect(rowErr).not.toHaveAttribute("aria-hidden");
    await userEvent.click(rowErr);
    const dialog = screen.getByRole("dialog", { name: "Có lỗi cần sửa" });
    expect(dialog.tagName).toBe("SECTION");
    expect(dialog).toHaveClass("slds-popover", "slds-popover_error", "slds-nubbin_bottom-left");
    const close = within(dialog).getByRole("button", { name: "Đóng hộp thoại" });
    expect(close).toHaveClass("slds-button_icon-small", "slds-float_right", "slds-popover__close", "slds-button_icon-inverse");
    expect(close).toHaveFocus();
    const heading = dialog.querySelector("header.slds-popover__header .slds-media.slds-media_center h2")!;
    expect(heading).toHaveClass("slds-truncate", "slds-text-heading_medium");
    expect(heading).toHaveTextContent("Có lỗi cần sửa");
    expect(dialog.querySelector(".slds-media__figure .slds-icon_container.slds-icon-utility-error svg")).toHaveClass("slds-icon", "slds-icon_x-small");
    const body = dialog.querySelector(".slds-popover__body")!;
    expect(dialog).toHaveAttribute("aria-describedby", body.id);
    expect(body.querySelector("ul")).toHaveClass("slds-list_dotted");
    expect(within(dialog).getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      "Tên: Tên đã tồn tại",
      "Khách hàng: Khách hàng không tồn tại",
    ]);
    // Esc closes it onto the error button (still actionable mode); the second Esc leaves to the cell.
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(rowErr).toHaveFocus();
    expect(cellAt(1, 0)).toHaveClass("slds-cell_action-mode");
    await userEvent.keyboard("{Escape}");
    expect(cellAt(1, 0)).toHaveFocus();
  });

  it("a single row error is a paragraph; the cell editor still shows its message", async () => {
    render(
      <DataTable label="Cơ hội" columns={EDIT_COLS} data={DATA} editable cellErrors={{ o1: { account: "Khách hàng không tồn tại" } }} />,
    );
    await userEvent.click(screen.getByRole("button", { name: "Mục 1 có lỗi" }));
    const body = screen.getByRole("dialog").querySelector(".slds-popover__body")!;
    expect(body.querySelector("ul")).toBeNull();
    expect(body.querySelector("p")).toHaveTextContent("Khách hàng: Khách hàng không tồn tại");
    await userEvent.keyboard("{Escape}");
    await userEvent.click(pencil("Khách hàng", "Minh Phát - 1.200 bộ"));
    const input = screen.getByRole("textbox", { name: "Khách hàng" });
    expect(input).toHaveAccessibleDescription("Khách hàng không tồn tại");
    // Blueprint error state: the error sits on the editor's form element, the cell under it is plain.
    expect(input.closest(".slds-form-element")).toHaveClass("slds-has-error");
    expect(bodyRows()[0].children[2]).not.toHaveClass("slds-has-error");
  });

  it("keyboard: Enter on a cell reaches the pencil, Enter opens the editor, arrows stay in the input", async () => {
    render(<DataTable label="Cơ hội" columns={EDIT_COLS} data={DATA} editable />);
    cellAt(1, 1).focus(); // row header (column 0 is the errors column)
    await userEvent.keyboard("{ArrowRight}{Enter}");
    const edit = pencil("Khách hàng", "Minh Phát - 1.200 bộ");
    expect(edit).toHaveFocus();
    await userEvent.keyboard("{Enter}");
    const input = screen.getByRole("textbox", { name: "Khách hàng" });
    expect(input).toHaveFocus();
    await userEvent.keyboard("{ArrowLeft}{ArrowDown}");
    expect(input).toHaveFocus();
    await userEvent.keyboard("{Escape}");
    expect(edit).toHaveFocus();
    await userEvent.keyboard("{Escape}");
    expect(cellAt(1, 2)).toHaveFocus();
  });
});

describe("DataTable — column resize", () => {
  it("renders the blueprint resize handle and range input per resizable column", () => {
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={DATA} resizable sortable />);
    expect(grid()).toHaveClass("slds-table_resizable-cols");
    const th = grid().querySelectorAll("thead th")[0];
    expect(th).toHaveClass("slds-is-resizable", "slds-is-sortable");
    const range = within(th as HTMLElement).getByRole("slider", { name: "Độ rộng cột Tên" });
    expect(range).toHaveClass("slds-resizable__input", "slds-assistive-text");
    expect(range).toHaveAttribute("min", "20");
    expect(range).toHaveAttribute("max", "1000");
    expect(range.parentElement).toHaveClass("slds-resizable");
    expect(range.nextElementSibling).toHaveClass("slds-resizable__handle");
    expect(range.nextElementSibling!.firstElementChild).toHaveClass("slds-resizable__divider");
    // Unsized columns share the width (fixed layout); no inline width until resized.
    expect(th.getAttribute("style")).toBeNull();
  });

  it("the range input (keyboard) and the handle (pointer) resize the column", () => {
    const onColumnSizingChange = vi.fn();
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={DATA} resizable onColumnSizingChange={onColumnSizingChange} />);
    const th = () => grid().querySelectorAll("thead th")[1] as HTMLElement;
    fireEvent.change(within(th()).getByRole("slider"), { target: { value: "240" } });
    expect(onColumnSizingChange).toHaveBeenLastCalledWith({ account: 240 });
    expect(th().style.width).toBe("240px");
    const handle = th().querySelector(".slds-resizable__handle")!;
    fireEvent.pointerDown(handle, { clientX: 100, pointerId: 1 });
    fireEvent.pointerMove(handle, { clientX: 160, pointerId: 1 });
    fireEvent.pointerUp(handle, { clientX: 160, pointerId: 1 });
    fireEvent.pointerMove(handle, { clientX: 400, pointerId: 1 }); // after release: ignored
    // happy-dom has no layout, so the drag starts from the width state already holds.
    expect(th().style.width).toBe("300px");
  });
});

describe("DataTable — scrolling, sticky header, virtualisation", () => {
  const MANY: Opp[] = Array.from({ length: 500 }, (_, i) => ({ id: `r${i}`, name: `Cơ hội ${i + 1}`, account: `KH ${i + 1}`, amount: i }));

  it("maxHeight wraps the grid in a scroller and pins the header", () => {
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={DATA} maxHeight="20rem" />);
    const scroller = grid().parentElement!;
    expect(scroller).toHaveClass("slds-scrollable");
    expect(scroller.getAttribute("style")).toContain("20rem");
    expect(grid().querySelector("thead th")).toHaveClass("sticky");
  });

  it("renders only a window of rows past 100, keeping aria-rowcount / aria-rowindex true", () => {
    // happy-dom has no layout; give the scroller a height so the virtualiser has a viewport.
    const rect = vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockReturnValue(320);
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={MANY} maxHeight="20rem" />);
    rect.mockRestore();
    expect(grid()).toHaveAttribute("aria-rowcount", "501");
    const rendered = grid().querySelectorAll("tbody tr[aria-rowindex]");
    expect(rendered.length).toBeGreaterThan(0);
    expect(rendered.length).toBeLessThan(100);
    expect(rendered[0]).toHaveAttribute("aria-rowindex", "2");
  });

  it("keeps the active row (the grid's one tab stop) mounted when scrolling moves the window away", () => {
    const rect = vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockReturnValue(320);
    try {
      render(<DataTable label="Cơ hội" columns={COLUMNS} data={MANY} maxHeight="20rem" />);
      const scroller = grid().parentElement!;
      const indexes = () => Array.from(grid().querySelectorAll("tbody tr[aria-rowindex]"), (tr) => Number(tr.getAttribute("aria-rowindex")));
      const lastBefore = Math.max(...indexes());
      act(() => {
        Object.defineProperty(scroller, "scrollTop", { value: 8000, configurable: true });
        fireEvent.scroll(scroller);
      });
      // The window moved past everything that was rendered before (happy-dom rows measure as the
      // mocked offsetHeight, so where it lands is not 8000 / 33) …
      const after = indexes();
      expect(after[0]).toBe(2);
      expect(after[1]).toBeGreaterThan(lastBefore);
      // … and the tab stop is still in the DOM, so Tab still reaches the grid.
      const stops = grid().querySelectorAll("[tabindex='0']");
      expect(stops).toHaveLength(1);
      expect(stops[0]).toBe(cellAt(1, 0));
      // Spacers fill both gaps so the kept row and the window sit where they scroll to.
      const spacers = grid().querySelectorAll("tbody tr[aria-hidden='true']");
      expect(spacers.length).toBe(2);
      expect(spacers[0].previousElementSibling).toBe(cellAt(1, 0).parentElement);
    } finally {
      rect.mockRestore();
    }
  });

  it("does not virtualise small tables", () => {
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={MANY.slice(0, 100)} />);
    expect(grid().querySelectorAll("tbody tr[aria-rowindex]")).toHaveLength(100);
  });
});

describe("DataTable — stacked, rowClassName, onRowClick (specs/073 u0-be-mat)", () => {
  const linkCols: ColumnDef<Opp>[] = [{ accessorKey: "name", header: "Tên", cell: (c) => <a href="#x">{c.getValue<string>()}</a> }, COLUMNS[1]];

  it("stacked adds the SLDS max-medium stacked class and a data-label per cell", () => {
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={DATA} stacked />);
    expect(grid()).toHaveClass("slds-max-medium-table_stacked");
    const row = bodyRows()[0];
    expect(within(row).getByRole("rowheader")).toHaveAttribute("data-label", "Tên");
    expect(within(row).getAllByRole("gridcell")[0]).toHaveAttribute("data-label", "Khách hàng");
  });

  it("without stacked the markup has no stacked class or data-label", () => {
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={DATA} />);
    expect(grid()).not.toHaveClass("slds-max-medium-table_stacked");
    expect(grid().querySelector("[data-label]")).toBeNull();
  });

  it("rowClassName puts the class on the row (.nx-proposed)", () => {
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={DATA} rowClassName={(r) => (r.id === "o2" ? "nx-proposed" : undefined)} />);
    const rows = bodyRows();
    expect(rows[1]).toHaveClass("nx-proposed", "slds-hint-parent");
    expect(rows[0]).not.toHaveClass("nx-proposed");
  });

  it("onRowClick fires on a row click with the row data", async () => {
    const onRowClick = vi.fn();
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={DATA} onRowClick={onRowClick} />);
    await userEvent.click(within(bodyRows()[2]).getByText("Bảo Tín"));
    expect(onRowClick).toHaveBeenCalledTimes(1);
    expect(onRowClick.mock.calls[0][0]).toMatchObject({ id: "o3" });
  });

  it("onRowClick does not fire for a click on a control inside the row", async () => {
    const onRowClick = vi.fn();
    render(<DataTable label="Cơ hội" columns={linkCols} data={DATA} onRowClick={onRowClick} />);
    await userEvent.click(screen.getAllByRole("link")[0]);
    expect(onRowClick).not.toHaveBeenCalled();
  });

  it("onRowClick fires on Enter in a plain cell, and the row is a pointer target", async () => {
    const onRowClick = vi.fn();
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={DATA} onRowClick={onRowClick} />);
    expect(bodyRows()[0]).toHaveClass("cursor-pointer");
    within(bodyRows()[0]).getByRole("rowheader").focus();
    await userEvent.keyboard("{Enter}");
    expect(onRowClick).toHaveBeenCalledTimes(1);
    expect(onRowClick.mock.calls[0][0]).toMatchObject({ id: "o1" });
  });

  it("Enter on a cell that holds a control goes to the control, not the row", async () => {
    const onRowClick = vi.fn();
    render(<DataTable label="Cơ hội" columns={linkCols} data={DATA} onRowClick={onRowClick} />);
    within(bodyRows()[0]).getByRole("rowheader").focus();
    await userEvent.keyboard("{Enter}");
    expect(onRowClick).not.toHaveBeenCalled();
  });

  it("virtualisation is unchanged with the new props on (still windowed past 100 rows)", () => {
    const many: Opp[] = Array.from({ length: 500 }, (_, i) => ({ id: `r${i}`, name: `Cơ hội ${i + 1}`, account: `KH ${i + 1}`, amount: i }));
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={many} onRowClick={() => {}} stacked />);
    expect(grid().querySelectorAll("tbody tr[aria-rowindex]").length).toBeLessThan(many.length);
  });
});

describe("DataTable — rowDetail", () => {
  it("draws the detail as a full-width row right under its own row, and nothing for the others", () => {
    render(<DataTable label="Cơ hội" columns={COLUMNS} data={DATA} rowDetail={(o) => (o.id === "o2" ? <p>Xác nhận An Khang?</p> : null)} />);
    const rows = bodyRows();
    expect(rows).toHaveLength(4);
    expect(rows[1]).toHaveTextContent("An Khang - 200 bộ");
    expect(rows[2]).toHaveTextContent("Xác nhận An Khang?");
    expect(rows[2].querySelector("td")).toHaveAttribute("colspan", "3");
  });
});
