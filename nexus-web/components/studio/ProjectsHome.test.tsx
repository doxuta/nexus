// specs/073 so1b T453 (FR-050, FR-091, FR-094): /studio is the stage board over GET /admin/projects.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ProjectHealth } from "@/lib/api";
import { projectRow } from "@/app/dev/kit/sections/projectRows";

const nav = vi.hoisted(() => ({ search: "", replace: vi.fn(), push: vi.fn() }));
vi.mock("next/navigation", () => ({
  usePathname: () => "/studio",
  useRouter: () => ({ push: nav.push, replace: nav.replace }),
  useSearchParams: () => new URLSearchParams(nav.search),
}));
vi.mock("./ContextGate", () => ({ ContextGate: ({ children }: { children: React.ReactNode }) => <>{children}</> }));
const ctxMock = vi.hoisted(() => ({ enterProject: vi.fn(async () => {}) }));
vi.mock("@/lib/studioContext", async (orig) => ({ ...(await orig<typeof import("@/lib/studioContext")>()), enterProject: ctxMock.enterProject }));
const api = vi.hoisted(() => ({ listProjects: vi.fn(), patchProject: vi.fn(), healthCheckProject: vi.fn() }));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));

import { resetStudioContextForTests } from "@/lib/studioContext";
import { ProjectsHome } from "./ProjectsHome";

const FRESH = new Date().toISOString();
const STALE = "2026-01-01T00:00:00Z";
const okHealth: ProjectHealth = { status: "ok", critical: 0, warn: 0, checks: [], appsNotReady: [], checkedAt: FRESH };

function mount() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ProjectsHome />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  resetStudioContextForTests();
  vi.clearAllMocks();
  nav.search = "";
  window.localStorage.clear();
  window.localStorage.setItem("nexus_tenant", "northwind");
  api.listProjects.mockResolvedValue([]);
  api.healthCheckProject.mockResolvedValue(okHealth);
});

describe("ProjectsHome (stage board)", () => {
  it("h1 'Dự án khách', and no 'not built yet' notice any more", async () => {
    mount();
    expect(screen.getByRole("heading", { level: 1, name: "Dự án khách" })).toBeInTheDocument();
    await screen.findAllByText("Chưa có dự án nào.");
    expect(screen.queryByText(/chưa có trong bản Nexus/)).toBeNull();
  });

  it("loading, error with retry, empty, data", async () => {
    api.listProjects.mockReturnValueOnce(new Promise(() => {}));
    const a = mount();
    expect(a.container.querySelector(".slds-spinner")).not.toBeNull();
    a.unmount();
    api.listProjects.mockRejectedValueOnce(new Error("boom"));
    const b = mount();
    fireEvent.click(await screen.findByRole("button", { name: "Thử lại" }));
    await screen.findAllByText("Chưa có dự án nào.");
    b.unmount();
    api.listProjects.mockResolvedValue([projectRow({ health: okHealth })]);
    mount();
    expect(await screen.findByRole("button", { name: "Phòng khám Bạn Nhỏ" })).toBeInTheDocument();
  });

  it("auto-rechecks snapshots older than 24h, at most 2 at a time, and only the project check endpoint", async () => {
    const rows = [1, 2, 3, 4].map((i) => projectRow({ tenantId: `t-${i}`, slug: `p-${i}`, name: `P${i}`, health: { ...okHealth, checkedAt: STALE } }));
    rows.push(projectRow({ tenantId: "t-9", slug: "p-9", name: "P9", health: okHealth }));
    api.listProjects.mockResolvedValue(rows);
    let live = 0;
    let peak = 0;
    const done: (() => void)[] = [];
    api.healthCheckProject.mockImplementation(() => {
      live++;
      peak = Math.max(peak, live);
      return new Promise((res) => done.push(() => { live--; res(okHealth); }));
    });
    mount();
    await waitFor(() => expect(api.healthCheckProject).toHaveBeenCalledTimes(2));
    expect(peak).toBe(2);
    done.splice(0).forEach((f) => f());
    await waitFor(() => expect(api.healthCheckProject).toHaveBeenCalledTimes(4));
    done.splice(0).forEach((f) => f());
    expect(api.healthCheckProject.mock.calls.map((c) => c[0]).sort()).toEqual(["t-1", "t-2", "t-3", "t-4"]);
  });

  it("'Khám lại' from the menu shows the result on the row", async () => {
    api.listProjects.mockResolvedValue([projectRow({ health: okHealth })]);
    api.healthCheckProject.mockResolvedValue({ ...okHealth, status: "warn", warn: 2 });
    const user = userEvent.setup();
    mount();
    await user.click(await screen.findByRole("button", { name: "Thao tác với dự án Phòng khám Bạn Nhỏ" }));
    await user.click(await screen.findByRole("menuitem", { name: "Khám lại" }));
    const li = screen.getByRole("button", { name: "Phòng khám Bạn Nhỏ" }).closest("li")!;
    expect(await within(li).findByText(/Cần xem/)).toBeInTheDocument();
  });

  it("'Bắt đầu chạy thử' patches trial=true", async () => {
    api.listProjects.mockResolvedValue([projectRow({ health: okHealth })]);
    api.patchProject.mockResolvedValue(projectRow({ stage: "trial" }));
    const user = userEvent.setup();
    mount();
    await user.click(await screen.findByRole("button", { name: "Thao tác với dự án Phòng khám Bạn Nhỏ" }));
    await user.click(await screen.findByRole("menuitem", { name: "Bắt đầu chạy thử" }));
    await waitFor(() => expect(api.patchProject).toHaveBeenCalledWith("t-1", { trial: true }));
  });

  it("opening a project enters its build session; settings is a plain link in the home context", async () => {
    api.listProjects.mockResolvedValue([projectRow({ health: okHealth })]);
    const user = userEvent.setup();
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Phòng khám Bạn Nhỏ" }));
    await waitFor(() => expect(ctxMock.enterProject).toHaveBeenCalledWith("pk-ban-nho", "/studio/du-an/pk-ban-nho/dung/du-lieu", expect.anything()));
    await user.click(screen.getByRole("button", { name: "Thao tác với dự án Phòng khám Bạn Nhỏ" }));
    await user.click(await screen.findByRole("menuitem", { name: "Cài đặt dự án" }));
    expect(nav.push).toHaveBeenCalledWith("/studio/du-an/pk-ban-nho/cai-dat");
  });

  it("the search box keeps ?q= and filters; 'Dự án mới' is a real link to its own page (tao1)", async () => {
    api.listProjects.mockResolvedValue([projectRow({ health: okHealth })]);
    nav.search = "q=zzz";
    mount();
    await screen.findAllByText("Không có dự án khớp “zzz”.");
    fireEvent.change(screen.getByRole("searchbox"), { target: { value: "ban" } });
    expect(nav.replace).toHaveBeenCalledWith("/studio?q=ban");
    expect(screen.getByRole("link", { name: "Dự án mới" })).toHaveAttribute("href", "/studio/moi");
  });
});
