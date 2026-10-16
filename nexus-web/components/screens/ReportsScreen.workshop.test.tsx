// Queue #302 — Báo cáo + Bảng điều khiển inside an app's workshop: filtered to the app's tables, rows open
// in the workshop (?panel=…&report=/dashboard=) instead of /reports/<id>, "Xem tất cả" lifts the filter.
// A dashboard the builder's list cannot attribute stays visible (never hide what we cannot place).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ReportsScreen } from "./ReportsScreen";
import { DashboardsScreen } from "./DashboardsScreen";
import { WorkshopScopeProvider, type WorkshopScope } from "@/lib/workshopScope";
import {
  ApiError, getPublishState, listAdminDashboards, listDashboards, listReports,
  type DashboardCatalogItem, type DashboardSummary, type ReportSummary,
} from "@/lib/api";

const auth = vi.hoisted(() => ({ user: { id: "u1", fullName: "Admin", email: "admin@example.com", isAdmin: true } }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => auth }));
const nav = vi.hoisted(() => ({ push: vi.fn(), replace: vi.fn() }));
vi.mock("next/navigation", () => ({
  useRouter: () => nav,
  usePathname: () => "/studio/apps/crm",
  useSearchParams: () => new URLSearchParams("panel=bao-cao"),
}));
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  listReports: vi.fn(), listDashboards: vi.fn(), listAdminDashboards: vi.fn(), getPublishState: vi.fn(),
}));
vi.mock("@/lib/metadata/hooks", () => ({
  useEntities: () => ({ data: [
    { name: "crm_accounts", label: "Khách hàng" },
    { name: "lms_classes", label: "Lớp học" },
  ] }),
}));

const SCOPE: WorkshopScope = {
  slug: "crm", appLabel: "CRM", entityNames: new Set(["crm_accounts"]),
  panelHref: (p, extra) => `/studio/apps/crm?${new URLSearchParams({ panel: p, ...extra })}`,
};
function mount(ui: React.ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}><WorkshopScopeProvider value={SCOPE}>{ui}</WorkshopScopeProvider></QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(getPublishState).mockRejectedValue(new ApiError(403, "FORBIDDEN", "builder only", {}));
});

describe("ReportsScreen inside an app workshop (#302)", () => {
  const REPORTS: ReportSummary[] = [
    { id: "r1", name: "Khách theo vùng", entityName: "crm_accounts", type: "summary", isActive: true },
    { id: "r2", name: "Điểm danh lớp", entityName: "lms_classes", type: "tabular", isActive: true },
  ];
  it("shows only the app's reports, links them into the workshop, and 'Xem tất cả' brings the rest", async () => {
    vi.mocked(listReports).mockResolvedValue(REPORTS);
    mount(<ReportsScreen />);
    const grid = await screen.findByRole("grid", { name: "Danh sách báo cáo" });
    const link = await within(grid).findByRole("link", { name: "Khách theo vùng" });
    expect(link).toHaveAttribute("href", "/studio/apps/crm?panel=bao-cao&report=r1");
    expect(within(grid).queryByText("Điểm danh lớp")).not.toBeInTheDocument();
    expect(screen.getByTestId("scope-hidden-note")).toHaveTextContent("1 báo cáo của app khác đang ẩn.");

    await userEvent.click(screen.getByRole("button", { name: "Xem tất cả" }));
    expect(within(grid).getByText("Điểm danh lớp")).toBeInTheDocument();
  });
});

describe("DashboardsScreen inside an app workshop (#302)", () => {
  const CATALOG: DashboardCatalogItem[] = [
    { id: "d1", name: "Bảng CRM", widgets: [{ id: "w1", type: "bar" }] },
    { id: "d2", name: "Bảng Lớp học", widgets: [{ id: "w2", type: "bar" }] },
    { id: "d3", name: "Bảng chưa rõ", widgets: [] },
  ];
  const widget = (id: string, entity: string) => ({ id, title: id, type: "bar", entity, metric: "count" });
  const ADMIN: DashboardSummary[] = [
    { id: "d1", name: "Bảng CRM", widgets: [widget("w1", "crm_accounts")] },
    { id: "d2", name: "Bảng Lớp học", widgets: [widget("w2", "lms_classes")] },
    { id: "d3", name: "Bảng chưa rõ", widgets: [] },
  ];
  it("filters by the tables the widgets read; unattributable stays; links stay in the workshop", async () => {
    vi.mocked(listDashboards).mockResolvedValue(CATALOG);
    vi.mocked(listAdminDashboards).mockResolvedValue(ADMIN);
    mount(<DashboardsScreen />);
    const crm = await screen.findByRole("link", { name: "Bảng CRM" });
    expect(crm).toHaveAttribute("href", "/studio/apps/crm?panel=dashboard&dashboard=d1");
    await waitFor(() => expect(screen.queryByRole("link", { name: "Bảng Lớp học" })).not.toBeInTheDocument());
    expect(screen.getByRole("link", { name: "Bảng chưa rõ" })).toBeInTheDocument();
    expect(screen.getByTestId("scope-hidden-note")).toHaveTextContent("1 bảng điều khiển của app khác đang ẩn.");
  });
});
