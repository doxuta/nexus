// DashboardAuthoringPanel on SLDS (specs/038 T029) — the admin widget panel inside
// /dashboards/[id]: add / edit / remove a widget and hand the page the NEXT widget array (built
// on the layout-aware base at save time), plus the delete-dashboard entry point.
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { DashboardWidgetDef } from "@/lib/api";

const api = vi.hoisted(() => ({ listEntities: vi.fn(), listFields: vi.fn(), updateField: vi.fn() }));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));

import { DashboardAuthoringPanel } from "./DashboardAuthoring";

const WIDGETS: DashboardWidgetDef[] = [
  { id: "w1", title: "Pipeline", type: "bar", entity: "crm_opportunities", metric: "count", groupBy: "stage", x: 0, y: 0, w: 6, h: 4 },
];
const FIELDS = [
  { name: "stage", label: "Giai đoạn", fieldType: "select", isIndexed: true, options: { values: ["won"] } },
  { name: "amount", label: "Giá trị", fieldType: "currency" },
  { name: "doctor", label: "Bác sĩ", fieldType: "relation", options: { relatedEntity: "doctors" } },
  { id: "f-note", name: "note", label: "Ghi chú", fieldType: "text" },
];

function renderPanel(extra: { draftState?: "live" | "edited" | "new" | "deleted" } = {}) {
  const props = {
    ...extra,
    widgets: WIDGETS,
    saving: false,
    getBaseWidgets: () => WIDGETS,
    onSave: vi.fn().mockResolvedValue(true),
    onDeleteDashboard: vi.fn(),
  };
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <DashboardAuthoringPanel {...props} />
    </QueryClientProvider>,
  );
  return props;
}

beforeEach(() => {
  api.listEntities.mockReset().mockResolvedValue([{ name: "crm_opportunities", label: "Cơ hội", tableName: "crm_opportunities" }]);
  api.listFields.mockReset().mockResolvedValue(FIELDS);
});

