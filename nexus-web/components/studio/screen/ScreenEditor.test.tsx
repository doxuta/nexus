import { beforeEach, describe, expect, it, vi } from "vitest";
import { useState } from "react";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

const api = vi.hoisted(() => ({
  listFields: vi.fn(), getLayout: vi.fn(), listRecords: vi.fn(), createLayout: vi.fn(), updateLayout: vi.fn(), deleteLayout: vi.fn(),
  getTenantBrandingDoc: vi.fn(), listAllRelationships: vi.fn(), setRelationshipViewConfig: vi.fn(),
}));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));


// publish state of the project, switched per test (specs/075 L5a)
const publish = vi.hoisted(() => ({ state: { delivered: false, draftableTypes: ["layout", "field"], directTypes: [] as string[] } as Record<string, unknown> }));
vi.mock("@/components/studio/usePublishState", () => ({ usePublishState: () => ({ data: publish.state }) }));

import { ApiError, type EntityMeta, type FieldMeta } from "@/lib/api";
import { ScreenEditor } from "./ScreenEditor";

const F = (name: string, label: string, isRequired = false): FieldMeta => ({ name, label, fieldType: "text", isRequired }) as FieldMeta;
const fields = [F("a", "Tên", true), F("b", "Email"), F("c", "Chức danh")];
const entities = [{ name: "t", label: "Liên hệ" }, { name: "u", label: "Cơ hội" }] as EntityMeta[];
const autoLayout = { entityName: "t", views: { edit: { isDefault: true, sections: [{ title: "Chung", columns: 2, fields: ["a", "b"] }] }, detail: {}, list: {} } };
const savedLayout = { entityName: "t", views: { edit: { id: "L1", sections: [{ title: "Chung", columns: 2, fields: ["a", "b"] }] }, detail: {}, list: {} } };

function Host({ onPick = vi.fn() }: { onPick?: () => void }) {
  const [truong, setTruong] = useState<string | null>(null);
  return <ScreenEditor slug="northwind" entities={entities} bang="t" truong={truong} onTruong={setTruong} onPick={onPick} />;
}
const mount = (onPick?: () => void) =>
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <Host onPick={onPick} />
    </QueryClientProvider>,
  );
const cell = (label: string) => screen.findByRole("button", { name: new RegExp(`^Trường ${label},`) });
const dirtyIt = async () => {
  await userEvent.click(await cell("Email"));
  await userEvent.type(await screen.findByRole("textbox", { name: "Nhãn khách thấy" }), "X");
};

beforeEach(() => {
  Object.values(api).forEach((f) => f.mockReset());
  api.listFields.mockResolvedValue(fields);
  api.getLayout.mockResolvedValue(autoLayout);
  api.listRecords.mockResolvedValue([]);
  api.getTenantBrandingDoc.mockResolvedValue({ branding: {} });
  api.createLayout.mockResolvedValue({ id: "L9" });
  api.updateLayout.mockResolvedValue({ id: "L1" });
  api.deleteLayout.mockResolvedValue(undefined);
});

