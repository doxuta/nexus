// FilterPanel — blueprint `panels/filtering` (SLDS 2.264.1) on the FilterRow contract.
import { describe, it, expect, vi } from "vitest";
import { useState } from "react";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { FilterPanel } from "@/components/slds/FilterPanel";
import { filtersToQuery, type FilterField, type FilterRow } from "@/lib/filterModel";

const FIELDS: FilterField[] = [
  { name: "name", label: "Tên", fieldType: "text" },
  { name: "created_at", label: "Ngày tạo", fieldType: "date" },
  { name: "price", label: "Giá niêm yết", fieldType: "currency" },
];
const INITIAL: FilterRow[] = [
  { id: "f1", field: "created_at", op: "eq", value: "this_week" },
  { id: "f2", field: "price", op: "gt", value: "500" },
];

function Harness(props: { errors?: Record<string, string>; locked?: FilterRow[]; onClose?: () => void }) {
  const [rows, setRows] = useState<FilterRow[]>(INITIAL);
  return (
    <>
      <FilterPanel fields={FIELDS} value={rows} onChange={setRows} {...props} />
      <output data-testid="query">{decodeURIComponent(filtersToQuery(rows))}</output>
      <output data-testid="count">{rows.length}</output>
    </>
  );
}
const items = () => Array.from(document.querySelectorAll<HTMLElement>(".slds-filters__item"));

