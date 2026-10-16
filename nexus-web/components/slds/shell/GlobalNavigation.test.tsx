// GlobalNavigation — blueprint global-navigation/navigation-bar (SLDS 2.264.1).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, renderHook, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { qk } from "@/lib/query/keys";
import { GlobalNavigation, activeTabIndex, splitTabs, useShellNav, type GlobalNavigationProps, type NavTab } from "./GlobalNavigation";

const lens = vi.hoisted(() => ({
  activeApp: "*",
  activeMeta: null as null | { label: string },
  activeNav: null as null | { type: string; refId: string; label: string; href: string }[],
  pins: [] as string[],
}));

vi.mock("next/navigation", () => ({ usePathname: () => "/home", useRouter: () => ({ push: vi.fn() }) }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { id: "u2", isAdmin: false } }) }));
vi.mock("@/lib/apps", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/apps")>()),
  useActiveApp: () => ({ ...lens, apps: [], setActiveApp: vi.fn() }),
}));
vi.mock("@/lib/pins", () => ({ usePins: () => ({ pins: lens.pins, toggle: vi.fn() }) }));
vi.mock("@/lib/metadata/hooks", () => ({
  useEntities: () => ({
    data: [
      { name: "crm_accounts", label: "Khách hàng" },
      { name: "crm_deals", label: "Cơ hội" },
      { name: "payroll", label: "Bảng lương" },
    ],
  }),
}));
vi.mock("@/lib/metadata/permissions", () => ({ usePermissions: () => ({ can: (_a: string, e: string) => e !== "payroll" }) }));
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  listPendingApprovals: vi.fn(async () => []),
  getNotificationUnreadCount: vi.fn(async () => 0),
}));

const TABS: NavTab[] = [
  { href: "/home", label: "Trang chủ" },
  { href: "/approvals", label: "Phê duyệt", count: 3, countLabel: "3 yêu cầu đang chờ duyệt" },
  { href: "/notifications", label: "Thông báo", count: 0 },
  { href: "/crm_accounts", label: "Khách hàng" },
  { href: "/crm_deals", label: "Cơ hội" },
  { href: "/tickets", label: "Phiếu hỗ trợ" },
];

function renderNav(over: Partial<GlobalNavigationProps> = {}) {
  const onNavigate = vi.fn();
  const view = render(
    <GlobalNavigation
      appName="Quản lý Bán hàng"
      items={TABS}
      pathname="/home"
      launcher={<button type="button">waffle</button>}
      onNavigate={onNavigate}
      {...over}
    />,
  );
  return { onNavigate, ...view };
}

const bar = () => screen.getByRole("navigation", { name: "Điều hướng ứng dụng" });

describe("activeTabIndex / splitTabs", () => {
  it("picks the longest matching href", () => {
    const items = [{ href: "/dashboards", label: "a" }, { href: "/dashboards/x1", label: "b" }];
    expect(activeTabIndex(items, "/dashboards/x1/edit")).toBe(1);
    expect(activeTabIndex(items, "/dashboards")).toBe(0);
    expect(activeTabIndex(items, "/dashboardsX")).toBe(-1);
  });

  it("keeps an overflowed active tab in the last visible slot", () => {
    const [shown, rest] = splitTabs(TABS, 5, 3);
    expect(shown.map((i) => i.href)).toEqual(["/home", "/approvals", "/tickets"]);
    expect(rest.map((i) => i.href)).toEqual(["/notifications", "/crm_accounts", "/crm_deals"]);
    expect(splitTabs(TABS, 1, 3)[0]).toHaveLength(3);
    expect(splitTabs(TABS, 1, 99)[1]).toEqual([]);
  });
});

