// ReportsPage (specs/074 lát f2-report-list-view, T252): `dung/bao-cao` — list on the left, the real report on the right.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { AppSummary, DashboardSummary, ReportSummary } from "@/lib/api";

const replace = vi.fn();
vi.mock("next/navigation", () => ({
  usePathname: () => "/studio/du-an/northwind/dung/bao-cao",
  useRouter: () => ({ push: vi.fn(), replace, back: vi.fn(), forward: vi.fn(), refresh: vi.fn(), prefetch: vi.fn() }),
}));
vi.mock("@/components/screens/ReportViewer", () => ({
  ReportViewer: ({ id, openConfig }: { id: string; openConfig?: boolean }) => <div data-testid="viewer">{id}{openConfig ? " config" : ""}</div>,
}));
vi.mock("./../ReportCreateForm", () => ({
  ReportCreateForm: ({ onCreated }: { onCreated: (r: ReportSummary) => void }) => (
    <button onClick={() => onCreated({ id: "r9", name: "Mới" } as ReportSummary)}>fake create</button>
  ),
}));

const app = (name: string): AppSummary => ({ id: `id-${name}`, name, label: name.toUpperCase(), icon: null, color: null, isDefault: false, sortOrder: 0 });
const R: ReportSummary[] = [
  { id: "r1", name: "Hợp đồng", entityName: "crm_contracts", type: "tabular", isActive: true },
  { id: "r2", name: "Vé hỗ trợ", entityName: "tk_tickets", type: "summary", isActive: true },
];
const D: DashboardSummary[] = [{ id: "d1", name: "Bán hàng", widgets: [] }];
const appsCatalog = vi.fn();
const appNav = vi.fn();
const listAdminReports = vi.fn();
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  ensureAuth: vi.fn().mockResolvedValue(undefined),
  appsCatalog: (...a: unknown[]) => appsCatalog(...a),
  appNav: (...a: unknown[]) => appNav(...a),
  listAdminReports: (...a: unknown[]) => listAdminReports(...a),
  listAdminDashboards: vi.fn(async () => D),
}));

import { ReportsPage } from "./ReportsPage";

function renderPage(props: { bc?: string; bang?: string; app?: string } = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}><ReportsPage slug="northwind" {...props} /></QueryClientProvider>);
}

beforeEach(() => {
  vi.clearAllMocks();
  listAdminReports.mockResolvedValue(R);
  appsCatalog.mockResolvedValue([app("crm"), app("ticket")]);
  appNav.mockImplementation(async (n: string) => ({ app: app(n), items: [{ type: "entity", refId: n, label: n, href: n === "crm" ? "/crm_contracts" : "/tk_tickets" }] }));
});

describe("ReportsPage", () => {
  it("with nothing in the URL the first report of the list is the one drawn", async () => {
    renderPage();
    expect(await screen.findByTestId("viewer")).toHaveTextContent("r1");
    expect(screen.getByText("2 báo cáo, 1 trang tổng quan.")).toBeInTheDocument();
  });
  it("?bc= picks that report; the first of the CURRENT filter wins when ?bc= is outside it", async () => {
    const { unmount } = renderPage({ bc: "r2" });
    expect(await screen.findByTestId("viewer")).toHaveTextContent("r2");
    unmount();
    renderPage({ bc: "r2", app: "crm" });
    expect(await screen.findByTestId("viewer")).toHaveTextContent("r1");
  });
  it("nothing to show says so and offers the form", async () => {
    listAdminReports.mockResolvedValue([]);
    renderPage();
    expect((await screen.findAllByText("Chưa có báo cáo nào")).length).toBeGreaterThan(0);
    expect(screen.queryByTestId("viewer")).toBeNull();
  });
  it("a created report is selected through ?bc= and its config panel opens in place", async () => {
    renderPage();
    await screen.findByTestId("viewer");
    await userEvent.click(screen.getByRole("button", { name: "+ Báo cáo mới" }));
    await userEvent.click(screen.getByRole("button", { name: "fake create" }));
    expect(replace).toHaveBeenCalledWith("/studio/du-an/northwind/dung/bao-cao?bc=r9", undefined);
  });
});
