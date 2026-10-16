// EntityDirectory on SLDS (specs/038): the "All Items" page — page-headers/base + app-launcher
// tiles (initials avatar + name), one tile per live entity, each opening /:entity. No entity →
// honest empty state; only an admin is pointed at the data model (Studio is admin-only).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import type { EntityMeta } from "@/lib/api";

const seam = vi.hoisted(() => ({
  entities: [] as EntityMeta[],
  isAdmin: false,
  isOperator: false,
  isLoading: false,
  isError: false,
}));

vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { isAdmin: seam.isAdmin, isPlatformOperator: seam.isOperator } }) }));
vi.mock("@/lib/metadata/hooks", () => ({
  useEntities: () => ({ data: seam.entities, isLoading: seam.isLoading, isError: seam.isError, error: null }),
}));

import { EntityDirectory } from "@/components/screens/EntityDirectory";

describe("EntityDirectory (SLDS)", () => {
  beforeEach(() => {
    seam.entities = [];
    seam.isAdmin = false;
    seam.isOperator = false;
    seam.isLoading = false;
    seam.isError = false;
  });

  it("renders one tile per entity linking to its list, with initials and a count", () => {
    seam.entities = [
      { name: "crm_accounts", label: "Khách hàng", tableName: "obj_crm_accounts" },
      { name: "crm_leads", label: "Khách hàng tiềm năng", tableName: "obj_crm_leads" },
    ];
    render(<EntityDirectory />);
    expect(screen.getByRole("heading", { name: "Danh mục đối tượng" })).toBeInTheDocument();
    expect(screen.getByText(/2 đối tượng/)).toBeInTheDocument();
    // The whole tile is the link, named by the label alone (the initials are decoration).
    const link = screen.getByRole("link", { name: "Khách hàng tiềm năng" });
    expect(link).toHaveAttribute("href", "/crm_leads");
    expect(link).toHaveClass("slds-app-launcher__tile");
    // SLDS avatar initials: first + last word → the two "Khách hàng…" tiles stay distinct.
    expect(link.querySelector("abbr.slds-avatar__initials")).toHaveTextContent("KN");
    expect(screen.getByRole("link", { name: "Khách hàng" })).toHaveAttribute("href", "/crm_accounts");
  });

  it("empty for a non-admin: contact the administrator, no Studio link", () => {
    render(<EntityDirectory />);
    expect(screen.getByText("Chưa có đối tượng nào")).toBeInTheDocument();
    expect(screen.getByText(/liên hệ quản trị viên/)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /Mô hình dữ liệu/ })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Nexus hoạt động thế nào?" })).toHaveAttribute("href", "/about");
  });

  it("empty for the builder: points at the project's Dữ liệu page", () => {
    seam.isAdmin = true;
    seam.isOperator = true;
    render(<EntityDirectory />);
    expect(screen.getByRole("link", { name: /Mô hình dữ liệu/ })).toHaveAttribute("href", "/studio/du-an/northwind/dung/du-lieu");
  });

  it("empty for a customer admin (not the builder): no link into Studio", () => {
    seam.isAdmin = true;
    render(<EntityDirectory />);
    expect(screen.queryByRole("link", { name: /Mô hình dữ liệu/ })).not.toBeInTheDocument();
  });
});
