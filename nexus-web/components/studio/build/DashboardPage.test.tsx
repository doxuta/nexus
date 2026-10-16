// DashboardPage (specs/074 lát f2-dashboard-viewer-extract): `dung/bao-cao/tong-quan/<id>` — the customer's dashboard
// viewer inside the workshop. Loading, ok, not-found and error states.
import { act, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  runDashboard: vi.fn(),
  listFields: vi.fn(),
  listEntities: vi.fn(),
  getMe: vi.fn(),
  getCachedMe: vi.fn(),
  getPublishState: vi.fn(),
}));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));
vi.mock("@/lib/admin", async (orig) => ({ ...(await orig<typeof import("@/lib/admin")>()), listUsers: vi.fn().mockResolvedValue([]) }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { id: "u1", isAdmin: true } }) }));
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => "/studio/du-an/northwind/dung/bao-cao/tong-quan/d1",
}));

import { ApiError } from "@/lib/api";
import { DashboardPage } from "./DashboardPage";

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}><DashboardPage slug="northwind" id="d1" /></QueryClientProvider>);
}

beforeEach(() => {
  Object.values(api).forEach((f) => f.mockReset());
  api.listFields.mockResolvedValue([]);
  api.listEntities.mockResolvedValue([]);
  api.getMe.mockResolvedValue({ id: "u1", isAdmin: true });
  api.getCachedMe.mockReturnValue({ id: "u1", isAdmin: true });
  api.getPublishState.mockRejectedValue(new Error("403"));
});

describe("DashboardPage", () => {
  it("opens the dashboard with a link back to Báo cáo", async () => {
    api.runDashboard.mockResolvedValue({ dashboard: { id: "d1", name: "CRM Overview", description: "", widgets: [] }, widgets: [] });
    await act(async () => { renderPage(); });
    expect(await screen.findByRole("heading", { level: 1, name: /CRM Overview/ })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Báo cáo/ })).toHaveAttribute("href", "/studio/du-an/northwind/dung/bao-cao");
  });

  it("puts the authoring panel in a right-hand column (aside) next to the viewer", async () => {
    api.runDashboard.mockResolvedValue({ dashboard: { id: "d1", name: "CRM Overview", description: "", widgets: [] }, widgets: [] });
    await act(async () => { renderPage(); });
    await screen.findByRole("heading", { level: 1, name: /CRM Overview/ });
    expect(screen.getByRole("complementary", { name: "Soạn trang tổng quan" })).toBeInTheDocument();
  });

  it("says so when the dashboard does not exist (404)", async () => {
    api.runDashboard.mockRejectedValue(new ApiError(404, "NOT_FOUND", "not found"));
    await act(async () => { renderPage(); });
    expect(await screen.findByText("Không có trang tổng quan này", {}, { timeout: 4000 })).toBeInTheDocument();
  });

  it("shows the server error for any other failure", async () => {
    api.runDashboard.mockRejectedValue(new ApiError(500, "INTERNAL", "boom"));
    await act(async () => { renderPage(); });
    expect(await screen.findByText("boom", {}, { timeout: 4000 })).toBeInTheDocument();
  });
});
