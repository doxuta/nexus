// A view-only employee must not be offered actions that always 403.
//
// The permission hook and the <Can> gate already backed the sidebar, the kanban and the recycle
// bin; the list screen never asked them, so Tạo mới / Thêm nhiều / Nhập dữ liệu / Sửa hàng loạt /
// Xoá hàng loạt were shown to everyone and answered 403 on click. Offering an action that always
// fails reads as a broken app rather than as a boundary.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

vi.mock("@/lib/auth/AuthProvider", () => ({
  useAuth: () => ({ user: { id: "u1", isAdmin: false } }),
}));

const canMock = vi.fn();
vi.mock("@/lib/metadata/permissions", () => ({
  usePermissions: () => ({ can: canMock, grants: [], isLoading: false }),
}));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn().mockResolvedValue(undefined),
    listRecordsPage: vi.fn().mockResolvedValue({ records: [], pagination: { page: 1, limit: 50, total: 0, totalPages: 0 } }),
    listFields: vi.fn().mockResolvedValue([
      { id: "f1", name: "name", label: "Tên", fieldType: "text" },
    ]),
    getLayout: vi.fn().mockResolvedValue({ entityName: "crm_accounts", views: {} }),
    // Hermetic: SavedViews and useEntities call these on mount — mock them or the unit test hits a real :8080.
    listViews: vi.fn().mockResolvedValue([]),
    listEntities: vi.fn().mockResolvedValue([]),
  };
});

import { LayoutList } from "./LayoutList";

function renderList() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <LayoutList entity="crm_accounts" onCreate={() => {}} />
    </QueryClientProvider>,
  );
}

beforeEach(() => canMock.mockReset());

describe("LayoutList — action affordances follow real grants", () => {
  it("a view-only employee sees no create/import/batch buttons", async () => {
    canMock.mockImplementation((action: string) => action === "view");
    renderList();
    await waitFor(() => expect(screen.queryByText("Đang tải…")).not.toBeInTheDocument(), { timeout: 3000 })
      .catch(() => { /* the empty list may render immediately */ });

    expect(screen.queryByRole("button", { name: /Tạo mới/ })).not.toBeInTheDocument();
    expect(screen.queryByText("Thêm nhiều")).not.toBeInTheDocument();
    expect(screen.queryByText("Nhập dữ liệu")).not.toBeInTheDocument();
  });

  it("someone who may create sees them", async () => {
    canMock.mockReturnValue(true);
    renderList();
    await waitFor(() => {
      expect(screen.getByRole("button", { name: /Tạo mới/ })).toBeInTheDocument();
    }, { timeout: 3000 });
    // Blueprint page-headers/object-home: one visible action (New), the rest under "More Actions" —
    // three buttons ran over the list title at phone width.
    expect(screen.queryByRole("button", { name: "Thêm nhiều" })).not.toBeInTheDocument();
    fireEvent.keyDown(screen.getByRole("button", { name: "Thêm thao tác" }), { key: "Enter" });
    expect(screen.getByRole("menuitem", { name: "Thêm nhiều" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Nhập dữ liệu" })).toBeInTheDocument();
  });
});
