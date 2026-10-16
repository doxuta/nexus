import { beforeEach, describe, expect, it, vi } from "vitest";
import { useState } from "react";
import { render, screen, waitFor } from "@testing-library/react";
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
const fields = [F("a", "Tên", true), F("b", "Email"), F("c", "Chức danh")];
const entities = [{ name: "t", label: "Liên hệ" }] as EntityMeta[];
const savedLayout = { entityName: "t", views: { edit: { id: "L1", sections: [{ title: "Chung", columns: 2, fields: ["a", "b"] }] }, detail: {}, list: {} } };

function Host({ start = null }: { start?: string | null }) {
  const [truong, setTruong] = useState<string | null>(start);
  return <ScreenEditor slug="northwind" entities={entities} bang="t" truong={truong} onTruong={setTruong} onPick={vi.fn()} />;
}
const mount = (start: string | null = null) =>
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <Host start={start} />
    </QueryClientProvider>,
  );

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

describe("ScreenEditor: Bắt buộc nhập (specs/075 L1c)", () => {
  it("turning it on for a field that is off the form puts it on the form and saves layout then field", async () => {
    mount("c"); // ?truong=c: a field that is not on the form yet
    await userEvent.click(await screen.findByRole("switch", { name: /Bắt buộc nhập/ }));
    expect(await screen.findByRole("button", { name: /^Trường Chức danh,/ })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(calls).toEqual(["layout", "id-c:true"]));
    const sent = api.updateLayout.mock.calls[0][1] as { sections: Array<{ fields: unknown[] }> };
    expect(JSON.stringify(sent)).toContain('"c"');
  });

  it("turning it off first, then removing the field: field write comes BEFORE the layout (no 422)", async () => {
    mount();
    await userEvent.click(await screen.findByRole("button", { name: /^Trường Tên,/ }));
    await userEvent.click(await screen.findByRole("switch", { name: /Bắt buộc nhập/ }));
    await userEvent.click(screen.getByRole("button", { name: "Bỏ khỏi form" }));
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(calls).toEqual(["id-a:false", "layout"]));
  });
});
