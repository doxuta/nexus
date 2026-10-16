// Queue #302 — FieldACL inside an app's workshop: the table picker offers only the app's tables (plus a
// deep-linked one), and a nav reload that yields the same tables must not reset the picked table.
// Standalone behaviour: FieldACL.test.tsx.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { FieldACL } from "@/components/screens/admin/FieldACL";
import { WorkshopScopeProvider, type WorkshopScope } from "@/lib/workshopScope";
import { logout } from "@/lib/api";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;
const jsonRes = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const ENTITIES = [
  { name: "crm_accounts", label: "Khách hàng" },
  { name: "crm_contacts", label: "Liên hệ" },
  { name: "lms_classes", label: "Lớp học" },
];
const FIELDS = [{ name: "name", label: "Tên", fieldType: "text" }];
const scope = (): WorkshopScope => ({
  slug: "crm", appLabel: "CRM", entityNames: new Set(["crm_accounts", "crm_contacts"]),
  panelHref: (p) => `/studio/apps/crm?panel=${p}`,
});

describe("FieldACL inside an app workshop (#302)", () => {
  let originalFetch: FetchFn;
  let entityCalls = 0;
  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    entityCalls = 0;
    await logout();
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
    global.fetch = vi.fn(async (url: RequestInfo | URL) => {
      const u = String(url);
      if (u.includes("/metadata/entities")) { entityCalls++; return jsonRes({ data: ENTITIES }); }
      if (u.includes("/admin/roles")) return jsonRes({ data: { roles: [{ id: "r1", name: "sales", label: "Kinh doanh" }] } });
      if (u.includes("/metadata/fields/")) return jsonRes({ data: FIELDS });
      if (u.includes("/admin/field-acl")) return jsonRes({ data: { defaultPolicy: "allow", grants: [], deadEndIfLocked: [] } });
      return jsonRes({ error: `unmocked ${u}`, code: "TEST_UNMOCKED" }, 500);
    }) as unknown as typeof fetch;
  });
  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.history.replaceState(null, "", "/");
    window.localStorage.clear();
  });

  it("offers only the app's tables in the picker", async () => {
    render(<WorkshopScopeProvider value={scope()}><FieldACL /></WorkshopScopeProvider>);
    await screen.findByRole("radiogroup", { name: "Tên · Kinh doanh" });
    await userEvent.click(screen.getByRole("combobox", { name: /Đối tượng/ }));
    expect(screen.getByRole("option", { name: "Liên hệ" })).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: "Lớp học" })).not.toBeInTheDocument();
  });

  it("still preselects a deep-linked table that is outside the app", async () => {
    window.history.replaceState(null, "", "/?entity=lms_classes");
    render(<WorkshopScopeProvider value={scope()}><FieldACL /></WorkshopScopeProvider>);
    await screen.findByRole("radiogroup", { name: "Tên · Kinh doanh" });
    expect(screen.getByRole("combobox", { name: /Đối tượng/ })).toHaveTextContent("Lớp học");
  });

  it("a new Set with the same tables does not reload or reset the picked table", async () => {
    const { rerender } = render(<WorkshopScopeProvider value={scope()}><FieldACL /></WorkshopScopeProvider>);
    await screen.findByRole("radiogroup", { name: "Tên · Kinh doanh" });
    await userEvent.click(screen.getByRole("combobox", { name: /Đối tượng/ }));
    await userEvent.click(screen.getByRole("option", { name: "Liên hệ" }));
    await waitFor(() => expect(screen.getByRole("combobox", { name: /Đối tượng/ })).toHaveTextContent("Liên hệ"));
    rerender(<WorkshopScopeProvider value={scope()}><FieldACL /></WorkshopScopeProvider>);
    await new Promise((r) => setTimeout(r, 50));
    expect(entityCalls).toBe(1);
    expect(screen.getByRole("combobox", { name: /Đối tượng/ })).toHaveTextContent("Liên hệ");
  });
});
