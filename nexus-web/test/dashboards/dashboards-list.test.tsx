// /dashboards on SLDS (specs/038 T029) — object-home page header + tiles + modal + illustration.
// Pins what the catalog must still let each persona do: find a dashboard (search on name and
// description), open it, see its widget count and shapes; an admin creates one (name → viewer),
// a user without the admin flag gets no create path and is told to ask an admin.
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ listDashboards: vi.fn(), createDashboard: vi.fn(), getPublishState: vi.fn() }));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));

const auth = vi.hoisted(() => ({ user: { id: "u1", isAdmin: false } as { id: string; isAdmin: boolean } }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => auth }));

const router = vi.hoisted(() => ({ push: vi.fn(), replace: vi.fn() }));
vi.mock("next/navigation", () => ({
  useRouter: () => router,
  usePathname: () => "/dashboards",
  useSearchParams: () => new URLSearchParams(),
}));

import DashboardsPage from "@/app/(app)/dashboards/page";

const CATALOG = [
  { id: "d1", name: "CRM Overview", description: "Pipeline funnel và leads theo trạng thái.", widgets: [
    { id: "w1", type: "bar" }, { id: "w2", type: "kpi" }, { id: "w3", type: "bar" }, { id: "w4", type: "line" }, { id: "w5", type: "table" },
  ] },
  { id: "d2", name: "Tổng quan Ticketing", description: "Tổng số phiếu theo ưu tiên.", widgets: [{ id: "w1", type: "kpi" }] },
];

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <DashboardsPage />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  api.listDashboards.mockReset().mockResolvedValue(CATALOG);
  api.createDashboard.mockReset();
  api.getPublishState.mockReset().mockRejectedValue(new Error("403")); // not the builder
  router.push.mockReset();
  auth.user = { id: "u1", isAdmin: false };
});

describe("/dashboards — catalog", () => {
  it("is an SLDS object home: header, count, one tile per dashboard linking to its viewer", async () => {
    renderPage();
    const tile = await screen.findByRole("link", { name: "CRM Overview" });
    expect(tile).toHaveAttribute("href", "/dashboards/d1");
    expect(document.querySelector(".slds-page-header")).not.toBeNull();
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Tất cả trang tổng quan");
    expect(screen.getByText(/^2 trang tổng quan ·/)).toBeInTheDocument();
    const card = tile.closest(".slds-tile") as HTMLElement;
    expect(card).not.toBeNull();
    expect(within(card).getByText("Pipeline funnel và leads theo trạng thái.")).toBeInTheDocument();
    expect(within(card).getByText("5")).toBeInTheDocument(); // widget count
    // First four shapes, then "+1" for the rest (same preview as before).
    expect(within(card).getAllByText("bar")).toHaveLength(2);
    expect(within(card).getByText("+1")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Tổng quan Ticketing" })).toHaveAttribute("href", "/dashboards/d2");
  });

  it("filters by name and by description", async () => {
    renderPage();
    await screen.findByRole("link", { name: "CRM Overview" });
    const search = screen.getByPlaceholderText("Tìm trang tổng quan…");
    fireEvent.change(search, { target: { value: "ưu tiên" } });
    expect(screen.queryByRole("link", { name: "CRM Overview" })).toBeNull();
    expect(screen.getByRole("link", { name: "Tổng quan Ticketing" })).toBeInTheDocument();
    fireEvent.change(search, { target: { value: "không có" } });
    expect(screen.getByRole("heading", { name: "Không tìm thấy kết quả" })).toBeInTheDocument();
  });

  it("says why the list failed to load", async () => {
    api.listDashboards.mockRejectedValue(new Error("Mất kết nối máy chủ"));
    renderPage();
    expect(await screen.findByText("Mất kết nối máy chủ")).toBeInTheDocument();
    expect(document.querySelector(".slds-scoped-notification.slds-theme_error")).not.toBeNull();
  });
});

describe("/dashboards — roles", () => {
  it("a non-admin has no create path and is told to ask an admin", async () => {
    api.listDashboards.mockResolvedValue([]);
    renderPage();
    expect(await screen.findByRole("heading", { name: "Chưa có trang tổng quan nào" })).toBeInTheDocument();
    expect(screen.getByText("Liên hệ quản trị viên để được cấp trang tổng quan.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Tạo trang tổng quan" })).toBeNull();
  });

  it("an admin names a new dashboard in a modal and lands in its viewer", async () => {
    auth.user = { id: "a1", isAdmin: true };
    api.createDashboard.mockResolvedValue({ id: "d9", name: "Tổng quan kinh doanh", widgets: [] });
    renderPage();
    await screen.findByRole("link", { name: "CRM Overview" });
    fireEvent.click(screen.getByRole("button", { name: "Tạo trang tổng quan" }));
    const dialog = screen.getByRole("dialog", { name: "Tạo trang tổng quan" });
    const submit = within(dialog).getByRole("button", { name: "Tạo" });
    expect(submit).toBeDisabled(); // no name yet
    fireEvent.change(within(dialog).getByLabelText(/Tên/), { target: { value: "  Tổng quan kinh doanh " } });
    await act(async () => {
      fireEvent.click(submit);
    });
    expect(api.createDashboard).toHaveBeenCalledWith("Tổng quan kinh doanh", []);
    expect(router.push).toHaveBeenCalledWith("/dashboards/d9");
  });

  it("an admin's empty catalog offers the create button", async () => {
    auth.user = { id: "a1", isAdmin: true };
    api.listDashboards.mockResolvedValue([]);
    renderPage();
    expect(await screen.findByText("Tạo trang tổng quan đầu tiên rồi lắp ô số liệu ngay trong trang xem.")).toBeInTheDocument();
    // Header action + the empty state's own call to action.
    expect(screen.getAllByRole("button", { name: "Tạo trang tổng quan" })).toHaveLength(2);
  });
});
