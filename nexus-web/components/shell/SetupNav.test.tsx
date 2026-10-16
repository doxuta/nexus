// SetupNav — the Setup column of /admin/* (specs/038 T047, blueprint vertical-navigation/list).
// Pins: the same pages as the header's Setup gear (one list, two surfaces), the current page marked,
// and the operator-only doors (API keys, Doctor) hidden from a customer admin.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { SetupNav } from "./SetupNav";

const nav = vi.hoisted(() => ({ pathname: "/admin/roles" }));
const session = vi.hoisted(() => ({ user: { id: "u1", isAdmin: true, isPlatformOperator: false } as Record<string, unknown> }));
vi.mock("next/navigation", () => ({ usePathname: () => nav.pathname }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: session.user }) }));

describe("SetupNav", () => {
  beforeEach(() => {
    nav.pathname = "/admin/roles";
    session.user = { id: "u1", isAdmin: true, isPlatformOperator: false };
  });

  it("lists the admin pages and marks the current one", () => {
    render(<SetupNav />);
    const setup = screen.getByRole("navigation", { name: "Thiết lập quản trị" });
    const links = within(setup).getAllByRole("link");
    expect(links.map((l) => l.textContent)).toEqual([
      "Người dùng",
      "Vai trò & Phân quyền",
      "Nhóm",
      "Phân quyền Field",
      "SSO / Đăng nhập một lần",
      "Kho khoá kết nối",
      "Lịch làm việc",
      "Nhật ký",
      "Việc nền",
      "Nhật ký email",
      "Mức dùng",
      "Sức khoẻ",
      "Chia sẻ",
    ]);
    const current = within(setup).getByRole("link", { name: "Vai trò & Phân quyền" });
    expect(current).toHaveAttribute("aria-current", "page");
    expect(current).toHaveAttribute("href", "/admin/roles");
    expect(within(setup).getByRole("link", { name: "Người dùng" })).not.toHaveAttribute("aria-current");
  });

  it("marks the parent page on a nested admin route", () => {
    nav.pathname = "/admin/users/123";
    render(<SetupNav />);
    expect(screen.getByRole("link", { name: "Người dùng" })).toHaveAttribute("aria-current", "page");
  });

  it("no longer links Doctor or API keys: they live in the project frame (specs/073 kh7)", () => {
    session.user = { id: "u1", isAdmin: true, isPlatformOperator: true };
    render(<SetupNav />);
    expect(screen.queryByRole("link", { name: "Khoá API" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Bác sĩ hệ thống" })).toBeNull();
  });
});