describe("DashboardAuthoringPanel", () => {
  it("is an SLDS card listing the widgets", async () => {
    renderPanel();
    const panel = screen.getByTitle("Quản lý ô số liệu").closest(".slds-card") as HTMLElement;
    expect(within(panel).getByText("Pipeline")).toBeInTheDocument();
    expect(within(panel).getByText("Biểu đồ cột · crm_opportunities · Đếm theo stage")).toBeInTheDocument();
  });

  it("adds a widget: the form's choices go into the next array after the existing ones", async () => {
    const p = renderPanel();
    const add = screen.getByRole("button", { name: "Thêm ô số liệu" });
    await screen.findByRole("button", { name: "Thêm ô số liệu" });
    await vi.waitFor(() => expect(add).toBeEnabled());
    fireEvent.click(add);
    expect(screen.getByText("Ô số liệu mới")).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Tiêu đề"), { target: { value: "Số cơ hội" } });
    fireEvent.click(screen.getByLabelText("Biểu đồ cột"));
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Lưu ô số liệu" }));
    });
    const next = p.onSave.mock.calls[0][0] as DashboardWidgetDef[];
    expect(next).toHaveLength(2);
    expect(next[0]).toEqual(WIDGETS[0]);
    expect(next[1]).toMatchObject({ title: "Số cơ hội", type: "bar", entity: "crm_opportunities", metric: "count" });
  });

  it("edits a widget in place and keeps its grid rect", async () => {
    const p = renderPanel();
    fireEvent.click(screen.getByRole("button", { name: "Sửa ô số liệu Pipeline" }));
    expect(screen.getByText("Sửa ô số liệu")).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Tiêu đề"), { target: { value: "Pipeline theo giai đoạn" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Lưu ô số liệu" }));
    });
    const next = p.onSave.mock.calls[0][0] as DashboardWidgetDef[];
    expect(next).toEqual([{ ...WIDGETS[0], title: "Pipeline theo giai đoạn" }]);
  });

  // specs/070 #265: a widget groups by a relation ("theo bác sĩ") or the record owner; an
  // unindexed field is indexed in place and blocks saving until then.
  it("groups by a relation or the owner; an unindexed field is fixed in place", async () => {
    api.updateField.mockReset().mockResolvedValue({});
    const p = renderPanel();
    fireEvent.click(screen.getByRole("button", { name: "Sửa ô số liệu Pipeline" }));
    const groupBy = await screen.findByLabelText("Nhóm theo (groupBy)");
    await vi.waitFor(() => expect(within(groupBy).getByRole("option", { name: "Bác sĩ" })).not.toBeDisabled());
    expect(within(groupBy).getByRole("option", { name: "Người phụ trách" })).not.toBeDisabled();

    fireEvent.change(groupBy, { target: { value: "note" } });
    const save = screen.getByRole("button", { name: "Lưu ô số liệu" });
    expect(save).toBeDisabled();
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Bật tìm nhanh ngay" }));
    });
    expect(api.updateField).toHaveBeenCalledWith("f-note", { options: { indexed: true } });

    fireEvent.change(groupBy, { target: { value: "doctor" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Lưu ô số liệu" }));
    });
    expect(p.onSave.mock.calls[0][0]).toEqual([{ ...WIDGETS[0], groupBy: "doctor" }]);
  });

  // specs/070 #266: KPI "Phiếu đang mở" = a widget WITH a fixed condition. The form used to say
  // only "có N điều kiện lọc đã lưu"; it now edits them with the list's condition builder.
  it("edits the widget's fixed filters: saved ones show, a condition can be added or removed", async () => {
    const withFilter: DashboardWidgetDef = { ...WIDGETS[0], filters: [{ field: "stage", op: "neq", values: ["won"] }] };
    const onSave = vi.fn().mockResolvedValue(true);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <DashboardAuthoringPanel widgets={[withFilter]} saving={false} getBaseWidgets={() => [withFilter]} onSave={onSave} onDeleteDashboard={vi.fn()} />
      </QueryClientProvider>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Sửa ô số liệu Pipeline" }));
    const builder = screen.getByRole("heading", { name: "Bộ lọc cố định" }).closest(".slds-expression") as HTMLElement;
    await vi.waitFor(() => expect(within(builder).getByRole("combobox", { name: "Trường" })).toHaveTextContent("Giai đoạn"));
    // Add "Ghi chú bằng QS-1" next to it.
    fireEvent.click(within(builder).getByRole("button", { name: "Thêm điều kiện" }));
    const added = builder.querySelectorAll<HTMLElement>("li.slds-expression__row")[1];
    fireEvent.click(within(added).getByRole("combobox", { name: "Trường" }));
    const listbox = document.getElementById(within(added).getByRole("combobox", { name: "Trường" }).getAttribute("aria-controls")!)!;
    fireEvent.click(within(listbox).getByRole("option", { name: "Ghi chú" }));
    const save = screen.getByRole("button", { name: "Lưu ô số liệu" });
    expect(save).toBeDisabled(); // half-typed: no value yet
    fireEvent.change(within(added).getByRole("textbox", { name: "Giá trị" }), { target: { value: "QS-1" } });
    await act(async () => {
      fireEvent.click(save);
    });
    expect(onSave.mock.calls[0][0][0].filters).toEqual([
      { field: "stage", op: "neq", values: ["won"] },
      { field: "note", op: "eq", values: ["QS-1"] },
    ]);
  });

  it("removes a widget only after confirming", async () => {
    const p = renderPanel();
    fireEvent.click(screen.getByRole("button", { name: "Xoá ô số liệu Pipeline" }));
    expect(screen.getByText("Xoá ô số liệu này?")).toBeInTheDocument();
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Xoá" }));
    });
    expect(p.onSave).toHaveBeenCalledWith([]);
  });

  it("hands dashboard deletion to the page", () => {
    const p = renderPanel();
    fireEvent.click(screen.getByRole("button", { name: "Xoá trang tổng quan" }));
    expect(p.onDeleteDashboard).toHaveBeenCalled();
  });
});

// specs/063-draft-publish S7-2 T060: a dashboard read over the draft says the customer does not see it.
describe("DashboardAuthoringPanel — draft workspace", () => {
  it.each(["edited", "new"] as const)("a %s draft shows 'Đã lưu vào bản nháp · Khách chưa thấy'", (draftState) => {
    renderPanel({ draftState });
    expect(screen.getByText("Đã lưu vào bản nháp · Khách chưa thấy")).toBeInTheDocument();
  });

  it.each([undefined, "live"] as const)("no notice when there is no draft change (%s)", (draftState) => {
    renderPanel({ draftState });
    expect(screen.queryByText("Đã lưu vào bản nháp · Khách chưa thấy")).toBeNull();
  });
});
