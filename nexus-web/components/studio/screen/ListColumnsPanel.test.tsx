import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import type { FieldMeta } from "@/lib/api";
import { listDraftFromView } from "@/lib/screenEditorModel";
import { ListColumnsPanel } from "./ListColumnsPanel";

const f = (name: string, label: string, fieldType = "text"): FieldMeta => ({ id: name, name, label, fieldType }) as FieldMeta;
const fields = [f("name", "Tên"), f("stage", "Giai đoạn", "picklist"), f("owner", "Người phụ trách", "relation"), f("note", "Ghi chú dài", "rich_text")];

function mount() {
  const onChange = vi.fn();
  const draft = { ...listDraftFromView(null, fields), cols: ["name", "stage"] };
  render(<ListColumnsPanel fields={fields} draft={draft} selected={null} onSelect={vi.fn()} onChange={onChange} />);
  return { onChange, draft };
}

describe("ListColumnsPanel", () => {
  it("lists only fields that can be columns, ticked ones first in column order", () => {
    mount();
    expect(screen.getByRole("checkbox", { name: "Hiện cột Tên" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "Hiện cột Giai đoạn" })).toBeChecked();
    expect(screen.queryByRole("checkbox", { name: /Người phụ trách/ })).toBeNull();
    expect(screen.queryByRole("checkbox", { name: /Ghi chú dài/ })).toBeNull();
    expect(screen.getByText(/không đưa vào cột được/)).toBeTruthy();
  });

  it("untick removes the column; Xuống moves it", () => {
    const { onChange } = mount();
    fireEvent.click(screen.getByRole("checkbox", { name: "Hiện cột Tên" }));
    expect(onChange.mock.calls[0][0].cols).toEqual(["stage"]);
    fireEvent.click(screen.getByRole("button", { name: "Xuống: Tên" }));
    expect(onChange.mock.calls[1][0].cols).toEqual(["stage", "name"]);
  });

  it("sort offers only the three server sort keys, with business names", () => {
    const { onChange } = mount();
    const sel = screen.getByLabelText("Sắp xếp theo") as HTMLSelectElement;
    expect([...sel.options].map((o) => o.textContent)).toEqual(["Ngày tạo", "Ngày cập nhật", "Mã"]);
    fireEvent.change(sel, { target: { value: "updated_at" } });
    expect(onChange.mock.calls[0][0]).toMatchObject({ sortField: "updated_at", sortOrder: "desc" });
    fireEvent.change(screen.getByLabelText("Thứ tự"), { target: { value: "asc" } });
    expect(onChange.mock.calls[1][0]).toMatchObject({ sortOrder: "asc" });
  });
});
