// specs/075 T191: draft labels on the form editor — none before delivery; from the read's draftState after.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { useState } from "react";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

const api = vi.hoisted(() => ({
  listFields: vi.fn(), getLayout: vi.fn(), listRecords: vi.fn(), createLayout: vi.fn(), updateLayout: vi.fn(), deleteLayout: vi.fn(),
  getTenantBrandingDoc: vi.fn(),
}));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
const publish = vi.hoisted(() => ({ state: {} as Record<string, unknown> }));
vi.mock("@/components/studio/usePublishState", () => ({ usePublishState: () => ({ data: publish.state }) }));

import type { EntityMeta, FieldMeta } from "@/lib/api";
import { ScreenEditor } from "./ScreenEditor";

const F = (name: string, label: string, extra: Partial<FieldMeta> = {}): FieldMeta => ({ name, label, fieldType: "text", ...extra }) as FieldMeta;
const entities = [{ name: "t", label: "Liên hệ" }] as EntityMeta[];
const layout = (draftState?: string) => ({ entityName: "t", views: { edit: { id: "L1", draftState, sections: [{ title: "Chung", columns: 2, fields: ["a"] }] }, detail: {}, list: {} } });
function Host({ initial = null }: { initial?: string | null }) {
  const [truong, setTruong] = useState<string | null>(initial);
  return <ScreenEditor slug="northwind" entities={entities} bang="t" truong={truong} onTruong={setTruong} onPick={vi.fn()} />;
}
const mount = (initial?: string | null) =>
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><Host initial={initial} /></QueryClientProvider>);

beforeEach(() => {
  Object.values(api).forEach((f) => f.mockReset());
  api.listRecords.mockResolvedValue([]);
  api.getTenantBrandingDoc.mockResolvedValue({ branding: {} });
  publish.state = { delivered: true, draftableTypes: ["layout", "field"], directTypes: [] };
});

describe("ScreenEditor draft labels (specs/075 T191)", () => {
  it("not delivered: zero draft labels", async () => {
    publish.state = { delivered: false, draftableTypes: ["layout"], directTypes: [] };
    api.listFields.mockResolvedValue([F("a", "Tên"), F("b", "Email", { draftState: "new" })]);
    api.getLayout.mockResolvedValue(layout());
    mount();
    await screen.findByRole("button", { name: /^Trường Tên,/ });
    expect(screen.queryByText("Nháp")).toBeNull();
  });

  it("delivered + saved screen the read marks edited: 'Nháp' beside Lưu", async () => {
    api.listFields.mockResolvedValue([F("a", "Tên")]);
    api.getLayout.mockResolvedValue(layout("edited"));
    mount();
    expect(await screen.findByText("Nháp")).toBeInTheDocument();
  });

  it("delivered: a new field stays in 'Trường chưa đặt' with its label; pending-required shows its sentence", async () => {
    api.listFields.mockResolvedValue([F("a", "Tên", { pendingRequired: true }), F("b", "Email", { draftState: "new" })]);
    api.getLayout.mockResolvedValue(layout());
    mount("a");
    expect(await screen.findByText("Bắt buộc sẽ bật khi xuất bản")).toBeInTheDocument();
    expect(screen.getByText("Email")).toBeInTheDocument();
    expect(screen.getByText("Mới, khách thấy khi xuất bản")).toBeInTheDocument();
  });

  it("a layout classed direct asks in place before writing", async () => {
    publish.state = { delivered: true, draftableTypes: [], directTypes: ["layout"] };
    api.listFields.mockResolvedValue([F("a", "Tên"), F("b", "Email")]);
    api.getLayout.mockResolvedValue(layout());
    api.updateLayout.mockResolvedValue({ id: "L1" });
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /^Trường Tên,/ }));
    await userEvent.type(await screen.findByRole("textbox", { name: "Nhãn khách thấy" }), "X");
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    expect(api.updateLayout).not.toHaveBeenCalled();
    await userEvent.click(await screen.findByRole("button", { name: "Lưu và áp ngay" }));
    expect(api.updateLayout).toHaveBeenCalled();
  });

  it("'Xem bản đang chạy': delivered only; Lưu locked while live, back returns to the draft", async () => {
    api.listFields.mockResolvedValue([F("a", "Tên")]);
    api.getLayout.mockResolvedValue(layout("edited"));
    mount();
    await userEvent.click(await screen.findByRole("button", { name: "Xem bản đang chạy" }));
    expect(await screen.findByText(/Đây là bản khách đang dùng/)).toBeInTheDocument();
    expect(screen.queryByText("Nháp")).toBeNull();
    expect(screen.getByRole("button", { name: "Lưu thay đổi" })).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Quay lại bản nháp" }));
    expect(await screen.findByText("Nháp")).toBeInTheDocument();
  });

  it("live view of an auto-generated screen: no 'Lưu để dựng màn riêng' sentence while Lưu is locked (#800)", async () => {
    api.listFields.mockResolvedValue([F("a", "Tên")]);
    api.getLayout.mockResolvedValue({ entityName: "t", views: { edit: { isDefault: true, sections: [{ title: "Chung", columns: 2, fields: ["a"] }] }, detail: {}, list: {} } });
    mount();
    expect(await screen.findByText(/Đang là màn tự sinh/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Xem bản đang chạy" }));
    expect(await screen.findByText(/Đây là bản khách đang dùng/)).toBeInTheDocument();
    expect(screen.queryByText(/Đang là màn tự sinh/)).toBeNull();
  });

  it("not delivered: no 'Xem bản đang chạy'", async () => {
    publish.state = { delivered: false, draftableTypes: ["layout"], directTypes: [] };
    api.listFields.mockResolvedValue([F("a", "Tên")]);
    api.getLayout.mockResolvedValue(layout());
    mount();
    await screen.findByRole("button", { name: /^Trường Tên,/ });
    expect(screen.queryByRole("button", { name: "Xem bản đang chạy" })).toBeNull();
  });
});
