// specs/075 L3 T122 + T121: the bar is a polite live region; the table stacks under 768px and edits on the row.
import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { FieldMeta } from "@/lib/api";
import type { DraftSection } from "@/lib/layoutDraft";
import { BulkBar, bulkMessage } from "./BulkBar";
import { BulkFieldTable } from "./BulkFieldTable";

const sections = [{ id: "s1", title: "Chung" }, { id: "s2", title: "Thêm" }];
const noop = vi.fn();

describe("BulkBar", () => {
  it("nothing ticked: only the hint, inside an aria-live=polite region", () => {
    render(<BulkBar count={0} sections={sections} message={null} onRequired={noop} onRemove={noop} onMove={noop} onClear={noop} />);
    const bar = screen.getByTestId("bulk-bar");
    expect(bar).toHaveAttribute("aria-live", "polite");
    expect(bar).toHaveTextContent("Tick nhiều trường để sửa cùng lúc.");
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("ticked: count, actions, and the move goes to the chosen section", async () => {
    const onMove = vi.fn();
    render(<BulkBar count={3} sections={sections} message="Bỏ qua 2 trường bắt buộc: A, B." onRequired={noop} onRemove={noop} onMove={onMove} onClear={noop} />);
    expect(screen.getByText("Đã chọn 3")).toBeInTheDocument();
    expect(screen.getByText("Bỏ qua 2 trường bắt buộc: A, B.")).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByLabelText("Đưa vào khung"), "s2");
    await userEvent.click(screen.getByRole("button", { name: "Chuyển" }));
    expect(onMove).toHaveBeenCalledWith("s2");
  });

  it("record page offers no 'Đặt bắt buộc'", () => {
    render(<BulkBar count={1} detail sections={sections} message={null} onRequired={noop} onRemove={noop} onMove={noop} onClear={noop} />);
    expect(screen.queryByRole("button", { name: "Đặt bắt buộc" })).toBeNull();
  });

  it("bulkMessage names every skip reason with the field labels", () => {
    expect(bulkMessage("Đã bỏ 1 trường khỏi form.", [{ field: "a", reason: "required", detail: [] }, { field: "b", reason: "controls", detail: ["c"] }], (n) => n.toUpperCase()))
      .toBe("Đã bỏ 1 trường khỏi form. Bỏ qua 1 trường bắt buộc: A. Bỏ qua 1 trường đang là điều kiện của ô khác: B. Bỏ ô phụ thuộc trước.");
  });
});

describe("BulkFieldTable", () => {
  const F = (name: string, isRequired = false) => ({ id: name, name, label: name.toUpperCase(), fieldType: "text", isRequired }) as FieldMeta;
  const draft = [{ id: "s1", title: "Chung", columns: 2, cells: [{ id: "c1", kind: "field", field: "a" }] }] as DraftSection[];

  it("stacks rows, shows tên · Kiểu · Khung · Bắt buộc and edits on the row", async () => {
    const onSection = vi.fn();
    const onRequired = vi.fn();
    render(<BulkFieldTable fields={[F("a"), F("b")]} draft={draft} selected={[]} onSelect={noop} onSection={onSection} onRequired={onRequired} />);
    const grid = screen.getByRole("grid");
    expect(grid.className).toContain("slds-max-medium-table_stacked");
    for (const h of ["Trường", "Kiểu", "Khung trên form", "Bắt buộc"]) expect(screen.getByRole("columnheader", { name: h })).toBeInTheDocument();
    const rowB = screen.getAllByRole("row").find((r) => within(r).queryByText("B"))!;
    await userEvent.selectOptions(within(rowB).getByRole("combobox"), "s1");
    expect(onSection).toHaveBeenCalledWith("b", "s1");
    await userEvent.click(within(rowB).getByRole("switch"));
    expect(onRequired).toHaveBeenCalledWith("b", true);
  });
});
