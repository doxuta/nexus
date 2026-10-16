// specs/075 T144/T145: the relation map's right column — modes, impact-first removal, blocked / error / legacy cases.
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { CanvasEdgeSpec } from "@/lib/relCanvas";
import type { RelationshipImpact } from "@/lib/api";

const api = vi.hoisted(() => ({
  getRelationshipImpact: vi.fn(),
  deleteRelationshipById: vi.fn(),
  setRelationshipViewConfig: vi.fn(),
  updateField: vi.fn(),
}));
vi.mock("@/lib/api", async (importOriginal) => ({ ...(await importOriginal<typeof import("@/lib/api")>()), ...api }));
const toast = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), warning: vi.fn(), info: vi.fn() }));
vi.mock("@/components/ui/Toaster", () => ({ toast }));


// publish state of the project, switched per test (specs/075 L5a)
const publish = vi.hoisted(() => ({ state: { delivered: false, draftableTypes: ["layout", "field"], directTypes: [] as string[] } as Record<string, unknown> }));
vi.mock("@/components/studio/usePublishState", () => ({ usePublishState: () => ({ data: publish.state }) }));

import { ApiError } from "@/lib/api";
import { RelationPanel, type PanelEntity } from "./RelationPanel";

const field = (name: string, label: string, extra = {}) => ({ name, label, fieldType: "text", ...extra });
const entities: PanelEntity[] = [
  { name: "crm_accounts", label: "Khách hàng", fields: [field("name", "Tên"), field("total", "Tổng")] },
  { name: "crm_events", label: "Lịch hẹn", fields: [field("title", "Tiêu đề"), field("account", "Khách hàng liên quan", { fieldType: "relation", id: "f9" })] },
];
const oneMany: CanvasEdgeSpec = {
  id: "r1", source: "crm_accounts", target: "crm_events", relType: "1-N", legacy: false, parallelIndex: 0,
  rel: { id: "r1", name: "crm_events_account", parentEntityName: "crm_accounts", childEntityName: "crm_events", relationshipType: "1-N", childFkColumn: "account_id" },
  field: entities[1].fields[1],
};
const manyMany: CanvasEdgeSpec = {
  id: "r2", source: "crm_accounts", target: "crm_events", relType: "N-N", legacy: false, parallelIndex: 1,
  rel: { id: "r2", name: "acc_ev", parentEntityName: "crm_accounts", childEntityName: "crm_events", relationshipType: "N-N" },
};
const legacy: CanvasEdgeSpec = { id: "legacy:crm_events.account", source: "crm_accounts", target: "crm_events", relType: "1-N", legacy: true, parallelIndex: 0 };

const impact = (over: Partial<RelationshipImpact> = {}): RelationshipImpact => ({
  relationship: oneMany.rel!, affectedRecords: 3, dependentRollups: [], dependentLookups: [], deletable: true, ...over,
});

const onSelect = vi.fn();
const onChanged = vi.fn();
const onCreated = vi.fn();
function mount(selection: Parameters<typeof RelationPanel>[0]["selection"], edges = [oneMany, manyMany]) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <RelationPanel slug="northwind" selection={selection} entities={entities} edges={edges} onSelect={onSelect} onChanged={onChanged} onCreated={onCreated} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  api.getRelationshipImpact.mockResolvedValue(impact());
  api.deleteRelationshipById.mockResolvedValue(undefined);
});

