// specs/064 T142 (FE-S9-permissions-preview) / specs/074 f2-view-as-role-button: "Xem như vai này" on the open role.
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const ROLES = [
  { id: "r1", name: "crm_sales_rep", label: "Nhân viên kinh doanh" },
  { id: "r2", name: "front_desk", label: "Lễ tân" },
];
vi.mock("@/lib/admin", async (orig) => ({
  ...(await orig<typeof import("@/lib/admin")>()),
  ensureAuth: async () => undefined,
  listRoles: async () => ROLES,
  getRole: async (id: string) => ({ ...ROLES.find((r) => r.id === id), actions: [{}] }),
}));
vi.mock("./ACLMatrix", () => ({ ACLMatrix: () => <p>ma trận</p> }));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { toast } from "@/components/ui/Toaster";
import { RolesList } from "./RolesList";

const open = vi.fn();
beforeEach(() => {
  vi.clearAllMocks();
  open.mockResolvedValue(undefined);
});

describe("RolesList view-as", () => {
  it("no handler (standalone screen): no button", async () => {
    render(<RolesList />);
    await screen.findByText("ma trận");
    expect(screen.queryByRole("button", { name: /Xem như/ })).toBeNull();
  });

  it("button acts on the OPEN role and says which app it opens", async () => {
    render(<RolesList viewAs={{ appLabel: "CRM", open }} />);
    await screen.findByText("ma trận");
    const b = screen.getByRole("button", { name: "Xem như vai này" });
    expect(b.getAttribute("title")).toContain("CRM");
    fireEvent.click(b);
    await waitFor(() => expect(open).toHaveBeenCalledWith(ROLES[0]));
  });

  it("failure shows the sentence and unlocks the button", async () => {
    open.mockRejectedValue(new Error("boom"));
    render(<RolesList viewAs={{ appLabel: "CRM", open }} />);
    await screen.findByText("ma trận");
    fireEvent.click(screen.getByRole("button", { name: "Xem như vai này" }));
    await waitFor(() => expect(toast.error).toHaveBeenCalled());
    expect((screen.getByRole("button", { name: "Xem như vai này" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("double click starts one session", async () => {
    open.mockReturnValue(new Promise(() => {}));
    render(<RolesList viewAs={{ appLabel: "CRM", open }} />);
    await screen.findByText("ma trận");
    const b = screen.getByRole("button", { name: "Xem như vai này" });
    fireEvent.click(b);
    fireEvent.click(b);
    expect(open).toHaveBeenCalledTimes(1);
  });
});
