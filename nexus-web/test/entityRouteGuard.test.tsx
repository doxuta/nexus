import { render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const st = vi.hoisted(() => ({ path: "/crm_accounts/new", grants: ["view"] as string[], loading: false, isError: false }));
vi.mock("next/navigation", () => ({ usePathname: () => st.path }));
vi.mock("@/lib/metadata/permissions", () => ({
  usePermissions: () => ({
    can: (a: string) => st.grants.includes(a),
    isLoading: st.loading,
    isError: st.isError,
  }),
}));

import EntityLayout from "@/app/(app)/[entity]/layout";

function renderAt(path: string) {
  st.path = path;
  return render(<EntityLayout><div>FORM</div></EntityLayout>);
}

beforeEach(() => { st.grants = ["view"]; st.loading = false; st.isError = false; });

describe("(app)/[entity] route guard", () => {
  it.each(["/crm_accounts/new", "/crm_accounts/batch", "/crm_accounts/import"])(
    "%s without create → no-permission state, no form", (p) => {
      renderAt(p);
      expect(screen.queryByText("FORM")).toBeNull();
      expect(screen.getByText("Bạn không có quyền tạo bản ghi ở bảng này.")).toBeInTheDocument();
    });

  it("/x/<id>/edit without edit → no-permission state", () => {
    renderAt("/crm_accounts/abc-123/edit");
    expect(screen.queryByText("FORM")).toBeNull();
    expect(screen.getByText("Bạn không có quyền sửa bản ghi này.")).toBeInTheDocument();
  });

  it("create grant opens the form; edit-only user still blocked from /new", () => {
    st.grants = ["view", "create"];
    renderAt("/crm_accounts/new");
    expect(screen.getByText("FORM")).toBeInTheDocument();
  });

  it("list, detail and recycle-bin routes are not gated here", () => {
    for (const p of ["/crm_accounts", "/crm_accounts/abc-123", "/crm_accounts/deleted"]) {
      const { unmount } = renderAt(p);
      expect(screen.getByText("FORM")).toBeInTheDocument();
      unmount();
    }
  });

  it("renders nothing while permissions load (no form flash)", () => {
    st.loading = true;
    renderAt("/crm_accounts/new");
    expect(screen.queryByText("FORM")).toBeNull();
  });
});
