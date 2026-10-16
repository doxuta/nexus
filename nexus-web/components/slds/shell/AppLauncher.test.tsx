// AppLauncher — blueprint app-launcher/base (SLDS 2.264.1) on @radix-ui/react-dialog.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ALL_ITEMS } from "@/lib/apps";
import { AppLauncher, ShellAppLauncher, type AppLauncherProps } from "./AppLauncher";

const session = vi.hoisted(() => ({ user: null as null | Record<string, unknown>, setActiveApp: vi.fn() }));

vi.mock("next/navigation", () => ({ usePathname: () => "/home", useRouter: () => ({ push: vi.fn() }) }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: session.user }) }));
vi.mock("@/lib/apps", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/apps")>()),
  useActiveApp: () => ({
    apps: [{ id: "a1", name: "crm", label: "Quản lý Bán hàng", icon: "briefcase", color: "ac", isDefault: true, sortOrder: 0 }],
    activeApp: "crm",
    setActiveApp: session.setActiveApp,
  }),
}));
vi.mock("@/lib/metadata/hooks", () => ({
  useEntities: () => ({ data: [{ name: "crm_accounts", label: "Khách hàng" }, { name: "payroll", label: "Bảng lương" }] }),
}));
vi.mock("@/lib/metadata/permissions", () => ({ usePermissions: () => ({ can: (_a: string, e: string) => e !== "payroll" }) }));

const APPS = [
  { name: "crm", label: "Quản lý Bán hàng", icon: "briefcase", color: "ac" },
  { name: "helpdesk", label: "Chăm sóc khách hàng", icon: "ticket", color: "wn" },
];
const ITEMS = [
  { href: "/crm_accounts", label: "Khách hàng" },
  { href: "/crm_deals", label: "Cơ hội" },
  { href: "/tickets", label: "Phiếu hỗ trợ" },
];

function renderLauncher(over: Partial<AppLauncherProps> = {}) {
  const onSelectApp = vi.fn();
  render(<AppLauncher apps={APPS} activeApp="crm" items={ITEMS} onSelectApp={onSelectApp} {...over} />);
  return { onSelectApp };
}

const waffle = () => screen.getByRole("button", { name: "Mở trình chọn ứng dụng" });

