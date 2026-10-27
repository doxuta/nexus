// DashboardViewer (specs/074 lát f2-dashboard-viewer-extract): renders standalone from props {id, embedded, onDeleted};
// in the embedded (Studio) form a delete asks in place and hands control back through onDeleted instead of routing.
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  runDashboard: vi.fn(),
  updateDashboard: vi.fn(),
  deleteDashboard: vi.fn(),
  listFields: vi.fn(),
  listEntities: vi.fn(),
  getMe: vi.fn(),
  getCachedMe: vi.fn(),
  getPublishState: vi.fn(),
}));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));
vi.mock("@/lib/admin", async (orig) => ({ ...(await orig<typeof import("@/lib/admin")>()), listUsers: vi.fn().mockResolvedValue([]) }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { id: "u1", isAdmin: true } }) }));
const nav = vi.hoisted(() => ({ push: vi.fn() }));
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: nav.push, replace: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => "/studio/du-an/northwind/dung/bao-cao/tong-quan/d1",
}));

import { onDraftsChanged } from "@/lib/draftEvents";
import { DashboardViewer } from "./DashboardViewer";

const run = () => ({
  dashboard: { id: "d1", name: "CRM Overview", description: "", widgets: [] },
  widgets: [],
});

function renderViewer(props: { embedded?: boolean; onDeleted?: () => void }) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}><DashboardViewer id="d1" {...props} /></QueryClientProvider>);
}

beforeEach(() => {
  Object.values(api).forEach((f) => f.mockReset());
  api.runDashboard.mockResolvedValue(run());
  api.deleteDashboard.mockResolvedValue(undefined);
  api.listFields.mockResolvedValue([]);
  api.listEntities.mockResolvedValue([]);
  api.getMe.mockResolvedValue({ id: "u1", isAdmin: true });
  api.getCachedMe.mockReturnValue({ id: "u1", isAdmin: true });
  api.getPublishState.mockRejectedValue(new Error("403"));
  nav.push.mockReset();
});

describe("DashboardViewer", () => {
  it("renders standalone from an id", async () => {
    await act(async () => { renderViewer({}); });
    expect(await screen.findByRole("heading", { level: 1, name: /CRM Overview/ })).toBeInTheDocument();
  });

  it("embedded delete asks first, then calls onDeleted and does not route", async () => {
    const onDeleted = vi.fn();
    await act(async () => { renderViewer({ embedded: true, onDeleted }); });
    await screen.findByRole("heading", { level: 1, name: /CRM Overview/ });
    // embedded: the authoring column is already open
    fireEvent.click(await screen.findByRole("button", { name: /Xoá trang tổng quan/ }));
    expect(api.deleteDashboard).not.toHaveBeenCalled();
    expect(screen.queryByRole("alertdialog")).toBeNull(); // in place, never a dialog (D-UI-28)
    const buttons = await screen.findAllByRole("button", { name: "Xoá trang tổng quan" });
    await act(async () => { fireEvent.click(buttons[buttons.length - 1]); });
    expect(api.deleteDashboard).toHaveBeenCalledWith("d1");
    expect(onDeleted).toHaveBeenCalled();
    expect(nav.push).not.toHaveBeenCalled();
  });

  // Issue #62: on a delivered project DELETE /admin/dashboards/:id writes a dashboard "delete" draft (or drops a draft-only
  // dashboard; DraftDashboardRepo.Delete): the header recounts once the DELETE has answered, never before, never on a refusal.
  it("delete announces on the drafts bus once it has answered, never before, never on a refusal (#62)", async () => {
    const heard: string[] = [];
    const off = onDraftsChanged((s) => heard.push(s));
    try {
      const onDeleted = vi.fn();
      await act(async () => { renderViewer({ embedded: true, onDeleted }); });
      await screen.findByRole("heading", { level: 1, name: /CRM Overview/ });
      const confirmDelete = async () => {
        fireEvent.click(await screen.findByRole("button", { name: /Xoá trang tổng quan/ }));
        const buttons = await screen.findAllByRole("button", { name: "Xoá trang tổng quan" });
        await act(async () => { fireEvent.click(buttons[buttons.length - 1]); });
      };

      api.deleteDashboard.mockRejectedValueOnce(new Error("refused"));
      await confirmDelete();
      expect(api.deleteDashboard).toHaveBeenCalledTimes(1);
      expect(onDeleted).not.toHaveBeenCalled();
      expect(heard).toEqual([]);

      let answer!: () => void;
      api.deleteDashboard.mockReturnValueOnce(new Promise<void>((r) => { answer = r; }));
      await confirmDelete();
      expect(api.deleteDashboard).toHaveBeenCalledTimes(2);
      await act(async () => { await new Promise((r) => setTimeout(r, 20)); });
      expect(heard).toEqual([]);
      await act(async () => { answer(); });
      expect(onDeleted).toHaveBeenCalled();
      expect(heard).toEqual(["dashboard"]);
    } finally {
      off();
    }
  });

  it("embedded rename is a form in the page (no dialog) and saves the new name", async () => {
    api.updateDashboard.mockResolvedValue({});
    await act(async () => { renderViewer({ embedded: true }); });
    await screen.findByRole("heading", { level: 1, name: /CRM Overview/ });
    fireEvent.click(screen.getByRole("button", { name: /Đổi tên/ }));
    expect(screen.queryByRole("dialog")).toBeNull();
    const input = await screen.findByLabelText(/Tên/);
    fireEvent.change(input, { target: { value: "Tổng quan mới" } });
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: /Lưu tên/ })); });
    expect(api.updateDashboard).toHaveBeenCalled();
    expect(api.updateDashboard.mock.calls[0][1]).toBe("Tổng quan mới");
  });
  it("a widget the caller cannot read shows the Vietnamese no-permission state, not FORBIDDEN, and probes no fields", async () => {
    const def = { id: "w1", title: "Số việc", type: "bar", entity: "pm_tasks", metric: "count", groupBy: "status", x: 0, y: 0, w: 4, h: 3 };
    api.runDashboard.mockResolvedValue({
      dashboard: { id: "d1", name: "Tổng quan Dự án", description: "", widgets: [def] },
      widgets: [{ widgetId: "w1", title: "Số việc", type: "bar", result: null, error: 'không có quyền xem entity "pm_tasks"', code: "FORBIDDEN" }],
    });
    await act(async () => { renderViewer({}); });
    expect(await screen.findByText("Bạn không có quyền xem dữ liệu này.")).toBeInTheDocument();
    expect(screen.queryByText("FORBIDDEN")).toBeNull();
    expect(screen.queryByText(/pm_tasks/)).toBeNull();
    expect(api.listFields).not.toHaveBeenCalledWith("pm_tasks");
  });
});
