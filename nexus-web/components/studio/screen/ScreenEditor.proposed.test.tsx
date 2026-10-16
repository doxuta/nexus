// specs/075 US9 (T211): the screen editor draws the assistant's proposal; Áp dụng/Bỏ are not here; no provider = nothing.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

const api = vi.hoisted(() => ({ listFields: vi.fn(), getLayout: vi.fn(), listRecords: vi.fn(), getTenantBrandingDoc: vi.fn() }));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("@/components/studio/usePublishState", () => ({ usePublishState: () => ({ data: {} }) }));
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }), usePathname: () => "/studio/du-an/northwind/dung/man-hinh/t" }));

import type { EntityMeta, FieldMeta } from "@/lib/api";
import { Off, WithGhost, mkChange } from "@/components/studio/assistant/ghostTestKit";
import { ScreenEditor } from "./ScreenEditor";

const F = (name: string, label: string): FieldMeta => ({ name, label, fieldType: "text" }) as FieldMeta;
const entities = [{ name: "t", label: "Lịch hẹn" }] as EntityMeta[];
const layout = { entityName: "t", views: { edit: { id: "L1", sections: [{ title: "Chung", columns: 2, fields: ["a", "b"] }] }, detail: {}, list: {} } };
const lay = (fields: unknown[]) => ({ entityName: "t", viewType: "edit", layout: { version: 1, sections: [{ title: "Chung", fields }] } });

function mount(ui?: Parameters<typeof WithGhost>[0]["change"]) {
  const ed = <ScreenEditor slug="northwind" entities={entities} bang="t" truong={null} onTruong={vi.fn()} onPick={vi.fn()} />;
  return render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>{ui === undefined ? ed : <WithGhost change={ui}>{ed}<Off /></WithGhost>}</QueryClientProvider>);
}
beforeEach(() => {
  Object.values(api).forEach((f) => f.mockReset());
  api.listFields.mockResolvedValue([F("a", "Tên"), F("b", "Email"), F("ngay", "Ngày tái khám")]);
  api.getLayout.mockResolvedValue(layout);
  api.listRecords.mockResolvedValue([]);
  api.getTenantBrandingDoc.mockResolvedValue({ branding: {} });
});

describe("ScreenEditor proposed overlay", () => {
  it("layout proposal: ghost for the added field and 'Sẽ bỏ' for the dropped one; stage cells stay", async () => {
    mount(mkChange({ docType: "layout", op: "update", label: "Sửa form", target: { docType: "layout", entity: "t", view: "edit" }, before: lay(["a", "b"]), after: lay(["a", "ngay"]) }));
    expect(await screen.findByRole("group", { name: "Đề xuất của trợ lý: Ngày tái khám" })).toBeInTheDocument();
    expect(screen.getByRole("group", { name: "Đề xuất của trợ lý: Sẽ bỏ khỏi màn: Email" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Áp dụng|^Bỏ$/ })).toBeNull();
  });
  it("another table's proposal is not drawn", async () => {
    mount(mkChange({ docType: "field", op: "create", label: "x", target: { docType: "field", entity: "khac", field: "z" }, after: { label: "Z" } }));
    await screen.findByRole("button", { name: /^Trường Tên,/ });
    expect(document.querySelector("[data-proposal-key]")).toBeNull();
  });
  it("no provider (hook F4 absent): no ghost", async () => {
    mount();
    await screen.findByRole("button", { name: /^Trường Tên,/ });
    expect(document.querySelector(".nx-proposed")).toBeNull();
  });
});
