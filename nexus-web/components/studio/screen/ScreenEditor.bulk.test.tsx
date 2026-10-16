// specs/075 L3 T121-T123: the list mode edits the SAME local draft as the stage, saves through the same path, and says what it skipped.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { useState } from "react";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

const calls: string[] = [];
const api = vi.hoisted(() => ({
  listFields: vi.fn(), getLayout: vi.fn(), listRecords: vi.fn(), createLayout: vi.fn(), updateLayout: vi.fn(), deleteLayout: vi.fn(),
  getTenantBrandingDoc: vi.fn(), updateField: vi.fn(),
}));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import type { EntityMeta, FieldMeta } from "@/lib/api";
import { ScreenEditor } from "./ScreenEditor";

const F = (name: string, label: string, isRequired = false): FieldMeta => ({ id: `id-${name}`, name, label, fieldType: "text", isRequired }) as FieldMeta;
const fields = [F("a", "Tên", true), F("b", "Email"), F("c", "Chức danh"), F("d", "Ghi chú")];
const entities = [{ name: "t", label: "Liên hệ" }] as EntityMeta[];
const savedLayout = {
  entityName: "t",
  views: {
    edit: { id: "L1", sections: [{ title: "Chung", columns: 2, fields: ["a", "b"] }, { title: "Thêm", columns: 2, fields: ["c"] }] },
    detail: {}, list: {},
  },
};

function Host({ man = "form" as "form" | "ho-so" }) {
  const [list, setList] = useState(true);
  const [truong, setTruong] = useState<string | null>(null);
  return <ScreenEditor slug="northwind" entities={entities} bang="t" man={man} truong={truong} onTruong={setTruong} onPick={vi.fn()} listMode={list} onMode={setList} />;
}
const mount = (man: "form" | "ho-so" = "form") =>
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <Host man={man} />
    </QueryClientProvider>,
  );
const rowOf = (name: RegExp | string) => screen.getAllByRole("row").find((r) => within(r).queryByText(name))!;
const tick = async (name: string) => userEvent.click(within(rowOf(name)).getAllByRole("checkbox")[0]);

beforeEach(() => {
  calls.length = 0;
  Object.values(api).forEach((f) => f.mockReset());
  api.listFields.mockResolvedValue(fields);
  api.getLayout.mockResolvedValue(savedLayout);
  api.listRecords.mockResolvedValue([]);
  api.getTenantBrandingDoc.mockResolvedValue({ branding: {} });
  api.updateLayout.mockImplementation(async () => { calls.push("layout"); return { id: "L1" }; });
  api.updateField.mockImplementation(async (id: string, p: { isRequired: boolean }) => { calls.push(`${id}:${p.isRequired}`); return {}; });
});

describe("ScreenEditor list mode (specs/075 L3)", () => {
  it("shows the hint, then the bar once rows are ticked", async () => {
    mount();
    expect(await screen.findByText("Tick nhiều trường để sửa cùng lúc.")).toBeInTheDocument();
    await tick("Email");
    await tick("Ghi chú");
    expect(screen.getByText("Đã chọn 2")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Đặt bắt buộc" })).toBeInTheDocument();
  });

  it("'Bỏ khỏi form' skips a required field out loud and removes the rest", async () => {
    mount();
    await screen.findByText("Tick nhiều trường để sửa cùng lúc.");
    await tick("Tên");
    await tick("Email");
    await userEvent.click(screen.getByRole("button", { name: "Bỏ khỏi form" }));
    expect(screen.getByText(/Đã bỏ 1 trường khỏi form\. Bỏ qua 1 trường bắt buộc: Tên\./)).toBeInTheDocument();
    expect(within(rowOf("Email")).getByRole("combobox")).toHaveValue("");
    expect(within(rowOf("Tên")).getByRole("combobox")).not.toHaveValue("");
  });

  it("when every ticked field is skipped, only the skip is told (no 'Đã bỏ 0')", async () => {
    mount();
    await screen.findByText("Tick nhiều trường để sửa cùng lúc.");
    await tick("Tên");
    await userEvent.click(screen.getByRole("button", { name: "Bỏ khỏi form" }));
    expect(screen.getByText("Bỏ qua 1 trường bắt buộc: Tên.")).toBeInTheDocument();
  });

  it("the row switch and select edit at once, the mode switch keeps the draft, and Lưu writes field then layout once", async () => {
    mount();
    await screen.findByText("Tick nhiều trường để sửa cùng lúc.");
    await userEvent.click(within(rowOf("Ghi chú")).getByRole("switch"));
    await userEvent.selectOptions(within(rowOf("Ghi chú")).getByRole("combobox"), "Thêm");
    await userEvent.click(screen.getByRole("button", { name: "Trên màn" }));
    expect(await screen.findByRole("button", { name: /^Trường Ghi chú,/ })).toBeInTheDocument(); // same draft on the stage
    await userEvent.click(screen.getByRole("button", { name: "Danh sách" }));
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(calls).toEqual(["layout", "id-d:true"]));
  });

  it("a partly failed save keeps the row it could not write", async () => {
    api.updateField.mockImplementation(async (id: string) => { if (id === "id-d") throw new Error("boom"); calls.push(id); return {}; });
    mount();
    await screen.findByText("Tick nhiều trường để sửa cùng lúc.");
    await tick("Email");
    await tick("Ghi chú");
    await userEvent.click(screen.getByRole("button", { name: "Đặt bắt buộc" }));
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(screen.getByText(/Đã đặt bắt buộc 1 trên 2 trường/)).toBeInTheDocument());
    expect(within(rowOf("Ghi chú")).getByRole("switch")).toBeChecked();
  });

  it("the record page has no 'Đặt bắt buộc' and no required column", async () => {
    mount("ho-so");
    await screen.findByText("Tick nhiều trường để sửa cùng lúc.");
    await tick("Email");
    expect(screen.queryByRole("button", { name: "Đặt bắt buộc" })).toBeNull();
    expect(screen.queryByRole("switch")).toBeNull();
  });
});