describe("FilterPanel", () => {
  it("markup: panel_filters > stacked form > slds-filters with header, body list and footer links", async () => {
    const onClose = vi.fn();
    render(<Harness onClose={onClose} />);
    const panel = document.querySelector(".slds-panel")!;
    expect(panel).toHaveClass("slds-grid", "slds-grid_vertical", "slds-nowrap", "slds-panel_filters");
    const form = panel.firstElementChild!;
    expect(form).toHaveClass("slds-form", "slds-form_stacked", "slds-grow", "slds-scrollable_y");
    const header = form.querySelector(".slds-filters > .slds-filters__header")!;
    expect(header).toHaveClass("slds-grid", "slds-has-divider_bottom-space");
    expect(header.querySelector("h2")).toHaveTextContent("Bộ lọc");
    const close = within(header as HTMLElement).getByRole("button", { name: "Đóng bảng lọc" });
    expect(close).toHaveClass("slds-button_icon-small", "slds-col_bump-left");
    await userEvent.click(close);
    expect(onClose).toHaveBeenCalled();
    expect(form.querySelector(".slds-filters__body > h3")).toHaveTextContent("Khớp tất cả các bộ lọc sau");
    const [created, price] = items();
    expect(created.parentElement).toHaveClass("slds-item", "slds-hint-parent");
    const edit = within(created).getByRole("button", { name: /Sửa bộ lọc:/ });
    expect(edit).toHaveClass("slds-button_reset", "slds-grow", "slds-has-blur-focus");
    expect(edit.querySelector(".slds-show.slds-text-body_small")).toHaveTextContent("Ngày tạo");
    expect(edit.querySelector(".slds-show:not(.slds-text-body_small)")).toHaveTextContent("bằng Tuần này");
    const remove = within(price).getByRole("button", { name: 'Xoá bộ lọc: Giá niêm yết lớn hơn "500"' });
    expect(remove).toHaveAttribute("title", 'Xoá lớn hơn "500"');
    expect(remove.querySelector("svg")).toHaveClass("slds-button__icon_hint");
    const footer = form.querySelector(".slds-filters__footer")!;
    expect(footer).toHaveClass("slds-grid", "slds-shrink-none");
    expect(within(footer as HTMLElement).getByRole("button", { name: "Xoá tất cả" })).toHaveClass("slds-text-link", "slds-col_bump-left");
  });

  it("clicking a filter opens its editor popover (field · operator · value); edits flow out; Xong closes", async () => {
    render(<Harness />);
    await userEvent.click(within(items()[1]).getByRole("button", { name: /Sửa bộ lọc:/ }));
    const dialog = screen.getByRole("dialog", { name: "Giá niêm yết" });
    expect(dialog).toHaveClass("slds-popover");
    expect(within(dialog).getByRole("combobox", { name: "Trường" })).toHaveTextContent("Giá niêm yết");
    const input = within(dialog).getByRole("spinbutton", { name: "Giá trị" });
    await userEvent.clear(input);
    await userEvent.type(input, "900");
    expect(screen.getByTestId("query")).toHaveTextContent("price[gt]=900");
    await userEvent.click(within(dialog).getByRole("button", { name: "Xong" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(items()[1]).toHaveTextContent('lớn hơn "900"');
  });

  it("Thêm bộ lọc adds a 'Bộ lọc mới' item (slds-is-new) with its editor open; closing it unfilled drops it", async () => {
    render(<Harness />);
    await userEvent.click(screen.getByRole("button", { name: "Thêm bộ lọc" }));
    const fresh = items()[2];
    expect(fresh).toHaveClass("slds-is-new");
    expect(fresh).toHaveTextContent("Bộ lọc mới");
    expect(screen.getByRole("dialog", { name: "Bộ lọc mới" })).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByTestId("count")).toHaveTextContent("2");
    // Its trigger is gone with it: focus returns to "Thêm bộ lọc", not the page body.
    const add = screen.getByRole("button", { name: "Thêm bộ lọc" });
    await vi.waitFor(() => expect(add).toHaveFocus());
    await userEvent.keyboard("{Enter}");
    await userEvent.click(within(screen.getByRole("dialog", { name: "Bộ lọc mới" })).getByRole("button", { name: "Xong" }));
    expect(screen.getByTestId("count")).toHaveTextContent("2");
    await vi.waitFor(() => expect(add).toHaveFocus());
  });

  it("removing a filter moves focus to the next filter, else the previous, else 'Thêm bộ lọc'", async () => {
    const { unmount } = render(<Harness />);
    const edit = (i: number) => within(items()[i]).getByRole("button", { name: /Sửa bộ lọc:/ });
    screen.getByRole("button", { name: /Xoá bộ lọc: Ngày tạo/ }).focus();
    await userEvent.keyboard("{Enter}");
    expect(items()).toHaveLength(1);
    expect(edit(0)).toHaveTextContent("Giá niêm yết");
    expect(edit(0)).toHaveFocus();
    unmount();
    render(<Harness />);
    await userEvent.click(screen.getByRole("button", { name: /Xoá bộ lọc: Giá niêm yết/ }));
    expect(edit(0)).toHaveTextContent("Ngày tạo");
    expect(edit(0)).toHaveFocus();
    await userEvent.click(screen.getByRole("button", { name: /Xoá bộ lọc: Ngày tạo/ }));
    expect(items()).toHaveLength(0);
    expect(screen.getByRole("button", { name: "Thêm bộ lọc" })).toHaveFocus();
  });

  it("remove one, remove all", async () => {
    render(<Harness />);
    await userEvent.click(screen.getByRole("button", { name: /Xoá bộ lọc: Ngày tạo/ }));
    expect(screen.getByTestId("query")).toHaveTextContent("price[gt]=500");
    await userEvent.click(screen.getByRole("button", { name: "Xoá tất cả" }));
    expect(screen.getByTestId("count")).toHaveTextContent("0");
  });

  it("dirty: header becomes Huỷ / Lưu (align-spread)", async () => {
    const onCancel = vi.fn();
    const onSave = vi.fn();
    render(<FilterPanel fields={FIELDS} value={INITIAL} onChange={() => {}} dirty onCancel={onCancel} onSave={onSave} />);
    const header = document.querySelector(".slds-filters__header")!;
    expect(header).toHaveClass("slds-grid_align-spread");
    expect(header.querySelector("h2")).toBeNull();
    await userEvent.click(within(header as HTMLElement).getByRole("button", { name: "Huỷ" }));
    await userEvent.click(within(header as HTMLElement).getByRole("button", { name: "Lưu" }));
    expect(within(header as HTMLElement).getByRole("button", { name: "Lưu" })).toHaveClass("slds-button_brand");
    expect(onCancel).toHaveBeenCalled();
    expect(onSave).toHaveBeenCalled();
  });

  it("error: alert on top, the item slds-has-error with its message describing the edit button", () => {
    render(<Harness errors={{ f2: "Giá phải lớn hơn 0" }} />);
    expect(screen.getByRole("alert")).toHaveTextContent("Không áp dụng được bộ lọc");
    const price = items()[1];
    expect(price).toHaveClass("slds-has-error");
    expect(within(price).getByRole("button", { name: /Sửa bộ lọc:/ })).toHaveAccessibleDescription("Giá phải lớn hơn 0");
    expect(price.nextElementSibling).toHaveClass("slds-text-color_error");
  });

  it("locked filters: own heading with lock icon, disabled edit, no remove", () => {
    render(<Harness locked={[{ id: "f9", field: "name", op: "eq", value: "ACME" }]} />);
    const heading = screen.getByRole("heading", { name: "Bộ lọc bị khoá" });
    expect(heading).toHaveClass("slds-grid");
    expect(heading.querySelector("svg.slds-icon_x-small")).not.toBeNull();
    const lockedItem = items()[2];
    expect(lockedItem).toHaveClass("slds-is-locked");
    expect(within(lockedItem).getByRole("button")).toBeDisabled();
    expect(lockedItem).toHaveTextContent('bằng "ACME"');
  });
});
