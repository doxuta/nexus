// /dashboards/[id] on SLDS (specs/038 T029) — page header + widget cards on the 12-column grid +
// the run filter bar. Pins the viewer's behaviour across the restyle: per-widget error isolation,
// KPI/table rendering with display labels and code-based drill, the URL-synced run filters, and the
// admin-only authoring (layout edit mode, bar↔funnel toggle, rename, widget panel, delete).
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Suspense } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { DashboardRun } from "@/lib/api";
import { presetRange } from "@/lib/runFilters";

const api = vi.hoisted(() => ({
  runDashboard: vi.fn(),
  updateDashboard: vi.fn(),
  deleteDashboard: vi.fn(),
  listFields: vi.fn(),
  listEntities: vi.fn(),
  getMe: vi.fn(),
  getCachedMe: vi.fn(),
  getPublishState: vi.fn(), // specs/063 S7-2: the draft-view button asks; not the builder here
}));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));
vi.mock("@/lib/admin", async (orig) => ({ ...(await orig<typeof import("@/lib/admin")>()), listUsers: vi.fn().mockResolvedValue([]) }));

const auth = vi.hoisted(() => ({ user: { id: "u1", isAdmin: false } as { id: string; isAdmin: boolean } }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => auth }));

const nav = vi.hoisted(() => ({ push: vi.fn(), replace: vi.fn(), params: new URLSearchParams() }));
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: nav.push, replace: nav.replace }),
  useSearchParams: () => nav.params,
  usePathname: () => "/dashboards/d1",
}));

import DashboardViewerPage from "@/app/(app)/dashboards/[id]/page";

const WIDGETS = [
  { id: "w1", title: "Pipeline theo giai đoạn", type: "bar", entity: "crm_opportunities", metric: "count", groupBy: "stage", x: 0, y: 0, w: 6, h: 4 },
  { id: "w2", title: "Tổng hợp đồng", type: "kpi", entity: "crm_contracts", metric: "count", x: 6, y: 0, w: 3, h: 2 },
  { id: "w3", title: "Cơ hội theo giai đoạn", type: "table", entity: "crm_opportunities", metric: "count", groupBy: "stage", x: 0, y: 4, w: 6, h: 4 },
  { id: "w4", title: "Công việc", type: "kpi", entity: "pm_tasks", metric: "count", x: 9, y: 0, w: 3, h: 2 },
];

function run(): DashboardRun {
  const groups = [{ key: "won", value: "3", count: 3 }, { key: "lost", value: "1", count: 1 }];
  return {
    dashboard: { id: "d1", name: "CRM Overview", description: "Pipeline và hợp đồng.", widgets: WIDGETS },
    widgets: [
      { widgetId: "w1", title: "Pipeline theo giai đoạn", type: "bar", result: { count: 4, grouped: true, groups } },
      { widgetId: "w2", title: "Tổng hợp đồng", type: "kpi", result: { count: 12, grouped: false, value: "12" } },
      { widgetId: "w3", title: "Cơ hội theo giai đoạn", type: "table", result: { count: 4, grouped: true, groups } },
      // A run failure, not an ACL denial: FORBIDDEN has its own no-permission card (15888da17),
      // pinned in components/analytics/DashboardViewer.test.tsx.
      { widgetId: "w4", title: "Công việc", type: "kpi", error: "lỗi tính toán widget", code: "WIDGET_RUN_FAILED" },
    ],
  };
}

const STAGE = { name: "stage", label: "Giai đoạn", fieldType: "select", options: { values: ["won", "lost"], valueLabels: { won: "Thắng", lost: "Thua" } } };

async function renderViewer() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const params = Promise.resolve({ id: "d1" });
  await act(async () => {
    render(
      <QueryClientProvider client={qc}>
        <Suspense fallback={null}>
          <DashboardViewerPage params={params} />
        </Suspense>
      </QueryClientProvider>,
    );
  });
  await screen.findByRole("heading", { level: 1, name: /CRM Overview/ });
}

beforeEach(() => {
  api.runDashboard.mockReset().mockImplementation(async () => run());
  api.updateDashboard.mockReset().mockResolvedValue({ id: "d1", name: "CRM Overview", widgets: WIDGETS });
  api.deleteDashboard.mockReset().mockResolvedValue(undefined);
  api.listFields.mockReset().mockResolvedValue([STAGE]);
  api.listEntities.mockReset().mockResolvedValue([{ name: "crm_opportunities", label: "Cơ hội", tableName: "crm_opportunities" }]);
  api.getMe.mockReset().mockResolvedValue({ id: "u1", isAdmin: false });
  api.getCachedMe.mockReset().mockReturnValue({ id: "u1", isAdmin: false });
  api.getPublishState.mockReset().mockRejectedValue(new Error("403"));
  nav.push.mockReset();
  nav.replace.mockReset();
  nav.params = new URLSearchParams();
  auth.user = { id: "u1", isAdmin: false };
});

/** The widget card whose header names `title`. */
function card(title: string) {
  const el = screen.getByTitle(title).closest(".slds-card");
  expect(el).not.toBeNull();
  return el as HTMLElement;
}

