// FieldAclPage (specs/074 lát f2-access-field-acl, T204): ?bang=&truong= pick the table and the row; a table or field
// that is not in the project says so with the way back; a table picked in the screen is written to the URL.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { logout } from "@/lib/api";

const replace = vi.fn();
vi.mock("next/navigation", () => ({
  usePathname: () => "/studio/du-an/acme/dung/nguoi-dung-quyen/quyen-truong",
  useRouter: () => ({ push: vi.fn(), replace, back: vi.fn() }),
}));
vi.mock("@/lib/studioContext", () => ({
  useStudioIdentity: () => ({ status: "ready", me: { isPlatformOperator: true, workspace: null }, home: "northwind", sessionReadable: true, matches: () => true, retry: vi.fn() }),
  homeSlug: () => "northwind",
}));

import { FieldAclPage } from "./FieldAclPage";

const jsonRes = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const ENTITIES = [{ name: "crm_accounts", label: "Khách hàng" }, { name: "crm_contacts", label: "Liên hệ" }];
const FIELDS = [{ name: "name", label: "Tên", fieldType: "text" }, { name: "cost", label: "Giá vốn", fieldType: "currency" }];
const routed = () =>
  vi.fn(async (url: RequestInfo | URL) => {
    const u = String(url);
    if (u.endsWith("/metadata/entities")) return jsonRes({ data: ENTITIES });
    if (u.includes("/admin/roles")) return jsonRes({ data: { roles: [{ id: "r1", name: "le_tan", label: "Lễ tân" }] } });
    if (u.includes("/metadata/fields/")) return jsonRes({ data: FIELDS });
    if (u.includes("/admin/field-acl")) return jsonRes({ data: { defaultPolicy: "allow", grants: [{ roleId: "r1", entityName: "crm_accounts", fieldName: "cost", canView: false, canEdit: false }] } });
    return jsonRes({ error: `unmocked ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
const view = (bang?: string, truong?: string) => (
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <FieldAclPage slug="acme" bang={bang} truong={truong} />
  </QueryClientProvider>
);

describe("FieldAclPage", () => {
  let originalFetch: typeof fetch;
  beforeEach(async () => {
    originalFetch = global.fetch;
    replace.mockReset();
    await logout();
    window.localStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
  });
  afterEach(() => {
    global.fetch = originalFetch;
    window.localStorage.clear();
  });

  it("?bang=&truong= preselect the table and light the field row, with the way back", async () => {
    global.fetch = routed() as unknown as typeof fetch;
    render(view("crm_accounts", "cost"));
    const cell = await screen.findByRole("radiogroup", { name: "Giá vốn · Lễ tân" });
    expect(cell.closest("tr")).toHaveClass("slds-is-selected");
    expect(screen.getByRole("link", { name: /Người dùng & quyền/ })).toHaveAttribute("href", expect.stringContaining("/dung/nguoi-dung-quyen"));
  });

  it("a table that is not in the project says so and links back, without loading the matrix", async () => {
    global.fetch = routed() as unknown as typeof fetch;
    render(view("crm_ghost"));
    expect(await screen.findByText("Không tìm thấy bảng crm_ghost")).toBeInTheDocument();
    expect(screen.getAllByRole("link", { name: /Người dùng & quyền/ }).length).toBeGreaterThan(0);
    expect(screen.queryByRole("radiogroup")).toBeNull();
  });

  it("a field that is not on the table says so too", async () => {
    global.fetch = routed() as unknown as typeof fetch;
    render(view("crm_accounts", "ghost"));
    expect(await screen.findByText("Không có trường này")).toBeInTheDocument();
  });

  it("picking another table writes ?bang= to the URL (replace, not push)", async () => {
    global.fetch = routed() as unknown as typeof fetch;
    render(view("crm_accounts"));
    await screen.findByRole("radiogroup", { name: "Giá vốn · Lễ tân" });
    await userEvent.click(screen.getByRole("combobox", { name: /Đối tượng/ }));
    await userEvent.click(screen.getByRole("option", { name: "Liên hệ" }));
    await waitFor(() => expect(replace).toHaveBeenCalledWith(expect.stringContaining("bang=crm_contacts"), expect.anything()));
    expect(replace.mock.calls[0][0]).not.toContain("truong=");
  });
});
