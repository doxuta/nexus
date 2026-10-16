// RequireUserManager (specs/065 lát B): fail-closed while /auth/me loads; a person who is not a customer owner
// gets a toast and /home; an owner sees the page. (The server re-checks every request regardless.)
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { RequireUserManager } from "@/lib/auth/guards";

const h = vi.hoisted(() => ({
  auth: { user: null as null | Record<string, unknown>, isLoading: false },
  replace: vi.fn(),
  info: vi.fn(),
}));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => h.auth }));
vi.mock("next/navigation", () => ({ useRouter: () => ({ replace: h.replace }) }));
vi.mock("@/components/ui/Toaster", () => ({ toast: { info: h.info } }));

describe("RequireUserManager", () => {
  beforeEach(() => {
    h.replace.mockClear();
    h.info.mockClear();
  });

  it("renders nothing and does not redirect while loading", () => {
    h.auth = { user: null, isLoading: true };
    render(<RequireUserManager><p>trang</p></RequireUserManager>);
    expect(screen.queryByText("trang")).not.toBeInTheDocument();
    expect(h.replace).not.toHaveBeenCalled();
  });

  it("a plain user and an admin are sent to /home with a toast", () => {
    for (const user of [{ id: "u", isAdmin: false }, { id: "a", isAdmin: true, canManageUsers: false }]) {
      h.replace.mockClear();
      h.info.mockClear();
      h.auth = { user, isLoading: false };
      const { unmount } = render(<RequireUserManager><p>trang</p></RequireUserManager>);
      expect(screen.queryByText("trang")).not.toBeInTheDocument();
      expect(h.replace).toHaveBeenCalledWith("/home");
      expect(h.info).toHaveBeenCalledWith("Mục này dành cho chủ khách.");
      unmount();
    }
  });

  it("an owner sees the page", () => {
    h.auth = { user: { id: "o", isAdmin: false, canManageUsers: true }, isLoading: false };
    render(<RequireUserManager><p>trang</p></RequireUserManager>);
    expect(screen.getByText("trang")).toBeInTheDocument();
    expect(h.replace).not.toHaveBeenCalled();
  });
});
