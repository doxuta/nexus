// ListFilters — the record list's filter glue around the kit (specs/038 T018/T022): the docked kit
// FilterPanel (blueprint panels/filtering) and the active-filter pills above the list.
import { describe, it, expect } from "vitest";
import { useState } from "react";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ListFilterDock, ListFilterPills } from "@/components/layout/ListFilters";
import type { FilterField, FilterRow } from "@/lib/filterModel";

const FIELDS: FilterField[] = [
  { name: "stage", label: "Giai đoạn", fieldType: "picklist", options: { values: ["New", "Won"], valueLabels: { New: "Mới", Won: "Thắng" } } },
  { name: "created_at", label: "Ngày tạo", fieldType: "date" },
  { name: "amount", label: "Giá trị", fieldType: "currency" },
];
const ROWS: FilterRow[] = [
  { id: "a", field: "stage", op: "eq", value: "New" },
  { id: "b", field: "created_at", op: "eq", value: "this_week" },
];

function Harness({ initial = ROWS, startOpen = false }: { initial?: FilterRow[]; startOpen?: boolean }) {
  const [rows, setRows] = useState<FilterRow[]>(initial);
  const [open, setOpen] = useState(startOpen);
  return (
    <>
      <button type="button" aria-controls="fp" aria-pressed={open} onClick={() => setOpen((v) => !v)}>
        Bật bộ lọc
      </button>
      <ListFilterPills fields={FIELDS} value={rows} onChange={setRows} panelId="fp" />
      <ListFilterDock id="fp" open={open} onClose={() => setOpen(false)} fields={FIELDS} value={rows} onChange={setRows} />
      <output data-testid="count">{rows.length}</output>
    </>
  );
}

const pills = () => screen.queryAllByRole("button", { name: /^Bỏ lọc / });

describe("ListFilterPills", () => {
  it("words each active filter with the kit's field label + condition text (picklist labels, relative dates)", () => {
    render(<Harness />);
    expect(screen.getByLabelText("Bộ lọc đang áp dụng")).toHaveClass("slds-listbox_horizontal");
    expect(screen.getByText('Giai đoạn bằng "Mới"')).toBeInTheDocument();
    expect(screen.getByText("Ngày tạo bằng Tuần này")).toBeInTheDocument();
  });

  it("removing the last pill hands focus to the filter toggle instead of dropping it on <body>", async () => {
    render(<Harness />);
    await userEvent.click(pills()[0]);
    // Not the last one: the kit PillContainer moves focus to the remaining pill.
    expect(pills()).toHaveLength(1);
    expect(document.activeElement).toBe(pills()[0]);
    await userEvent.keyboard("{Delete}");
    expect(pills()).toHaveLength(0);
    expect(screen.getByTestId("count")).toHaveTextContent("0");
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Bật bộ lọc" }));
  });
});

describe("ListFilterDock", () => {
  it("is the kit FilterPanel (panels/filtering), hidden until the toggle opens it", async () => {
    render(<Harness />);
    const dock = document.getElementById("fp")!;
    expect(dock).toHaveClass("slds-hide");
    const panel = dock.querySelector(".slds-panel")!;
    expect(panel).toHaveClass("slds-panel_filters");
    expect(panel.querySelector(".slds-filters > .slds-filters__body")).not.toBeNull();
    // Remove buttons carry the blueprint's hint glyph.
    const remove = within(dock).getByRole("button", { name: 'Xoá bộ lọc: Giai đoạn bằng "Mới"' });
    expect(remove.querySelector("svg")).toHaveClass("slds-button__icon_hint");
    await userEvent.click(screen.getByRole("button", { name: "Bật bộ lọc" }));
    expect(dock).not.toHaveClass("slds-hide");
  });

  it("opening from the toggle moves focus to the first filter; closing returns it to the toggle", async () => {
    render(<Harness />);
    const toggle = screen.getByRole("button", { name: "Bật bộ lọc" });
    await userEvent.click(toggle);
    const dock = document.getElementById("fp")!;
    expect(document.activeElement).toBe(within(dock).getAllByRole("button", { name: /Sửa bộ lọc:/ })[0]);
    await userEvent.click(within(dock).getByRole("button", { name: "Đóng bảng lọc" }));
    expect(dock).toHaveClass("slds-hide");
    expect(document.activeElement).toBe(toggle);
  });

  it("a panel already open on load (deep link) does not steal focus", () => {
    render(<Harness startOpen />);
    expect(document.activeElement).toBe(document.body);
  });

  it("editing in the panel flows into the pills (one filter model)", async () => {
    render(<Harness startOpen />);
    const dock = document.getElementById("fp")!;
    await userEvent.click(within(dock).getByRole("button", { name: /Xoá bộ lọc: Ngày tạo/ }));
    expect(screen.queryByText("Ngày tạo bằng Tuần này")).toBeNull();
    // The kit keeps focus inside the panel on the remaining filter.
    expect(document.activeElement).toBe(within(dock).getByRole("button", { name: /Sửa bộ lọc:/ }));
  });
});