describe("ScreenEditor (specs/075 T016)", () => {
  it("loading shows a spinner, then the stage", async () => {
    mount();
    expect(document.querySelector(".slds-spinner")).not.toBeNull();
    expect(await cell("Tên")).toBeInTheDocument();
  });

  it("a read error shows the sentence and Thử lại", async () => {
    api.listFields.mockRejectedValueOnce(new ApiError(500, "X", "boom"));
    mount();
    expect(await screen.findByRole("button", { name: "Thử lại" })).toBeInTheDocument();
  });

  it("a table with no fields: empty state with a way to Data", async () => {
    api.listFields.mockResolvedValue([]);
    mount();
    expect(await screen.findByText("Bảng này chưa có trường")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Mở bước Dữ liệu" })).toBeInTheDocument();
  });

  it("first save POSTs the edit view; an auto-generated screen starts clean (Lưu off)", async () => {
    mount();
    await cell("Email");
    expect(screen.getByRole("button", { name: "Lưu thay đổi" })).toBeDisabled();
    await dirtyIt();
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(api.createLayout).toHaveBeenCalledTimes(1));
    expect(api.createLayout.mock.calls[0][0]).toMatchObject({ entityName: "t", viewType: "edit", isDefault: false });
    expect(api.updateLayout).not.toHaveBeenCalled();
  });

  it("later saves PUT /admin/layouts/:id", async () => {
    api.getLayout.mockResolvedValue(savedLayout);
    mount();
    await dirtyIt();
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(api.updateLayout).toHaveBeenCalledWith("L1", expect.objectContaining({ version: 1 })));
    expect(api.createLayout).not.toHaveBeenCalled();
  });

  it("PUT 404 LAYOUT_NOT_FOUND: says it was reset elsewhere, keeps the local edit, offers to save as a new screen", async () => {
    api.getLayout.mockResolvedValue(savedLayout);
    api.updateLayout.mockRejectedValue(new ApiError(404, "LAYOUT_NOT_FOUND", "not found"));
    mount();
    await dirtyIt();
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    expect(await screen.findByText(/Màn này đã được đưa về dạng tự sinh ở nơi khác/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^Trường X,/ })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Lưu lại như màn riêng mới" }));
    await waitFor(() => expect(api.createLayout).toHaveBeenCalledTimes(1));
  });

  it("422 LAYOUT_INVALID shows the business sentence, never the raw server text (FR-107)", async () => {
    api.createLayout.mockRejectedValue(new ApiError(422, "LAYOUT_INVALID", "layout.sections[0]: entity crm_x required_missing"));
    mount();
    await dirtyIt();
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    expect(await screen.findByText(/chưa lưu được/i)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain("required_missing");
  });

  it("503 TENANT_MAINTENANCE locks Lưu and says so", async () => {
    api.createLayout.mockRejectedValue(new ApiError(503, "TENANT_MAINTENANCE", "x", { retryAfter: 600 }));
    mount();
    await dirtyIt();
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    expect(await screen.findByText(/Đang khoá ghi đến \d{2}:\d{2}/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Lưu thay đổi" })).toBeDisabled();
  });

  it("removing a required field is refused before any server call", async () => {
    mount();
    await userEvent.click(await cell("Tên"));
    await userEvent.click(await screen.findByRole("button", { name: "Bỏ khỏi form" }));
    expect(screen.getByRole("alert")).toHaveTextContent("Trường bắt buộc phải nằm trên form");
    expect(api.createLayout).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Lưu thay đổi" })).toBeDisabled();
  });

  it("an optional field can leave the form and come back from 'Trường chưa đặt'", async () => {
    mount();
    await userEvent.click(await cell("Email"));
    await userEvent.click(await screen.findByRole("button", { name: "Bỏ khỏi form" }));
    expect(screen.queryByRole("button", { name: /^Trường Email,/ })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Đặt Email lên form" }));
    expect(await cell("Email")).toBeInTheDocument();
  });

  it("reset to auto-generated: inline confirm (0 dialogs), then DELETE /admin/layouts/:id; Esc cancels", async () => {
    api.getLayout.mockResolvedValue(savedLayout);
    mount();
    await cell("Tên");
    await userEvent.click(screen.getByRole("button", { name: "Thao tác của màn hình" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Đưa màn về màn tự sinh" }));
    expect(screen.getByText(/Đưa màn của bảng Liên hệ về màn tự sinh/)).toBeInTheDocument();
    expect(document.querySelectorAll('[role="dialog"]')).toHaveLength(0);
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByText(/Đưa màn của bảng Liên hệ về màn tự sinh/)).toBeNull();
    expect(api.deleteLayout).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Thao tác của màn hình" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Đưa màn về màn tự sinh" }));
    await userEvent.click(screen.getByRole("button", { name: "Đưa về màn tự sinh" }));
    await waitFor(() => expect(api.deleteLayout).toHaveBeenCalledWith("L1"));
  });

  it("changing screen with unsaved edits asks in place first; the select does not change; clean change goes straight", async () => {
    const onPick = vi.fn();
    mount(onPick);
    await cell("Tên");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Chọn màn hình" }), "u|form");
    expect(onPick).toHaveBeenCalledWith("u", "form");
    onPick.mockClear();
    await dirtyIt();
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Chọn màn hình" }), "u|danh-sach");
    expect(onPick).not.toHaveBeenCalled();
    expect(screen.getByText("Bỏ thay đổi chưa lưu và chuyển sang Danh sách Cơ hội?")).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Chọn màn hình" })).toHaveValue("t|form");
    await userEvent.click(screen.getByRole("button", { name: "Bỏ và chuyển" }));
    expect(onPick).toHaveBeenCalledWith("u", "danh-sach");
  });

  it("Bỏ thay đổi restores the saved state", async () => {
    mount();
    await dirtyIt();
    expect(screen.getByText("Chưa lưu")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Bỏ thay đổi" }));
    expect(screen.queryByText("Chưa lưu")).toBeNull();
  });
});

describe("ScreenEditor, trang hồ sơ (specs/075 T083)", () => {
  const detailAuto = { entityName: "t", views: { edit: {}, detail: { isDefault: true, sections: [{ title: "Chung", columns: 2, fields: ["a", "b", "c"] }] }, list: {} } };
  const rels = [{ id: "R1", name: "r1", parentEntityName: "t", childEntityName: "u", relationshipType: "1-N", viewConfig: { label: "Cơ hội của khách" } }];
  function RecordHost({ relId = null }: { relId?: string | null }) {
    const [truong, setTruong] = useState<string | null>(null);
    const [rel, setRel] = useState<string | null>(relId);
    return <ScreenEditor slug="northwind" entities={entities} bang="t" man="ho-so" truong={truong} onTruong={setTruong} relId={rel} onRel={setRel} onPick={vi.fn()} />;
  }
  const mountRecord = (relId: string | null = null) =>
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <RecordHost relId={relId} />
      </QueryClientProvider>,
    );
  beforeEach(() => {
    api.getLayout.mockResolvedValue(detailAuto);
    api.listAllRelationships.mockResolvedValue(rels);
  });

  it("man=ho-so mounts the record page canvas, not the form", async () => {
    mountRecord();
    expect(await screen.findByRole("tab", { name: "Quan hệ" })).toBeInTheDocument();
    expect(await cell("Email")).toBeInTheDocument();
  });

  it("saves the DETAIL view (viewType detail); cell properties have no Bắt buộc / Chỉ đọc", async () => {
    mountRecord();
    await userEvent.click(await cell("Email"));
    expect(screen.queryByText(/Bắt buộc nhập|Chỉ đọc/)).toBeNull();
    await userEvent.type(await screen.findByRole("textbox", { name: "Nhãn khách thấy" }), "X");
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(api.createLayout).toHaveBeenCalledTimes(1));
    expect(api.createLayout.mock.calls[0][0]).toMatchObject({ entityName: "t", viewType: "detail", isDefault: false });
  });

  it("a record page never blocks Lưu on required fields (they are not editable here)", async () => {
    mountRecord();
    await cell("Tên");
    expect(screen.queryByText(/bắt buộc nên phải nằm/)).toBeNull();
  });

  it("?lien-quan opens 'Cách hiện danh sách liên quan' in the right column", async () => {
    mountRecord("R1");
    expect(await screen.findByRole("heading", { name: "Cách hiện danh sách liên quan" })).toBeInTheDocument();
    expect(screen.getByLabelText("Tên mục")).toHaveValue("Cơ hội của khách");
  });
});
