import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ApiRecord, FieldMeta } from "@/lib/api";
import { listDraftFromView } from "@/lib/screenEditorModel";
import { ListCanvas } from "./ListCanvas";

const f = (name: string, label: string, fieldType = "text", extra: Partial<FieldMeta> = {}): FieldMeta => ({ id: name, name, label, fieldType, ...extra }) as FieldMeta;
const fields = [f("name", "Tên"), f("stage", "Giai đoạn", "picklist", { options: { values: ["new", "won"], valueLabels: { new: "Mới", won: "Thắng" } } }), f("due", "Hạn", "date")];
const rec = (id: string, name: string): ApiRecord => ({ id, data: { name, stage: "new" } }) as unknown as ApiRecord;

function mount(over: Partial<React.ComponentProps<typeof ListCanvas>> = {}) {
  const onSelect = vi.fn();
  const draft = { ...listDraftFromView(null, fields), cols: ["name", "stage"] };
  render(
    <QueryClientProvider client={new QueryClient()}>
      <ListCanvas tableLabel="Cơ hội" fields={fields} draft={draft} records={[rec("1", "Hợp đồng A")]} appName="App" selected={null} onSelect={onSelect} {...over} />
    </QueryClientProvider>,
  );
  return { onSelect, draft };
}

describe("ListCanvas", () => {
  it("draws the real data table with the draft's columns and the real records", () => {
    mount();
    expect(screen.getByRole("grid")).toBeTruthy();
    expect(screen.getByRole("button", { name: /Tên/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Giai đoạn/ })).toBeTruthy();
    expect(screen.getByText("Hợp đồng A")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Hạn/ })).toBeNull();
  });

  it("no records: header stays, body says Chưa có bản ghi", () => {
    mount({ records: [] });
    expect(screen.getByRole("button", { name: /Tên/ })).toBeTruthy();
    expect(screen.getByText("Chưa có bản ghi")).toBeTruthy();
  });

  it("clicking a column header selects that column and does not sort", () => {
    const { onSelect } = mount();
    fireEvent.click(screen.getByRole("button", { name: /Giai đoạn/ }));
    expect(onSelect).toHaveBeenCalledWith("stage");
  });

  it("no columns: the empty note replaces the table", () => {
    mount({ draft: { ...listDraftFromView(null, fields), cols: [] } });
    expect(screen.queryByRole("grid")).toBeNull();
    expect(screen.getByText("Chọn ít nhất một cột bên phải")).toBeTruthy();
  });

  it("declared Kanban shows the real picklist labels as text, Calendar one line; nothing live is mounted", () => {
    const { draft } = mount({ draft: { ...listDraftFromView(null, fields), cols: ["name"], kanban: "stage", calendar: "due" } });
    expect(draft).toBeTruthy();
    expect(screen.getByText("Kanban theo Giai đoạn")).toBeTruthy();
    expect(screen.getByText(/Mới/)).toBeTruthy();
    expect(screen.getByText(/Thắng/)).toBeTruthy();
    expect(screen.getByText("Lịch theo Hạn")).toBeTruthy();
    expect(document.querySelector(".slds-kanban, [data-testid=kanban-board], [data-testid=calendar-view]")).toBeNull();
  });
});
