// ScreensPage (specs/074 lát f2-screens-header; specs/075 L2b: form, record page and list are all the ScreenEditor,
// no LayoutBuilder is mounted).
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { EntityMeta } from "@/lib/api";

vi.mock("next/navigation", () => ({
  usePathname: () => "/studio/du-an/northwind/dung/man-hinh",
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), back: vi.fn(), forward: vi.fn(), refresh: vi.fn(), prefetch: vi.fn() }),
}));
const se = vi.fn();
vi.mock("@/components/studio/screen/ScreenEditor", () => ({ ScreenEditor: (p: unknown) => { se(p); return <div data-testid="se" />; } }));
const listEntities = vi.fn();
const appsCatalog = vi.fn();
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  listEntities: (...a: unknown[]) => listEntities(...a),
  appsCatalog: (...a: unknown[]) => appsCatalog(...a),
}));
import { ScreensPage } from "./ScreensPage";

const ent = (name: string, label: string): EntityMeta => ({ id: name, name, label, tableName: `obj_${name}` });
function mount(props: { entity?: string; man?: string } = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}><ScreensPage slug="northwind" {...props} /></QueryClientProvider>);
}

describe("ScreensPage", () => {
  beforeEach(() => { vi.clearAllMocks(); appsCatalog.mockResolvedValue([]); });
  it("no tables -> EmptyState with a link to the Dữ liệu step, no editor", async () => {
    listEntities.mockResolvedValue([]);
    mount();
    expect(await screen.findByText("Chưa có màn hình nào")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Mở bước Dữ liệu" }).getAttribute("href")).toBe("/studio/du-an/northwind/dung/du-lieu");
    expect(screen.queryByTestId("se")).toBeNull();
  });
  it("the form screen is the new ScreenEditor with no flag (default = first table, form)", async () => {
    listEntities.mockResolvedValue([ent("crm_accounts", "Khách hàng"), ent("pm_tasks", "Công việc")]);
    mount();
    await screen.findByTestId("se");
    expect(se).toHaveBeenCalledWith(expect.objectContaining({ bang: "crm_accounts" }));
  });
  it("?man=form on a named table opens that table's ScreenEditor", async () => {
    listEntities.mockResolvedValue([ent("crm_accounts", "Khách hàng"), ent("pm_tasks", "Công việc")]);
    mount({ entity: "pm_tasks", man: "form" });
    await screen.findByTestId("se");
    expect(se).toHaveBeenCalledWith(expect.objectContaining({ bang: "pm_tasks" }));
  });
  it("?man=ho-so opens the ScreenEditor on the record page", async () => {
    listEntities.mockResolvedValue([ent("crm_accounts", "Khách hàng"), ent("pm_tasks", "Công việc")]);
    mount({ entity: "pm_tasks", man: "ho-so" });
    await screen.findByTestId("se");
    expect(se).toHaveBeenCalledWith(expect.objectContaining({ bang: "pm_tasks", man: "ho-so" }));
  });
  it("?man=danh-sach opens the ScreenEditor on the list", async () => {
    listEntities.mockResolvedValue([ent("crm_accounts", "Khách hàng")]);
    mount({ man: "danh-sach" });
    await screen.findByTestId("se");
    expect(se).toHaveBeenCalledWith(expect.objectContaining({ bang: "crm_accounts", man: "danh-sach" }));
  });
});