describe("/dashboards/[id] — viewing (non-admin)", () => {
  it("shows the dashboard in an SLDS page header with its highlights", async () => {
    await renderViewer();
    const header = document.querySelector(".slds-page-header") as HTMLElement;
    expect(within(header).getByText("Pipeline và hợp đồng.")).toBeInTheDocument();
    expect(within(header).getByText("4")).toBeInTheDocument(); // widget count
    expect(within(header).getByRole("button", { name: "Làm mới" })).toBeInTheDocument();
  });

  it("draws each widget in its own card: KPI value, table with labels, isolated error", async () => {
    await renderViewer();
    expect(within(card("Tổng hợp đồng")).getByText("12")).toBeInTheDocument();
    expect(within(card("Tổng hợp đồng")).getByText("12 bản ghi · count")).toBeInTheDocument();

    const table = within(card("Cơ hội theo giai đoạn")).getByRole("table");
    expect(table).toHaveClass("slds-table");
    expect(await within(table).findByRole("button", { name: "Thắng" })).toBeInTheDocument();

    const err = card("Công việc");
    expect(within(err).getByText("lỗi tính toán widget")).toBeInTheDocument();
    expect(within(err).getByText("WIDGET_RUN_FAILED")).toBeInTheDocument();
    expect(within(err).getByText(/các ô số liệu khác vẫn chạy/)).toBeInTheDocument();
    // The failure speaks as a warning to assistive tech too, not as "info" behind a warning glyph.
    const note = within(err).getByRole("status");
    expect(note).toHaveClass("slds-theme_warning");
    expect(note.querySelector(".slds-icon_container")).toHaveClass("slds-icon-utility-warning");
    expect(within(note).getByText("Cảnh báo")).toHaveClass("slds-assistive-text");
    expect(within(note).queryByText("Thông tin")).toBeNull();
  });

  it("drills a table group into the list filtered by the stored code, not the label", async () => {
    await renderViewer();
    const table = within(card("Cơ hội theo giai đoạn")).getByRole("table");
    fireEvent.click(await within(table).findByRole("button", { name: "Thắng" }));
    expect(nav.push).toHaveBeenCalledWith("/crm_opportunities?stage%5Beq%5D=won");
  });

  it("has no authoring controls", async () => {
    await renderViewer();
    for (const name of ["Sắp xếp lại ô", "Quản lý ô số liệu", "Đổi tên", "Phễu"]) {
      expect(screen.queryByRole("button", { name })).toBeNull();
    }
  });

  it("a date preset rewrites the URL the run reads its filters from", async () => {
    await renderViewer();
    fireEvent.click(screen.getByRole("button", { name: /Khoảng ngày/ }));
    fireEvent.change(screen.getByLabelText("Khoảng có sẵn"), { target: { value: "7d" } });
    const r = presetRange("7d", new Date());
    expect(nav.replace).toHaveBeenCalledWith(`/dashboards/d1?dateFrom=${r.dateFrom}&dateTo=${r.dateTo}`, { scroll: false });
  });

  it("runs with the filters in the URL", async () => {
    nav.params = new URLSearchParams("dateFrom=2026-09-01&dateTo=2026-09-29&ownerId=u1");
    await renderViewer();
    expect(api.runDashboard).toHaveBeenCalledWith("d1", { dateFrom: "2026-09-01", dateTo: "2026-09-29", ownerId: "u1" });
  });
});

