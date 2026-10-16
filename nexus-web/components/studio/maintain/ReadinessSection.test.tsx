// specs/079 f7-kham-san-sang: per-app readiness under the health check — bounded reads, honest "not read", links only to mounted pages.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { AppReadiness } from "@/lib/api";

const h = vi.hoisted(() => ({ apps: [] as { id: string; name: string; label: string }[], detail: (_id: string): unknown => ({}), live: 0, peak: 0, mounted: {} as Record<string, boolean> }));
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  adminApps: vi.fn(async () => h.apps),
  adminApp: vi.fn(async (id: string) => {
    h.live++; h.peak = Math.max(h.peak, h.live);
    await new Promise((r) => setTimeout(r, 5));
    h.live--;
    const d = h.detail(id);
    if (d instanceof Error) throw d;
    return d;
  }),
}));
vi.mock("@/lib/studioRoutes", async (orig) => {
  const actual = await orig<typeof import("@/lib/studioRoutes")>();
  return { ...actual, isMounted: (id: never) => (id in h.mounted ? h.mounted[id] : actual.isMounted(id)) };
});
vi.mock("../useProjectQuery", async () => {
  const { useQuery } = await import("@tanstack/react-query");
  return { useProjectQuery: (_s: string, key: unknown[], fn: () => Promise<unknown>) => useQuery({ queryKey: key, queryFn: fn }) };
});
vi.mock("../AclBackfillControl", () => ({ AclBackfillControl: () => <button>Bù quyền còn thiếu</button> }));
import { ReadinessSection } from "./ReadinessSection";

const rd = (o: Partial<AppReadiness>): AppReadiness => ({
  entityCount: 2, entitiesMissingFields: [], allEntitiesHaveFields: true, hasNonAdminViewRole: true, hasNonAdminUserAssigned: true, ready: true, ...o,
});
const view = () =>
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ReadinessSection slug="acme" /></QueryClientProvider>);
const app = (n: number) => ({ id: `a${n}`, name: `app${n}`, label: `App ${n}` });

beforeEach(() => { h.apps = [app(1), app(2)]; h.detail = () => ({ readiness: rd({}) }); h.live = 0; h.peak = 0; h.mounted = { "van-hanh/suc-khoe": false, "dung/man-hinh/app": false }; });

describe("ReadinessSection", () => {
  it("two apps ⇒ two groups, each condition a row with words", async () => {
    h.detail = (id) => ({ readiness: id === "a1" ? rd({ allEntitiesHaveFields: false, entitiesMissingFields: ["order"], ready: false }) : rd({}) });
    view();
    await screen.findByText("order");
    expect(screen.getAllByRole("heading", { level: 3 })).toHaveLength(2);
    expect(within(screen.getAllByRole("listitem")[0]).getByText("Chưa đạt")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Mở Dữ liệu" }).getAttribute("href")).toContain("dung/du-lieu");
  });

  it("six apps ⇒ never more than 4 detail reads in flight", async () => {
    h.apps = [1, 2, 3, 4, 5, 6].map(app);
    view();
    await waitFor(() => expect(screen.getAllByRole("heading", { level: 3 })).toHaveLength(6));
    expect(h.peak).toBeLessThanOrEqual(4);
    expect(h.peak).toBeGreaterThan(1);
  });

  it("a detail that fails or has no readiness is 'not read', never green", async () => {
    h.detail = (id) => (id === "a1" ? new Error("boom") : {});
    view();
    await waitFor(() => expect(screen.getAllByText("Chưa đọc được độ sẵn sàng của app này.")).toHaveLength(2));
    expect(screen.queryByText("Đạt")).not.toBeInTheDocument();
  });

  it("no apps ⇒ empty state; the create link only when its route is mounted", async () => {
    h.apps = [];
    const a = view();
    await screen.findByText(/chưa có app nào/);
    expect(screen.queryByRole("link", { name: "Tạo app" })).not.toBeInTheDocument();
    a.unmount();
    h.mounted["dung/man-hinh/app"] = true;
    view();
    expect(await screen.findByRole("link", { name: "Tạo app" })).toBeInTheDocument();
  });

  it("the health link appears only when van-hanh/suc-khoe is mounted", async () => {
    const a = view();
    await screen.findAllByRole("heading", { level: 3 });
    expect(screen.queryByRole("link", { name: "Xem sức khoẻ và mức dùng" })).toBeNull();
    a.unmount();
    h.mounted["van-hanh/suc-khoe"] = true;
    view();
    expect(await screen.findByRole("link", { name: "Xem sức khoẻ và mức dùng" })).toBeInTheDocument();
  });
});
