// Carried from components/studio/LayoutBuilder.viewFields.test.tsx (DAILY-QUEUE #5) onto the new panel.
import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import type { FieldMeta } from "@/lib/api";
import { listDraftFromView, listDraftToJson } from "@/lib/screenEditorModel";
import { ViewFieldsPanel } from "./ViewFieldsPanel";

const f = (name: string, label: string, fieldType: string): FieldMeta => ({ id: name, name, label, fieldType }) as FieldMeta;
const fields = [f("subject", "Tiêu đề", "text"), f("priority", "Mức ưu tiên", "picklist"), f("status", "Trạng thái", "picklist"), f("reported_at", "Ngày tiếp nhận", "date"), f("due", "Hạn xử lý", "datetime")];

function mount(over: { kanban?: string; calendar?: string; fields?: FieldMeta[] } = {}) {
  const onChange = vi.fn();
  const draft = { ...listDraftFromView(null, fields), cols: ["subject"], kanban: over.kanban ?? "", calendar: over.calendar ?? "" };
  render(<ViewFieldsPanel fields={over.fields ?? fields} draft={draft} onChange={onChange} />);
  return { onChange, draft };
}

describe("ViewFieldsPanel", () => {
  it("Kanban lists only picklists, Calendar only date and datetime, both start on Tự đoán", () => {
    mount();
    const k = screen.getByLabelText("Cột Kanban") as HTMLSelectElement;
    const c = screen.getByLabelText("Trường ngày cho Lịch") as HTMLSelectElement;
    expect([...k.options].map((o) => o.textContent)).toEqual(["Tự đoán", "Mức ưu tiên", "Trạng thái"]);
    expect([...c.options].map((o) => o.textContent)).toEqual(["Tự đoán", "Ngày tiếp nhận", "Hạn xử lý"]);
    expect(k.value).toBe("");
  });

  it("choosing a field reaches the draft, and the saved JSON carries it without sortable", () => {
    const { onChange, draft } = mount();
    fireEvent.change(screen.getByLabelText("Cột Kanban"), { target: { value: "status" } });
    const next = onChange.mock.calls[0][0];
    expect(next.kanban).toBe("status");
    expect(next).not.toEqual(draft);
    expect(JSON.stringify(listDraftToJson(next))).not.toContain("sortable");
  });

  it("a declared field that is no longer usable is named, not shown as Tự đoán", () => {
    mount({ kanban: "old_stage" });
    const k = screen.getByLabelText("Cột Kanban") as HTMLSelectElement;
    expect(k.value).toBe("old_stage");
    expect(screen.getByRole("option", { name: "old_stage — không dùng được nữa" })).toBeTruthy();
  });

  it("a table with no picklist / no date says so and disables the select", () => {
    mount({ fields: [f("subject", "Tiêu đề", "text")] });
    expect(screen.getByText("Bảng này chưa có trường chọn nào để dựng Kanban.")).toBeTruthy();
    expect(screen.getByText("Bảng này chưa có trường ngày nào để dựng Lịch.")).toBeTruthy();
    expect(screen.getByLabelText("Cột Kanban")).toBeDisabled();
  });
});
