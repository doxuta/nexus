// specs/078 u7-cap-chu-khach-tung-nguoi: owner-tier switch on the user detail page.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { getUser, listRoles, listUserSessions, type AdminUser } from "@/lib/admin";
import { UserDetailStudio } from "./UserDetailStudio";

vi.mock("@/lib/admin", async (orig) => ({
  ...(await orig<typeof import("@/lib/admin")>()),
  getUser: vi.fn(),
  listRoles: vi.fn(),
  listUserSessions: vi.fn(),
  setUserManager: vi.fn(),
}));
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }) }));
vi.mock("@/lib/studioContext", () => ({ useStudioIdentity: () => ({ matches: () => true, me: { id: "u-me" } }) }));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(getUser).mockResolvedValue({ id: "u-an", tenantId: "t", email: "an@x.vn", fullName: "An Nguyễn", status: "active", isAdmin: false, canManageUsers: false } as AdminUser);
  vi.mocked(listRoles).mockResolvedValue([] as never);
  vi.mocked(listUserSessions).mockResolvedValue([] as never);
});

describe("owner tier confirm", () => {
  it("cancel returns focus to the checkbox, and the help says it applies without Lưu", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={qc}><UserDetailStudio slug="northwind" id="u-an" /></QueryClientProvider>);
    const box = await screen.findByRole("checkbox", { name: /Chủ khách/ });
    expect(screen.getByText(/không cần bấm Lưu/)).toBeInTheDocument();
    await userEvent.click(box);
    await userEvent.click(await screen.findByRole("button", { name: "Huỷ" }));
    expect(screen.getByRole("checkbox", { name: /Chủ khách/ })).toHaveFocus();
  });
});
