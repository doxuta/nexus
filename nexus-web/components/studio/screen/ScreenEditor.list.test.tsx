// ScreenEditor on the list screen (specs/075 L2b, T110 wire proof): the save reaches the server in the shape the live list
// reads, and the inline guards of the other screens hold here too.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { useState } from "react";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

const api = vi.hoisted(() => ({
  listFields: vi.fn(), getLayout: vi.fn(), listRecords: vi.fn(), createLayout: vi.fn(), updateLayout: vi.fn(), deleteLayout: vi.fn(),
  getTenantBrandingDoc: vi.fn(),
}));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("@/components/studio/usePublishState", () => ({ usePublishState: () => ({ data: { delivered: false, draftableTypes: ["layout"], directTypes: [] } }) }));

import { ApiError, type EntityMeta, type FieldMeta } from "@/lib/api";
import { ScreenEditor } from "./ScreenEditor";

const F = (name: string, label: string, fieldType = "text"): FieldMeta => ({ id: name, name, label, fieldType }) as FieldMeta;
const fields = [F("name", "Tên"), F("stage", "Giai đoạn", "picklist"), F("due", "Hạn", "date"), F("owner", "Phụ trách", "relation")];
const entities = [{ name: "t", label: "Cơ hội" }] as EntityMeta[];
const auto = { entityName: "t", views: { edit: {}, detail: {}, list: { isDefault: true, columns: [{ field: "name" }, { field: "stage" }], defaultSort: { field: "created_at", order: "desc" } } } };
const saved = { entityName: "t", views: { edit: {}, detail: {}, list: { id: "L7", columns: [{ field: "name" }], defaultSort: { field: "updated_at", order: "asc" }, kanbanField: "stage" } } };

function Host() {
  const [truong, setTruong] = useState<string | null>(null);
  return <ScreenEditor slug="northwind" entities={entities} bang="t" man="danh-sach" truong={truong} onTruong={setTruong} onPick={vi.fn()} />;
}
const mount = () =>
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <Host />
    </QueryClientProvider>,
  );
const save = () => screen.getByRole("button", { name: "Lưu thay đổi" });

beforeEach(() => {
  Object.values(api).forEach((f) => f.mockReset());
  api.listFields.mockResolvedValue(fields);
  api.getLayout.mockResolvedValue(auto);
  api.listRecords.mockResolvedValue([{ id: "r1", data: { name: "Hợp đồng A", stage: "new" } }]);
  api.getTenantBrandingDoc.mockResolvedValue({ branding: {} });
  api.createLayout.mockResolvedValue({ id: "L9" });
  api.updateLayout.mockResolvedValue({ id: "L7" });
  api.deleteLayout.mockResolvedValue(undefined);
});

describe("ScreenEditor, list screen", () => {
  it("auto list starts clean; first save POSTs viewType list with the chosen columns, sort and Kanban, no sortable", async () => {
    mount();
    await waitFor(() => expect(screen.getByText("Hợp đồng A")).toBeInTheDocument());
    expect(save()).toBeDisabled();
    await userEvent.click(screen.getByRole("checkbox", { name: "Hiện cột Hạn" }));
    await userEvent.selectOptions(screen.getByLabelText("Sắp xếp theo"), "updated_at");
    await userEvent.selectOptions(screen.getByLabelText("Cột Kanban"), "stage");
    await userEvent.click(save());
    await waitFor(() => expect(api.createLayout).toHaveBeenCalledTimes(1));
    const sent = api.createLayout.mock.calls[0][0];
    expect(sent).toMatchObject({ entityName: "t", viewType: "list", isDefault: false });
    expect(sent.layoutJson).toEqual({
      version: 1,
      columns: [{ field: "name" }, { field: "stage" }, { field: "due" }],
      defaultSort: { field: "updated_at", order: "desc" },
      kanbanField: "stage",
    });
    expect(JSON.stringify(sent)).not.toContain("sortable");
  });

  it("a saved list PUTs; changing only the Kanban field already enables Lưu", async () => {
    api.getLayout.mockResolvedValue({ ...saved, views: { ...saved.views, list: { ...saved.views.list, kanbanField: undefined } } });
    mount();
    await screen.findByText("Hợp đồng A");
    expect(save()).toBeDisabled();
    await userEvent.selectOptions(screen.getByLabelText("Cột Kanban"), "stage");
    expect(save()).toBeEnabled();
    await userEvent.click(save());
    await waitFor(() => expect(api.updateLayout).toHaveBeenCalledWith("L7", expect.objectContaining({ kanbanField: "stage", columns: [{ field: "name" }] })));
    expect(api.createLayout).not.toHaveBeenCalled();
  });

  it("unticking every column locks Lưu and says what to do", async () => {
    mount();
    await screen.findByText("Hợp đồng A");
    await userEvent.click(screen.getByRole("checkbox", { name: "Hiện cột Tên" }));
    await userEvent.click(screen.getByRole("checkbox", { name: "Hiện cột Giai đoạn" }));
    expect(screen.getByText("Chọn ít nhất một cột bên phải")).toBeInTheDocument();
    expect(save()).toBeDisabled();
  });

  it("PUT 404 LAYOUT_NOT_FOUND keeps the edit and offers to save as a new screen", async () => {
    api.getLayout.mockResolvedValue(saved);
    api.updateLayout.mockRejectedValue(new ApiError(404, "LAYOUT_NOT_FOUND", "x"));
    mount();
    await screen.findByText("Hợp đồng A");
    await userEvent.click(screen.getByRole("checkbox", { name: "Hiện cột Giai đoạn" }));
    await userEvent.click(save());
    await userEvent.click(await screen.findByRole("button", { name: "Lưu lại như màn riêng mới" }));
    await waitFor(() => expect(api.createLayout).toHaveBeenCalledTimes(1));
    expect(api.createLayout.mock.calls[0][0].layoutJson.columns).toEqual([{ field: "name" }, { field: "stage" }]);
  });

  it("Đưa màn về màn tự sinh asks in place (no dialog) then DELETEs the layout", async () => {
    api.getLayout.mockResolvedValue(saved);
    mount();
    await screen.findByText("Hợp đồng A");
    await userEvent.click(screen.getByRole("button", { name: "Thao tác của màn hình" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Đưa màn về màn tự sinh" }));
    expect(document.querySelectorAll("[role=dialog]").length).toBe(0);
    await userEvent.click(screen.getByRole("button", { name: "Đưa về màn tự sinh" }));
    await waitFor(() => expect(api.deleteLayout).toHaveBeenCalledWith("L7"));
  });
});
