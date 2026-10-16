// specs/073 FR-051: the project switcher in the Studio header. At the front page (no project) it offers the internal
// group (Xưởng Nexus) and "Tất cả dự án"; "Dự án mới" stays hidden until a project can be created. Keyboard: Enter opens,
// arrows move, Enter chooses, Esc closes and gives focus back.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render as rtlRender, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactElement } from "react";

import { projectRow } from "@/app/dev/kit/sections/projectRows";

const nav = vi.hoisted(() => ({ push: vi.fn(), confirm: vi.fn(() => true), switchProject: vi.fn(), exitProject: vi.fn(), enterProject: vi.fn(async () => {}) }));
vi.mock("@/lib/navigation-blocker", () => ({ useGuardedRouter: () => ({ push: nav.push }), useConfirmLeave: () => nav.confirm }));
const api = vi.hoisted(() => ({ listProjects: vi.fn() }));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));
vi.mock("@/lib/studioContext", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/studioContext")>()),
  switchProject: nav.switchProject,
  exitProject: nav.exitProject,
  enterProject: nav.enterProject,
}));

import { resetStudioContextForTests } from "@/lib/studioContext";
import { clearWorkspace, setWorkspace } from "@/lib/workspace";
import { ProjectSwitcher } from "./ProjectSwitcher";

const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
const render = (ui: ReactElement) => rtlRender(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);

beforeEach(() => {
  qc.clear();
  api.listProjects.mockResolvedValue([]);
  resetStudioContextForTests();
  nav.push.mockReset();
  nav.switchProject.mockReset();
  nav.exitProject.mockReset();
  window.localStorage.clear();
  window.sessionStorage.clear();
  clearWorkspace();
  window.localStorage.setItem("nexus_tenant", "northwind");
});

