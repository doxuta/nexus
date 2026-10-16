// BrandingPage: the frame's tabs are the first tables of the real customer app (appNav), not invented labels.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrandingPage } from "./BrandingPage";
import { ensureAuth, getTenantBrandingDoc, appsCatalog, appNav } from "@/lib/api";

vi.mock("@/components/ui/Toaster", () => ({ toast: { info: vi.fn(), success: vi.fn(), error: vi.fn(), warning: vi.fn() } }));
vi.mock("@/lib/studioContext", () => ({ useStudioIdentity: () => ({ matches: () => true }) }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, ensureAuth: vi.fn(), getTenantBrandingDoc: vi.fn(), appsCatalog: vi.fn(), appNav: vi.fn() };
});

beforeEach(() => {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(getTenantBrandingDoc).mockReset().mockResolvedValue({ branding: { appName: "ABC" } });
  vi.mocked(appsCatalog).mockReset().mockResolvedValue([{ name: "crm" }] as never);
  vi.mocked(appNav).mockReset().mockResolvedValue({
    app: {} as never,
    items: [
      { type: "entity", refId: "1", label: "Khách hàng", href: "/crm_accounts" },
      { type: "dashboard", refId: "2", label: "Tổng quan", href: "/dashboards/2" },
      { type: "entity", refId: "3", label: "Cơ hội", href: "/crm_opps" },
    ],
  });
});

describe("BrandingPage", () => {
  it("draws the real app's first tables as tabs in the customer frame", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={qc}><BrandingPage slug="p1" /></QueryClientProvider>);
    // the form remounts when the saved document lands, so the frame is re-queried inside waitFor
    const frame = () => screen.getByTestId("customer-preview-frame");
    await waitFor(() => expect(frame().querySelectorAll("[role=tab]").length).toBe(2));
    expect(frame().textContent).toContain("Khách hàng");
    expect(frame().textContent).toContain("Cơ hội");
    expect(frame().textContent).not.toContain("Tổng quan");
    expect(appNav).toHaveBeenCalledWith("crm");
  });
});
