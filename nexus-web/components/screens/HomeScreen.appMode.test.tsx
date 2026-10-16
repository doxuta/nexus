// HomeScreen inside an app (queue #289): the app's chosen home dashboard leads the page, and the
// work cards show ONLY the app's own work — a clinic app must not list the CRM's "Lead mới" queue.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { HomeScreen } from "./HomeScreen";
import { t } from "@/lib/i18n";
import {
  listRecordsPage, listDashboards, listPendingApprovals, listWorkQueues, runDashboard,
  type ApprovalItem, type WorkQueueView,
} from "@/lib/api";

vi.mock("@/lib/auth/AuthProvider", () => ({
  useAuth: () => ({ user: { id: "u1", fullName: "Lễ Tân", email: "test@example.com", isAdmin: false } }),
}));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    listRecordsPage: vi.fn(), listDashboards: vi.fn(), listPendingApprovals: vi.fn(),
    listWorkQueues: vi.fn(), runDashboard: vi.fn(),
  };
});

const meta = vi.hoisted(() => ({ entities: [{ name: "appointments", label: "Lịch hẹn" }], fields: [{ name: "name", label: "Tên", fieldType: "text" }] }));
vi.mock("@/lib/metadata/hooks", () => ({
  useEntities: () => ({ data: meta.entities }),
  useFields: () => ({ data: meta.fields, isLoading: false }),
}));

const active = vi.hoisted(() => ({
  value: {
    activeApp: "clinic",
    activeMeta: { id: "a", name: "clinic", label: "Phòng khám", homeDashboardId: "dash-1" },
    activeNav: [{ type: "entity", refId: "e1", label: "Lịch hẹn", href: "/appointments" }],
  } as Record<string, unknown>,
}));
vi.mock("@/lib/apps", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/apps")>();
  return { ...actual, useActiveApp: () => active.value };
});
vi.mock("@/lib/pins", () => ({ usePins: () => ({ pins: [], toggle: vi.fn() }) }));
vi.mock("@/lib/recents", () => ({ pruneRecentRecords: () => [], onRecentsChanged: () => () => undefined }));

const queue = (id: string, label: string, entityName: string): WorkQueueView =>
  ({ id, name: id, label, entityName, actionVerb: "open", count: 3, href: `/${entityName}?queue=${id}` });

beforeEach(() => {
  active.value = {
    activeApp: "clinic",
    activeMeta: { id: "a", name: "clinic", label: "Phòng khám", homeDashboardId: "dash-1" },
    activeNav: [{ type: "entity", refId: "e1", label: "Lịch hẹn", href: "/appointments" }],
  };
  vi.mocked(runDashboard).mockClear();
  vi.mocked(listRecordsPage).mockResolvedValue({ records: [], pagination: { page: 1, limit: 1, total: 4, totalPages: 4 } });
  vi.mocked(listDashboards).mockResolvedValue([]);
  vi.mocked(listPendingApprovals).mockResolvedValue([
    { ID: "ap1", TenantID: "t", EntityName: "crm_quotes", RecordID: "r1", CurrentStep: 0, Status: "pending" } as ApprovalItem,
  ]);
  vi.mocked(listWorkQueues).mockResolvedValue([
    queue("q1", "Lead mới chưa xử lý", "crm_leads"),
    queue("q2", "Lịch hẹn hôm nay", "appointments"),
  ]);
  vi.mocked(runDashboard).mockResolvedValue({
    dashboard: { id: "dash-1", name: "Lịch phòng khám", widgets: [{ id: "w1", title: "Hẹn hôm nay", type: "kpi", entity: "appointments", metric: "count" }] },
    widgets: [{ widgetId: "w1", title: "Hẹn hôm nay", type: "kpi", result: { count: 7, grouped: false, value: "7" } }],
  });
});

function renderHome() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}><HomeScreen /></QueryClientProvider>);
}

describe("HomeScreen in an app", () => {
  it("leads with the app's home dashboard", async () => {
    renderHome();
    expect(await screen.findByText("Lịch phòng khám")).toBeInTheDocument();
    expect(await screen.findByText("Hẹn hôm nay")).toBeInTheDocument();
    expect(runDashboard).toHaveBeenCalledWith("dash-1");
    expect(screen.getByRole("link", { name: /Mở bảng điều khiển/ })).toHaveAttribute("href", "/dashboards/dash-1");
  });

  it("shows only the app's own queues and approvals, not other apps' work", async () => {
    renderHome();
    expect(await screen.findByText("Lịch hẹn hôm nay")).toBeInTheDocument();
    expect(screen.queryByText("Lead mới chưa xử lý")).toBeNull();
    // the only pending approval belongs to crm_quotes, which is not in this app's nav
    expect(screen.queryByText(/crm_quotes/)).toBeNull();
  });

  it("builder's explicit queue pick wins over the guess from the menu tables (#483)", async () => {
    active.value = { ...active.value, activeMeta: { id: "a", name: "clinic", label: "Phòng khám", homeQueueIds: ["q1"] } };
    renderHome();
    expect(await screen.findByText("Lead mới chưa xử lý")).toBeInTheDocument();
    expect(screen.queryByText("Lịch hẹn hôm nay")).toBeNull();
  });

  it("a nav that failed to load does not leave the work card spinning forever (#483)", async () => {
    active.value = { ...active.value, activeMeta: { id: "a", name: "clinic", label: "Phòng khám" }, activeNav: null, navError: true };
    renderHome();
    expect(await screen.findByText(t("page.home.noApprovals"))).toBeInTheDocument();
  });

  it("no home dashboard chosen → no dashboard is run", async () => {
    active.value = { ...active.value, activeMeta: { id: "a", name: "clinic", label: "Phòng khám" } };
    renderHome();
    expect(await screen.findByText("Lịch hẹn hôm nay")).toBeInTheDocument();
    expect(runDashboard).not.toHaveBeenCalled();
  });
});
