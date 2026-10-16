// TreeGrid — blueprint tree-grid/base (SLDS 2.264.1) on @tanstack/react-table getSubRows.
import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ColumnDef } from "@tanstack/react-table";
import { TreeGrid } from "./TreeGrid";

interface Account {
  id: string;
  name: string;
  employees: number;
  city: string;
  children?: Account[];
}

const DATA: Account[] = [
  { id: "a1", name: "Minh Phát", employees: 3100, city: "TP. Hồ Chí Minh" },
  {
    id: "a2",
    name: "Tập đoàn An Khang",
    employees: 10000,
    city: "Hà Nội",
    children: [
      { id: "a2-1", name: "An Khang Đà Nẵng", employees: 745, city: "Đà Nẵng" },
      { id: "a2-2", name: "An Khang Cần Thơ", employees: 578, city: "Cần Thơ", children: [{ id: "a2-2-1", name: "An Khang Sóc Trăng", employees: 90, city: "Sóc Trăng" }] },
    ],
  },
  { id: "a3", name: "Bảo Tín", employees: 6000, city: "Hải Phòng" },
];

const COLUMNS: ColumnDef<Account>[] = [
  { accessorKey: "name", header: "Khách hàng" },
  { accessorKey: "employees", header: "Nhân sự", meta: { numeric: true } },
  { accessorKey: "city", header: "Thành phố" },
];

const grid = () => screen.getByRole("treegrid");
const row = (name: string) => screen.getByText(name).closest("tr") as HTMLElement;
const renderTree = (props: Partial<Parameters<typeof TreeGrid<Account>>[0]> = {}) =>
  render(<TreeGrid label="Khách hàng theo cấp" columns={COLUMNS} data={DATA} getSubRows={(a) => a.children} {...props} />);

describe("TreeGrid — markup", () => {
  it("renders the blueprint treegrid with level / position / expanded state on rows", () => {
    renderTree({ selectable: true });
    const table = grid();
    expect(table).toHaveClass("slds-table", "slds-table_bordered", "slds-table_edit", "slds-table_fixed-layout", "slds-table_resizable-cols", "slds-tree", "slds-table_tree");
    expect(table).toHaveAttribute("aria-readonly", "true");
    expect(table).toHaveAttribute("aria-multiselectable", "true");
    const parent = row("Tập đoàn An Khang");
    expect(parent).toHaveAttribute("aria-level", "1");
    expect(parent).toHaveAttribute("aria-posinset", "2");
    expect(parent).toHaveAttribute("aria-setsize", "3");
    expect(parent).toHaveAttribute("aria-expanded", "false");
    expect(parent).toHaveAttribute("aria-selected", "false");
    expect(parent).toHaveClass("slds-hint-parent");
    const leaf = row("Minh Phát");
    expect(leaf).not.toHaveAttribute("aria-expanded");
    expect(leaf).toHaveAttribute("tabindex", "0"); // first row is the tab stop
    expect(parent).toHaveAttribute("tabindex", "-1");
    // Tree column: th.slds-tree__item[scope=row] with the chevron button, then the truncated value.
    const item = parent.querySelector("th.slds-tree__item")!;
    expect(item).toHaveAttribute("scope", "row");
    expect(item).toHaveAttribute("data-label", "Khách hàng");
    const chevron = item.querySelector("button")!;
    expect(chevron).toHaveClass("slds-button", "slds-button_icon", "slds-button_icon-x-small", "slds-m-right_x-small");
    expect(chevron).toHaveAttribute("aria-hidden", "true");
    expect(chevron).toHaveAttribute("tabindex", "-1");
    expect(chevron).toHaveAttribute("title", "Mở rộng Tập đoàn An Khang");
    expect(chevron.querySelector("svg")).toHaveClass("slds-button__icon", "slds-button__icon_small");
    expect(chevron.nextElementSibling).toHaveClass("slds-truncate");
    expect(chevron.nextElementSibling).toHaveAttribute("title", "Tập đoàn An Khang");
    // Leaves keep the chevron slot but hide it (slds-is-disabled).
    expect(leaf.querySelector(".slds-tree__item button")).toHaveClass("slds-is-disabled");
    // Other cells are gridcells with data-label.
    expect(parent.querySelector("td[data-label='Thành phố']")).toHaveAttribute("role", "gridcell");
  });

  it("the chevron expands and collapses a branch", async () => {
    renderTree();
    expect(screen.queryByText("An Khang Đà Nẵng")).toBeNull();
    await userEvent.click(row("Tập đoàn An Khang").querySelector(".slds-tree__item button")!);
    const parent = row("Tập đoàn An Khang");
    expect(parent).toHaveAttribute("aria-expanded", "true");
    expect(parent.querySelector(".slds-tree__item button")).toHaveAttribute("title", "Thu gọn Tập đoàn An Khang");
    const child = row("An Khang Cần Thơ");
    expect(child).toHaveAttribute("aria-level", "2");
    expect(child).toHaveAttribute("aria-posinset", "2");
    expect(child).toHaveAttribute("aria-setsize", "2");
    expect(child).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(parent.querySelector(".slds-tree__item button")!);
    expect(screen.queryByText("An Khang Đà Nẵng")).toBeNull();
  });

  it("honours controlled expanded state and reports toggles", async () => {
    const onExpandedChange = vi.fn();
    renderTree({ expanded: { a2: true }, onExpandedChange });
    expect(row("An Khang Đà Nẵng")).toHaveAttribute("aria-level", "2");
    await userEvent.click(row("An Khang Cần Thơ").querySelector(".slds-tree__item button")!);
    expect(onExpandedChange).toHaveBeenLastCalledWith({ a2: true, "a2-2": true });
  });
});