describe("/dashboards/[id] — authoring (admin)", () => {
  beforeEach(() => {
    auth.user = { id: "a1", isAdmin: true };
    api.getCachedMe.mockReturnValue({ id: "a1", isAdmin: true });
    api.getMe.mockResolvedValue({ id: "a1", isAdmin: true });
  });

  it("layout edit mode: hint + reset, and 'Xong' saves the layout on the shared definition", async () => {
    await renderViewer();
    fireEvent.click(screen.getByRole("button", { name: "Sắp xếp lại ô" }));
    expect(screen.getByText(/Kéo để di chuyển/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Đặt lại" }));
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Xong" }));
    });
    expect(api.updateDashboard).toHaveBeenCalledTimes(1);
    const [id, name, widgets, desc] = api.updateDashboard.mock.calls[0];
    expect([id, name, desc]).toEqual(["d1", "CRM Overview", "Pipeline và hợp đồng."]);
    expect(widgets.map((w: { id: string }) => w.id)).toEqual(["w1", "w2", "w3", "w4"]);
    // 'Đặt lại' dropped the stored rects for the default flow (lib/dashLayout defaultLayoutFor).
    type Rect = { x: number; y: number; w: number; h: number };
    expect(widgets.map(({ x, y, w, h }: Rect) => [x, y, w, h])).toEqual([
      [0, 0, 6, 5], [6, 0, 3, 2], [0, 5, 12, 5], [0, 10, 3, 2],
    ]);
    expect(screen.getByRole("button", { name: "Sắp xếp lại ô" })).toBeInTheDocument();
  });

  it("keyboard focus stays on the edit-layout button when edit mode toggles 'Đặt lại' in and out", async () => {
    await renderViewer();
    const edit = screen.getByRole("button", { name: "Sắp xếp lại ô" });
    edit.focus();
    fireEvent.click(edit);
    expect(screen.getByRole("button", { name: "Đặt lại" })).toBeInTheDocument();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Xong" }));
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Xong" }));
    });
    expect(screen.queryByRole("button", { name: "Đặt lại" })).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Sắp xếp lại ô" }));
  });

  it("toggles a grouped bar widget to a funnel", async () => {
    await renderViewer();
    const bar = card("Pipeline theo giai đoạn");
    fireEvent.click(within(bar).getByRole("button", { name: "Phễu" }));
    expect(within(bar).getByRole("button", { name: "Cột" })).toBeInTheDocument();
  });

  it("renames the dashboard from a modal", async () => {
    await renderViewer();
    fireEvent.click(screen.getByRole("button", { name: "Đổi tên" }));
    const dialog = screen.getByRole("dialog", { name: "Đổi tên trang tổng quan" });
    const name = within(dialog).getByLabelText(/Tên trang tổng quan/);
    expect(name).toHaveValue("CRM Overview");
    fireEvent.change(name, { target: { value: "CRM tổng quan" } });
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Lưu tên" }));
    });
    expect(api.updateDashboard).toHaveBeenCalledWith("d1", "CRM tổng quan", expect.any(Array), "Pipeline và hợp đồng.");
    expect(screen.queryByRole("dialog", { name: "Đổi tên trang tổng quan" })).toBeNull();
  });

  it("opens the widget panel and deletes the dashboard behind a prompt", async () => {
    await renderViewer();
    fireEvent.click(screen.getByRole("button", { name: "Quản lý ô số liệu" }));
    expect(screen.getByRole("button", { name: "Quản lý ô số liệu" })).toHaveAttribute("aria-pressed", "true");
    fireEvent.click(screen.getByRole("button", { name: "Xoá trang tổng quan" }));
    const prompt = screen.getByRole("alertdialog");
    await act(async () => {
      fireEvent.click(within(prompt).getByRole("button", { name: "Xoá trang tổng quan" }));
    });
    expect(api.deleteDashboard).toHaveBeenCalledWith("d1");
    expect(nav.push).toHaveBeenCalledWith("/dashboards");
  });
});

// specs/063-draft-publish S7-2 T060/T061: the builder of a delivered workspace saves into the draft and
// looks at the draft on THIS screen only.
describe("/dashboards/[id] — draft view (builder of a delivered workspace)", () => {
  beforeEach(() => {
    auth.user = { id: "a1", isAdmin: true };
    api.getCachedMe.mockReturnValue({ id: "a1", isAdmin: true });
    api.getMe.mockResolvedValue({ id: "a1", isAdmin: true });
    api.getPublishState.mockReset().mockResolvedValue({ delivered: true, pendingCount: 0 });
    nav.replace.mockReset();
    nav.params = new URLSearchParams();
  });

  it("offers 'Xem bản nháp' and turns the screen's flag on (?draft=1)", async () => {
    await renderViewer();
    fireEvent.click(await screen.findByRole("button", { name: "Xem bản nháp" }));
    expect(nav.replace).toHaveBeenCalledWith("/dashboards/d1?draft=1");
  });

  it("on the draft view a draft-state dashboard says the customer does not see it; 'Xem bản đang chạy' goes back", async () => {
    nav.params = new URLSearchParams("draft=1");
    api.runDashboard.mockImplementation(async () => ({ ...run(), dashboard: { ...run().dashboard, draftState: "edited" } }));
    await renderViewer();
    expect(await screen.findByText(/Khách chưa thấy thay đổi này/)).toBeInTheDocument();
    const live = screen.getAllByRole("button", { name: "Xem bản đang chạy" });
    fireEvent.click(live[live.length - 1]);
    expect(nav.replace).toHaveBeenCalledWith("/dashboards/d1");
  });

  it("a save answered with draftState moves the screen onto the draft", async () => {
    api.updateDashboard.mockResolvedValue({ id: "d1", name: "CRM tổng quan", widgets: WIDGETS, draftState: "edited" });
    await renderViewer();
    fireEvent.click(screen.getByRole("button", { name: "Đổi tên" }));
    const dialog = screen.getByRole("dialog", { name: "Đổi tên trang tổng quan" });
    fireEvent.change(within(dialog).getByLabelText(/Tên trang tổng quan/), { target: { value: "CRM tổng quan" } });
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Lưu tên" }));
    });
    expect(nav.replace).toHaveBeenCalledWith("/dashboards/d1?draft=1");
  });

  it("a save in a live workspace leaves the screen where it is", async () => {
    await renderViewer();
    fireEvent.click(screen.getByRole("button", { name: "Đổi tên" }));
    const dialog = screen.getByRole("dialog", { name: "Đổi tên trang tổng quan" });
    fireEvent.change(within(dialog).getByLabelText(/Tên trang tổng quan/), { target: { value: "CRM tổng quan" } });
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Lưu tên" }));
    });
    expect(nav.replace).not.toHaveBeenCalled();
  });
});