describe("GlobalNavigation", () => {
  it("renders the context bar: launcher + app name, then the tab row", () => {
    const { container } = renderNav();
    const root = container.firstElementChild!;
    expect(root).toHaveClass("slds-context-bar");
    expect(root.firstElementChild).toHaveClass("slds-context-bar__primary");
    const primary = root.firstElementChild!.firstElementChild!;
    expect(primary).toHaveClass("slds-context-bar__item");
    expect(primary).toHaveClass("slds-context-bar__dropdown-trigger", "slds-no-hover");
    expect(primary.querySelector(".slds-context-bar__icon-action")).toHaveTextContent("waffle");
    expect(primary.querySelector(".slds-context-bar__app-name .slds-truncate")).toHaveTextContent("Quản lý Bán hàng");
    expect(bar()).toHaveClass("slds-context-bar__secondary");
    const tabs = within(bar()).getAllByRole("link");
    expect(tabs.map((a) => a.getAttribute("href"))).toEqual(TABS.map((i) => i.href));
    for (const a of tabs) {
      expect(a).toHaveClass("slds-context-bar__label-action");
      expect(a.parentElement).toHaveClass("slds-context-bar__item");
    }
  });

  it("marks the tab of the current route active with the assistive prefix", () => {
    renderNav({ pathname: "/crm_accounts/42/edit" });
    const link = within(bar()).getByRole("link", { name: /Khách hàng/ });
    expect(link.parentElement).toHaveClass("slds-is-active");
    expect(link.querySelector(".slds-assistive-text")).toHaveTextContent("Trang hiện tại:");
    expect(bar().querySelectorAll(".slds-is-active")).toHaveLength(1);
  });

  it("shows a badge only for counts above zero, with assistive text", () => {
    renderNav();
    const approvals = within(bar()).getByRole("link", { name: /Phê duyệt/ });
    expect(approvals.querySelector(".slds-badge")).toHaveTextContent("3");
    expect(approvals).toHaveTextContent("3 yêu cầu đang chờ duyệt");
    expect(within(bar()).getByRole("link", { name: "Thông báo" }).querySelector(".slds-badge")).toBeNull();
  });

  it("moves tabs past the limit into the 'Thêm' menu, keyboard-operable", async () => {
    const { onNavigate } = renderNav({ maxVisible: 3 });
    expect(within(bar()).getAllByRole("link")).toHaveLength(3);
    const more = within(bar()).getByRole("button", { name: "Thêm mục điều hướng" });
    expect(more).toHaveClass("slds-context-bar__label-action");
    expect(more.closest("li")).toHaveClass("slds-context-bar__item", "slds-context-bar__dropdown-trigger");
    more.focus();
    await userEvent.keyboard("{Enter}");
    const menu = await screen.findByRole("menu", { name: "Thêm mục điều hướng" });
    expect(menu).toHaveClass("slds-dropdown_right", "slds-dropdown_length-10");
    expect(within(menu).getAllByRole("menuitem").map((i) => i.textContent)).toEqual(["Khách hàng", "Cơ hội", "Phiếu hỗ trợ"]);
    await waitFor(() => expect(within(menu).getAllByRole("menuitem")[0]).toHaveFocus());
    await userEvent.keyboard("{ArrowDown}{ArrowDown}{Enter}");
    expect(onNavigate).toHaveBeenCalledWith("/tickets");
  });

  it("keeps the active tab in the bar when it would overflow", () => {
    renderNav({ maxVisible: 3, pathname: "/tickets" });
    const links = within(bar()).getAllByRole("link");
    expect(links.map((a) => a.getAttribute("href"))).toEqual(["/home", "/approvals", "/tickets"]);
    expect(links[2].parentElement).toHaveClass("slds-is-active");
  });

  it("has a small-screen menu with every tab and its count", async () => {
    const { onNavigate } = renderNav();
    const btn = screen.getByRole("button", { name: "Mở menu điều hướng" });
    expect(btn.closest(".slds-context-bar__tertiary")).toHaveClass("md:hidden");
    expect(bar()).toHaveClass("max-md:hidden");
    btn.focus();
    await userEvent.keyboard("{Enter}");
    const menu = await screen.findByRole("menu", { name: "Mở menu điều hướng" });
    const labels = within(menu).getAllByRole("menuitem").map((i) => i.textContent);
    expect(labels).toEqual(["Trang chủ", "Phê duyệt (3)", "Thông báo", "Khách hàng", "Cơ hội", "Phiếu hỗ trợ"]);
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("menu")).toBeNull();
    expect(onNavigate).not.toHaveBeenCalled();
  });
});

function wrapper(qc: QueryClient) {
  return function W({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

describe("useShellNav", () => {
  beforeEach(() => {
    lens.activeApp = "*";
    lens.activeMeta = null;
    lens.activeNav = null;
    lens.pins = [];
  });

  const hrefs = (r: { current: { items: NavTab[] } }) => r.current.items.map((i) => i.href);

  it("All Items lens: overview tabs, then every viewable entity", () => {
    const { result } = renderHook(() => useShellNav(), { wrapper: wrapper(new QueryClient()) });
    expect(result.current.appName).toBe("Tất cả dữ liệu");
    expect(hrefs(result)).toEqual(["/home", "/dashboards", "/reports", "/approvals", "/notifications", "/crm_accounts", "/crm_deals"]);
  });

  it("app lens: home/approvals/notifications, pins first, then the app nav in its order", () => {
    lens.activeApp = "crm";
    lens.activeMeta = { label: "Quản lý Bán hàng" };
    lens.activeNav = [
      { type: "entity", refId: "e1", label: "Cơ hội", href: "/crm_deals" },
      { type: "dashboard", refId: "d1", label: "Doanh số", href: "/dashboards/d1" },
      { type: "entity", refId: "e2", label: "Khách hàng", href: "/crm_accounts" },
    ];
    lens.pins = ["payroll", "crm_accounts"];
    const { result } = renderHook(() => useShellNav(), { wrapper: wrapper(new QueryClient()) });
    expect(result.current.appName).toBe("Quản lý Bán hàng");
    // payroll is pinned but no longer viewable; crm_accounts is pinned so it leads and is not repeated.
    expect(hrefs(result)).toEqual(["/home", "/approvals", "/notifications", "/crm_accounts", "/crm_deals", "/dashboards/d1"]);
  });

  it("reads the badge counts from the shared query keys", () => {
    const qc = new QueryClient();
    qc.setQueryData(qk.pendingApprovals(), [{ id: "a1" }, { id: "a2" }]);
    qc.setQueryData(qk.notificationCount(), 7);
    const { result } = renderHook(() => useShellNav(), { wrapper: wrapper(qc) });
    const byHref = Object.fromEntries(result.current.items.map((i) => [i.href, i]));
    expect(byHref["/approvals"].count).toBe(2);
    expect(byHref["/approvals"].countLabel).toBe("2 yêu cầu đang chờ duyệt");
    expect(byHref["/notifications"].count).toBe(7);
    expect(byHref["/notifications"].countLabel).toBe("7 thông báo chưa đọc");
  });
});