describe("TreeGrid — keyboard", () => {
  it("Right expands then enters the cells; Left walks back to the row, collapses, and climbs to the parent", async () => {
    renderTree();
    const parent = () => row("Tập đoàn An Khang");
    row("Minh Phát").focus();
    await userEvent.keyboard("{ArrowDown}");
    expect(parent()).toHaveFocus();
    await userEvent.keyboard("{ArrowRight}");
    expect(parent()).toHaveAttribute("aria-expanded", "true");
    expect(parent()).toHaveFocus();
    await userEvent.keyboard("{ArrowRight}");
    expect(parent().querySelector("th.slds-tree__item")).toHaveFocus();
    await userEvent.keyboard("{ArrowRight}");
    expect(parent().querySelector("td[data-label='Nhân sự']")).toHaveFocus();
    await userEvent.keyboard("{ArrowLeft}{ArrowLeft}");
    expect(parent()).toHaveFocus();
    await userEvent.keyboard("{ArrowDown}{ArrowDown}");
    expect(row("An Khang Cần Thơ")).toHaveFocus();
    await userEvent.keyboard("{ArrowLeft}"); // collapsed child: to its parent
    expect(parent()).toHaveFocus();
    await userEvent.keyboard("{ArrowLeft}"); // expanded parent: collapse
    expect(parent()).toHaveAttribute("aria-expanded", "false");
    expect(parent()).toHaveFocus();
  });

  it("Up / Ctrl+Home reach the header cells (sort, select-all), Down comes back into the rows", async () => {
    renderTree({ selectable: true, sortable: true });
    const heads = () => grid().querySelectorAll<HTMLElement>("thead th");
    row("Minh Phát").focus();
    await userEvent.keyboard("{ArrowUp}");
    expect(heads()[0]).toHaveFocus(); // select-all
    await userEvent.keyboard("{ArrowRight}{Enter}");
    expect(within(heads()[1]).getByRole("button")).toHaveFocus(); // sort link
    await userEvent.keyboard("{Escape}{ArrowDown}");
    expect(row("Minh Phát").querySelector("th.slds-tree__item")).toHaveFocus();
    await userEvent.keyboard("{Control>}{Home}{/Control}");
    expect(heads()[0]).toHaveFocus();
  });

  it("an empty tree still has one tab stop, on the header", () => {
    renderTree({ data: [] });
    const stops = grid().querySelectorAll("[tabindex='0']");
    expect(stops).toHaveLength(1);
    expect(stops[0]).toBe(grid().querySelector("thead th"));
  });

  it("Space selects the focused row only (no cascade into children)", async () => {
    const onRowSelectionChange = vi.fn();
    renderTree({ selectable: true, defaultExpanded: { a2: true }, onRowSelectionChange });
    row("Minh Phát").focus();
    await userEvent.keyboard("{ArrowDown} ");
    expect(onRowSelectionChange).toHaveBeenLastCalledWith({ a2: true });
    expect(row("Tập đoàn An Khang")).toHaveAttribute("aria-selected", "true");
    expect(row("Tập đoàn An Khang")).toHaveClass("slds-hint-parent", "slds-is-selected");
    expect(row("An Khang Đà Nẵng")).toHaveAttribute("aria-selected", "false");
    expect(within(row("Tập đoàn An Khang")).getByRole("checkbox")).toBeChecked();
  });
});