describe("AppLauncher", () => {
  it("renders the waffle trigger from CSS spans", () => {
    renderLauncher();
    const btn = waffle();
    expect(btn).toHaveClass("slds-button", "slds-icon-waffle_container", "slds-context-bar__button");
    expect(btn.querySelector(".slds-icon-waffle")!.children).toHaveLength(9);
    expect(btn).toHaveAttribute("aria-haspopup", "dialog");
  });

  it("opens the launcher modal with the blueprint header, tiles and items", async () => {
    renderLauncher();
    await userEvent.click(waffle());
    const dialog = screen.getByRole("dialog", { name: "Trình chọn ứng dụng" });
    expect(dialog).toHaveClass("slds-modal", "slds-fade-in-open", "slds-modal_large", "slds-app-launcher");
    expect(dialog).toHaveAttribute("aria-modal", "true");
    expect(dialog.querySelector(".slds-modal__header")).toHaveClass("slds-grid", "slds-grid_align-spread", "slds-grid_vertical-align-center");
    expect(screen.getByRole("heading", { name: "Trình chọn ứng dụng" }).tagName).toBe("H2");
    expect(document.querySelector(".slds-backdrop.slds-backdrop_open")).not.toBeNull();
    // Type-to-find: focus starts in the search box, not on the close button.
    await waitFor(() => expect(screen.getByRole("searchbox", { name: "Tìm ứng dụng hoặc mục" })).toHaveFocus());

    const tiles = dialog.querySelectorAll(".slds-app-launcher__tile");
    // Two apps + the "Tất cả dữ liệu" lens tile.
    expect(tiles).toHaveLength(3);
    expect(tiles[0].querySelector(".slds-app-launcher__tile-figure .slds-avatar.slds-avatar_large .slds-icon-custom-27")).not.toBeNull();
    expect(tiles[1].querySelector(".slds-icon-custom-16")).not.toBeNull();
    expect(within(tiles[0] as HTMLElement).getByRole("link", { name: "Quản lý Bán hàng" })).toHaveAttribute("aria-current", "true");
    expect(within(tiles[0] as HTMLElement).getByText("Đang dùng")).toHaveClass("slds-badge", "slds-badge_lightest");

    const items = within(dialog).getByRole("button", { name: "Tất cả mục" });
    expect(items).toHaveAttribute("aria-expanded", "true");
    expect(within(dialog).getByRole("link", { name: "Phiếu hỗ trợ" })).toHaveClass("slds-truncate");
  });

  it("chooses an app: sets the lens, goes home and closes", async () => {
    const { onSelectApp } = renderLauncher();
    await userEvent.click(waffle());
    const link = screen.getByRole("link", { name: "Chăm sóc khách hàng" });
    expect(link).toHaveAttribute("href", "/home");
    await userEvent.click(link);
    expect(onSelectApp).toHaveBeenCalledWith("helpdesk");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("offers the All Items lens as a tile that leads to /entities", async () => {
    const { onSelectApp } = renderLauncher();
    await userEvent.click(waffle());
    const link = screen.getByRole("link", { name: "Tất cả dữ liệu" });
    expect(link).toHaveAttribute("href", "/entities");
    await userEvent.click(link);
    expect(onSelectApp).toHaveBeenCalledWith(ALL_ITEMS);
  });

  it("filters tiles and items, ignoring case and Vietnamese diacritics", async () => {
    renderLauncher();
    await userEvent.click(waffle());
    await userEvent.type(screen.getByRole("searchbox", { name: "Tìm ứng dụng hoặc mục" }), "khach");
    const dialog = screen.getByRole("dialog");
    expect(dialog.querySelectorAll(".slds-app-launcher__tile")).toHaveLength(1);
    expect(screen.getByRole("link", { name: "Chăm sóc khách hàng" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Khách hàng" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Cơ hội" })).toBeNull();
    await userEvent.clear(screen.getByRole("searchbox"));
    await userEvent.type(screen.getByRole("searchbox"), "zzz");
    expect(screen.getAllByText("Không có mục nào khớp.")).toHaveLength(2);
  });

  it("collapses a section from its title button", async () => {
    renderLauncher();
    await userEvent.click(waffle());
    const toggle = screen.getByRole("button", { name: "Tất cả mục" });
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle.closest(".slds-section")).not.toHaveClass("slds-is-open");
    expect(screen.queryByRole("link", { name: "Cơ hội" })).toBeNull();
  });

  it("closes on Escape and returns focus to the waffle", async () => {
    renderLauncher();
    waffle().focus();
    await userEvent.keyboard("{Enter}");
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    await waitFor(() => expect(waffle()).toHaveFocus());
  });

  it("closes from the close button", async () => {
    renderLauncher();
    await userEvent.click(waffle());
    await userEvent.click(screen.getByRole("button", { name: "Huỷ và đóng" }));
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

function wrap() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ShellAppLauncher />
    </QueryClientProvider>,
  );
}

describe("ShellAppLauncher", () => {
  beforeEach(() => session.setActiveApp.mockReset());

  it("lists dashboards, reports and only the entities the user can view", async () => {
    session.user = { id: "u2", isAdmin: false };
    wrap();
    await userEvent.click(waffle());
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByRole("link", { name: "Bảng điều khiển" })).toHaveAttribute("href", "/dashboards");
    expect(within(dialog).getByRole("link", { name: "Khách hàng" })).toBeInTheDocument();
    expect(within(dialog).queryByRole("link", { name: "Bảng lương" })).toBeNull();
    // No builder actions for a rep.
    expect(within(dialog).queryByRole("button", { name: "Tạo ứng dụng" })).toBeNull();
    await userEvent.click(within(dialog).getByRole("link", { name: "Quản lý Bán hàng" }));
    expect(session.setActiveApp).toHaveBeenCalledWith("crm");
  });

  it("keeps app create/manage away from customer admins (the API is operator-only)", async () => {
    session.user = { id: "u1", isAdmin: true, isPlatformOperator: false };
    wrap();
    await userEvent.click(waffle());
    expect(screen.queryByRole("button", { name: "Tạo ứng dụng" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Quản lý ứng dụng" })).toBeNull();
  });

  it("gives platform operators create and manage", async () => {
    session.user = { id: "u0", isAdmin: true, isPlatformOperator: true };
    wrap();
    await userEvent.click(waffle());
    expect(screen.getByRole("button", { name: "Tạo ứng dụng" })).toHaveClass("slds-button_neutral");
    expect(screen.getByRole("link", { name: "Quản lý ứng dụng" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/man-hinh/app");
  });
});