describe("RelationPanel — list and table modes", () => {
  it("lists every link; Enter/click on a row selects that link (keyboard path to an edge)", () => {
    mount(null);
    expect(screen.getByText("Liên kết (2)")).toBeInTheDocument();
    fireEvent.click(screen.getAllByRole("button", { name: /Khách hàng → Lịch hẹn/ })[0]);
    expect(onSelect).toHaveBeenCalledWith({ kind: "edge", id: "r1" });
  });
  it("a table shows 'Mở bảng' to its page and its own links", () => {
    mount({ kind: "node", id: "crm_events" });
    expect(screen.getByRole("link", { name: "Mở bảng" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/du-lieu/crm_events");
    expect(screen.getByText("Liên kết của bảng này (2)")).toBeInTheDocument();
  });
  it("a table without links says so honestly", () => {
    mount({ kind: "node", id: "crm_events" }, []);
    expect(screen.getByText("Bảng này chưa có liên kết nào")).toBeInTheDocument();
  });
});

describe("RelationPanel — one link", () => {
  it("shows the sentence, read-only rows, usage and the label row for 1-N", async () => {
    mount({ kind: "edge", id: "r1" });
    expect(screen.getByText("Mỗi Khách hàng có nhiều Lịch hẹn; mỗi Lịch hẹn thuộc về một Khách hàng.")).toBeInTheDocument();
    expect(screen.getByLabelText("Nhãn trường liên kết")).toHaveValue("Khách hàng liên quan");
    expect(await screen.findByText("3 bản ghi đang giữ liên kết này.")).toBeInTheDocument();
    expect(screen.getByText("Cách hiện danh sách liên quan")).toBeInTheDocument();
  });
  it("hides the label row for Nhiều–nhiều and says why", async () => {
    mount({ kind: "edge", id: "r2" });
    expect(screen.queryByLabelText("Nhãn trường liên kết")).toBeNull();
    await waitFor(() => expect(api.getRelationshipImpact).toHaveBeenCalledWith("r2"));
  });

  it("reads the impact FIRST and never calls DELETE before the in-place confirmation", async () => {
    mount({ kind: "edge", id: "r1" });
    await screen.findByText("3 bản ghi đang giữ liên kết này.");
    fireEvent.click(screen.getByRole("button", { name: "Gỡ liên kết…" }));
    expect(await screen.findByRole("button", { name: "Gỡ liên kết" })).toBeInTheDocument();
    expect(api.getRelationshipImpact.mock.calls.length).toBeGreaterThanOrEqual(2); // fresh read on click
    expect(api.deleteRelationshipById).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByText(/3 bản ghi đang dùng sẽ mất liên kết này/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Gỡ liên kết" }));
    await waitFor(() => expect(api.deleteRelationshipById).toHaveBeenCalledWith("r1"));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(toast.success).toHaveBeenCalled();
  });
  it("Esc / Huỷ leaves the link alone", async () => {
    mount({ kind: "edge", id: "r1" });
    await screen.findByText("3 bản ghi đang giữ liên kết này.");
    fireEvent.click(screen.getByRole("button", { name: "Gỡ liên kết…" }));
    fireEvent.click(await screen.findByRole("button", { name: "Huỷ" }));
    expect(api.deleteRelationshipById).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Gỡ liên kết…" })).toBeInTheDocument();
  });

  it("a link with dependent roll-ups is blocked: no confirm, fields listed as links to the field", async () => {
    api.getRelationshipImpact.mockResolvedValue(impact({ deletable: false, dependentRollups: [{ entityName: "crm_accounts", fieldName: "total" }] }));
    mount({ kind: "edge", id: "r1" });
    const row = await screen.findByRole("link", { name: /Khách hàng · total/ });
    expect(row).toHaveAttribute("href", "/studio/du-an/northwind/dung/du-lieu/crm_accounts?truong=total");
    expect(screen.getByRole("button", { name: "Gỡ liên kết…" })).toBeDisabled();
  });
  it("impact loading or failing keeps removal locked; failure offers 'Thử lại'", async () => {
    api.getRelationshipImpact.mockRejectedValue(new ApiError(500, "INTERNAL", "boom"));
    mount({ kind: "edge", id: "r1" });
    expect(screen.getByRole("button", { name: "Gỡ liên kết…" })).toBeDisabled(); // loading
    expect(await screen.findByText("Chưa đọc được mức ảnh hưởng nên chưa thể gỡ liên kết này.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Gỡ liên kết…" })).toBeDisabled(); // error
    api.getRelationshipImpact.mockResolvedValue(impact());
    fireEvent.click(screen.getByRole("button", { name: "Thử lại" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Gỡ liên kết…" })).toBeEnabled());
  });
  it("404 on DELETE: 'đã được gỡ trước đó' toast and the map reloads", async () => {
    api.deleteRelationshipById.mockRejectedValue(new ApiError(404, "RELATIONSHIP_NOT_FOUND", "gone"));
    mount({ kind: "edge", id: "r1" });
    await screen.findByText("3 bản ghi đang giữ liên kết này.");
    fireEvent.click(screen.getByRole("button", { name: "Gỡ liên kết…" }));
    fireEvent.click(await screen.findByRole("button", { name: "Gỡ liên kết" }));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(toast.warning).toHaveBeenCalledWith("Liên kết đã được gỡ trước đó");
  });
  it("409 RELATIONSHIP_HAS_DEPENDENTS shows the business sentence, not the raw message", async () => {
    api.deleteRelationshipById.mockRejectedValue(new ApiError(409, "RELATIONSHIP_HAS_DEPENDENTS", "raw english"));
    mount({ kind: "edge", id: "r1" });
    await screen.findByText("3 bản ghi đang giữ liên kết này.");
    fireEvent.click(screen.getByRole("button", { name: "Gỡ liên kết…" }));
    fireEvent.click(await screen.findByRole("button", { name: "Gỡ liên kết" }));
    await waitFor(() => expect(toast.error).toHaveBeenCalled());
    expect(String(toast.error.mock.calls[0][0])).not.toMatch(/raw english/);
    expect(String(toast.error.mock.calls[0][0])).toMatch(/trường tổng hợp/);
    expect(onChanged).not.toHaveBeenCalled();
  });

  it("an old link without a registry row: explainer + locked remove, impact never called", () => {
    mount({ kind: "edge", id: "legacy:crm_events.account" }, [legacy]);
    expect(screen.getByText("Liên kết tạo từ phiên bản cũ: chỉ gỡ được từ trang bảng.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Gỡ liên kết…" })).toBeDisabled();
    expect(api.getRelationshipImpact).not.toHaveBeenCalled();
  });

  it("pins setRelationshipViewConfig: 'Lưu cấu hình' PUTs the view config of THIS link", async () => {
    api.setRelationshipViewConfig.mockResolvedValue({ label: "Lịch" });
    mount({ kind: "edge", id: "r1" });
    fireEvent.change(screen.getByLabelText("Tên mục"), { target: { value: "Lịch" } });
    fireEvent.click(screen.getByRole("button", { name: "Lưu cấu hình" }));
    await waitFor(() => expect(api.setRelationshipViewConfig).toHaveBeenCalledWith("r1", { label: "Lịch" }));
  });
});

describe("RelationPanel — create and edit label (L4b)", () => {
  it("'Liên kết mới' arrives from the list and from a table, prefilled with that table", () => {
    mount(null);
    fireEvent.click(screen.getByRole("button", { name: "Liên kết mới" }));
    expect(onSelect).toHaveBeenLastCalledWith({ kind: "new" });
    mount({ kind: "node", id: "crm_events" });
    fireEvent.click(screen.getByRole("button", { name: "Liên kết mới từ bảng này" }));
    expect(onSelect).toHaveBeenLastCalledWith({ kind: "new", parent: "crm_events" });
  });
  it("the 4th mode renders the in-place form, no dialog", () => {
    mount({ kind: "new", parent: "crm_accounts", child: "crm_events" });
    expect(screen.getByRole("button", { name: "Tạo liên kết" })).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
  });
  it("editing 'Nhãn trường liên kết' PUTs the field label; no request ever PATCHes a relationship", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    api.updateField.mockResolvedValue({});
    mount({ kind: "edge", id: "r1" });
    fireEvent.change(screen.getByLabelText("Nhãn trường liên kết"), { target: { value: "Chủ lịch" } });
    fireEvent.click(screen.getByRole("button", { name: "Lưu nhãn" }));
    await waitFor(() => expect(api.updateField).toHaveBeenCalledWith("f9", { label: "Chủ lịch" }));
    expect(fetchSpy.mock.calls.some(([, init]) => (init as RequestInit | undefined)?.method === "PATCH")).toBe(false);
    expect(screen.getByText("Muốn đổi bảng đích hoặc kiểu, gỡ liên kết rồi tạo mới.")).toBeInTheDocument();
    expect(screen.queryByLabelText(/Bảng chính|Bảng chi tiết|Kiểu liên kết/)).toBeNull(); // no edit box for target / kind
  });
  it("Nhiều–nhiều: honest note, no label box", () => {
    mount({ kind: "edge", id: "r2" });
    expect(screen.getByText("Chưa có nhãn liên kết cho Nhiều–nhiều.")).toBeInTheDocument();
    expect(screen.queryByLabelText("Nhãn trường liên kết")).toBeNull();
  });
});

describe("RelationPanel draft labels (specs/075 T192/T193)", () => {
  const delivered = { delivered: true, draftableTypes: ["layout", "field"], directTypes: [] };
  it("not delivered: no 'Áp ngay' label on any link", async () => {
    mount({ kind: "edge", id: "r2" });
    await screen.findByRole("button", { name: "Gỡ liên kết…" });
    expect(screen.queryByText("Áp ngay, không có trong lịch sử")).toBeNull();
  });
  it.each([["N-N", "r2"], ["1-N", "r1"]])("delivered: removing a %s link is applied at once, said before and in the confirm", async (_k, id) => {
    publish.state = delivered;
    mount({ kind: "edge", id });
    expect(await screen.findAllByText("Áp ngay, không có trong lịch sử")).not.toHaveLength(0);
    fireEvent.click(await screen.findByRole("button", { name: "Gỡ liên kết…" }));
    expect(await screen.findByText(/Không quay lại được bằng Xuất bản/)).toBeInTheDocument();
    expect(api.deleteRelationshipById).not.toHaveBeenCalled();
  });
  it("delivered: saving the display asks first, then writes", async () => {
    publish.state = delivered;
    api.setRelationshipViewConfig.mockResolvedValue({ label: "X" });
    mount({ kind: "edge", id: "r2" });
    fireEvent.click(await screen.findByRole("button", { name: "Lưu cấu hình" }));
    expect(api.setRelationshipViewConfig).not.toHaveBeenCalled();
    fireEvent.click(await screen.findByRole("button", { name: "Lưu và áp ngay" }));
    await waitFor(() => expect(api.setRelationshipViewConfig).toHaveBeenCalled());
  });
});
