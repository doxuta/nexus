// specs/073 kh5 T315: inside the Studio, ⌘K offers the new front page and the workshop's first page (via studioHref).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

const nav = vi.hoisted(() => ({ push: vi.fn() }));
vi.mock("next/navigation", () => ({ usePathname: () => "/studio" }));
vi.mock("@/lib/navigation-blocker", () => ({ useGuardedRouter: () => ({ push: nav.push }) }));
vi.mock("@/lib/metadata/hooks", () => ({ useEntities: () => ({ data: [] }) }));
vi.mock("@/lib/metadata/permissions", () => ({ usePermissions: () => ({ can: () => false }) }));
vi.mock("@/lib/auth/AuthProvider", () => ({
  useAuth: () => ({ user: { id: "u1", isAdmin: true, isPlatformOperator: true }, logout: vi.fn() }),
}));
vi.mock("@/lib/pins", () => ({ usePins: () => ({ pins: [] }) }));
vi.mock("@/lib/recents", () => ({ loadRecentRoutes: () => [] }));
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  searchAll: vi.fn(async () => []),
}));

import { resetStudioContextForTests } from "@/lib/studioContext";
import { CommandPalette } from "./CommandPalette";

beforeEach(() => {
  resetStudioContextForTests();
  nav.push.mockReset();
  window.localStorage.setItem("nexus_tenant", "northwind");
});

function open() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <CommandPalette open onClose={() => {}} />
    </QueryClientProvider>,
  );
}

describe("CommandPalette in the Studio", () => {
  it("offers 'Dự án khách' and the workshop, and the workshop goes to its first page", async () => {
    open();
    expect(await screen.findByText("Dự án khách")).toBeInTheDocument();
    expect(screen.queryByText("Danh sách dự án")).toBeNull();
    fireEvent.click(screen.getByText("Xưởng Nexus"));
    expect(nav.push).toHaveBeenCalledWith("/studio/du-an/northwind/dung/du-lieu");
  });
});