describe("ProjectSwitcher", () => {
  it("at home shows 'Tất cả dự án' on the trigger and the two groups in the list", async () => {
    render(<ProjectSwitcher currentSlug={null} />);
    const trigger = screen.getByRole("button", { name: /Chuyển dự án/ });
    expect(trigger).toHaveTextContent("Tất cả dự án");
    await userEvent.click(trigger);
    const menu = await screen.findByRole("menu");
    expect(menu).toHaveTextContent("Nội bộ");
    expect(screen.getByRole("menuitem", { name: "Xưởng Nexus" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Tất cả dự án" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Dự án mới" })).toBeInTheDocument();
  });

  it("names the workshop on the trigger when the page is the workshop's", () => {
    render(<ProjectSwitcher currentSlug="northwind" />);
    expect(screen.getByRole("button", { name: /Chuyển dự án/ })).toHaveTextContent("Xưởng Nexus");
  });

  it("choosing the workshop goes to its first page; choosing 'Tất cả dự án' goes to /studio", async () => {
    render(<ProjectSwitcher currentSlug={null} />);
    await userEvent.click(screen.getByRole("button", { name: /Chuyển dự án/ }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Xưởng Nexus" }));
    expect(nav.push).toHaveBeenLastCalledWith("/studio/du-an/northwind/dung/du-lieu");
    await userEvent.click(screen.getByRole("button", { name: /Chuyển dự án/ }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Tất cả dự án" }));
    expect(nav.push).toHaveBeenLastCalledWith("/studio");
  });

  it("keyboard: Enter opens, arrows move, Enter chooses; Esc closes and returns focus to the trigger", async () => {
    render(<ProjectSwitcher currentSlug={null} />);
    const trigger = screen.getByRole("button", { name: /Chuyển dự án/ });
    trigger.focus();
    await userEvent.keyboard("{Enter}");
    await screen.findByRole("menu");
    await userEvent.keyboard("{ArrowDown}");
    const first = document.activeElement as HTMLElement;
    expect(first).toHaveAttribute("role", "menuitem");
    await userEvent.keyboard("{ArrowDown}");
    expect(document.activeElement).not.toBe(first);
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("menu")).toBeNull();
    expect(trigger).toHaveFocus();
    expect(nav.push).not.toHaveBeenCalled();
  });

  it("lists customer projects grouped by stage; choosing one opens it (from home) or switches (from another project)", async () => {
    api.listProjects.mockResolvedValue([
      projectRow({ tenantId: "t-1", slug: "pk", name: "Phòng khám", stage: "building" }),
      projectRow({ tenantId: "t-2", slug: "gv", name: "Gạch Việt", stage: "trial" }),
    ]);
    const { unmount } = render(<ProjectSwitcher currentSlug={null} />);
    await userEvent.click(screen.getByRole("button", { name: /Chuyển dự án/ }));
    const menu = await screen.findByRole("menu");
    await screen.findByRole("menuitem", { name: "Gạch Việt" });
    expect(menu).toHaveTextContent("Đang dựng");
    expect(menu).toHaveTextContent("Chạy thử");
    await userEvent.click(screen.getByRole("menuitem", { name: "Phòng khám" }));
    expect(nav.enterProject).toHaveBeenCalledWith("pk", "/studio/du-an/pk/dung/du-lieu", expect.anything());
    unmount();
    render(<ProjectSwitcher currentSlug="pk" />);
    await userEvent.click(screen.getByRole("button", { name: /Chuyển dự án/ }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Gạch Việt" }));
    expect(nav.switchProject).toHaveBeenCalledWith("gv", "/studio/du-an/gv/dung/du-lieu", expect.anything());
  });
});

// ── ph1b T425 (FR-083): inside a build session ──
// GET /admin/projects answers 403 there, so the list of customers is not offered: the project being built, "Gần đây"
// (what this operator opened lately, per home user) and the way back. Choosing a recent one is a real session switch.
describe("ProjectSwitcher inside a build session", () => {
  const enter = (slug = "nhakhoa", name = "Nha khoa An Tâm") =>
    setWorkspace({
      accessToken: "WS",
      expiresAt: "2099-01-01T00:00:00Z",
      homeUserId: "u-1",
      session: { id: "s", tenantId: "t", tenantSlug: slug, tenantName: name, openedAt: "2098-01-01T00:00:00Z", expiresAt: "2099-01-01T00:00:00Z" },
    });
  const recents = (...list: Array<[string, string]>) =>
    window.localStorage.setItem("nexus.studio.recentProjects:u-1", JSON.stringify(list.map(([slug, name]) => ({ slug, name }))));

  it("the trigger names the project being built; the list has it ticked, 'Gần đây' without it, and the way back", async () => {
    enter();
    recents(["nhakhoa", "Nha khoa An Tâm"], ["spa", "Spa Hương Sen"], ["tmdt", "Cửa hàng TMĐT"]);
    render(<ProjectSwitcher currentSlug="nhakhoa" />);
    const trigger = screen.getByRole("button", { name: /Chuyển dự án/ });
    expect(trigger).toHaveTextContent("Nha khoa An Tâm");
    await userEvent.click(trigger);
    const menu = await screen.findByRole("menu");
    expect(menu).toHaveTextContent("Gần đây");
    expect(screen.getByRole("menuitem", { name: "Nha khoa An Tâm" })).toHaveAttribute("aria-current", "true"); // not only a glyph
    expect(screen.getByRole("menuitem", { name: "Spa Hương Sen" })).not.toHaveAttribute("aria-current");
    expect(screen.getByRole("menuitem", { name: "Spa Hương Sen" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Cửa hàng TMĐT" })).toBeInTheDocument();
    expect(screen.getAllByRole("menuitem", { name: "Nha khoa An Tâm" })).toHaveLength(1); // not listed twice
    expect(screen.getByRole("menuitem", { name: "Về danh sách dự án" })).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: "Xưởng Nexus" })).toBeNull(); // the home workshop is not reachable from here without closing the session
    expect(screen.queryByRole("menuitem", { name: "Tất cả dự án" })).toBeNull();
  });

  it("choosing a recent project switches the session (frozen switch of lib/studioContext), it does not push a route", async () => {
    enter();
    recents(["spa", "Spa Hương Sen"]);
    render(<ProjectSwitcher currentSlug="nhakhoa" />);
    await userEvent.click(screen.getByRole("button", { name: /Chuyển dự án/ }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Spa Hương Sen" }));
    expect(nav.switchProject).toHaveBeenCalledTimes(1);
    const [slug, next, deps] = nav.switchProject.mock.calls[0];
    expect(slug).toBe("spa");
    expect(next).toBe("/studio/du-an/spa/dung/du-lieu");
    expect(deps.qc).toBe(qc);
    expect(deps.confirmLeave).toBe(nav.confirm);
    expect(nav.push).not.toHaveBeenCalled();
  });

  it("'Về danh sách dự án' closes the session and loads /studio", async () => {
    enter();
    render(<ProjectSwitcher currentSlug="nhakhoa" />);
    await userEvent.click(screen.getByRole("button", { name: /Chuyển dự án/ }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Về danh sách dự án" }));
    expect(nav.exitProject).toHaveBeenCalledWith("/studio", expect.objectContaining({ qc, confirmLeave: nav.confirm }));
    expect(nav.push).not.toHaveBeenCalled();
  });

  it("with no recent entries it still offers the current project and the way back, and nothing invented", async () => {
    enter();
    render(<ProjectSwitcher currentSlug="nhakhoa" />);
    await userEvent.click(screen.getByRole("button", { name: /Chuyển dự án/ }));
    const menu = await screen.findByRole("menu");
    expect(menu).not.toHaveTextContent("Gần đây");
    expect(screen.getAllByRole("menuitem").map((m) => m.textContent)).toEqual(["Nha khoa An Tâm", "Dự án mới", "Về danh sách dự án"]);
  });

  it("another operator's recents are not offered (keyed by the home user)", async () => {
    enter();
    window.localStorage.setItem("nexus.studio.recentProjects:u-9", JSON.stringify([{ slug: "bi-mat", name: "Khách bí mật" }]));
    render(<ProjectSwitcher currentSlug="nhakhoa" />);
    await userEvent.click(screen.getByRole("button", { name: /Chuyển dự án/ }));
    await screen.findByRole("menu");
    expect(screen.queryByText("Khách bí mật")).toBeNull();
  });

  it("leaving the session mid-page returns the switcher to the home list", async () => {
    enter();
    render(<ProjectSwitcher currentSlug={null} />);
    expect(screen.getByRole("button", { name: /Chuyển dự án/ })).toHaveTextContent("Nha khoa An Tâm");
    act(() => clearWorkspace());
    expect(screen.getByRole("button", { name: /Chuyển dự án/ })).toHaveTextContent("Tất cả dự án");
  });
});
