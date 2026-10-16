// specs/078 lát u7-du-lieu-danh-sach-bang T056: the REAL LayoutList inside EmbeddedSurface (no mock of the list).
import { cleanup, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { EMBEDDED_REGISTRY } from "@/test/studio-embedded-registry";
import { renderEmbedded } from "@/test/studioEmbeddedHarness";

const push = vi.fn();
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { id: "u-1", isAdmin: true }, isAuthenticated: true }) }));
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push, replace: vi.fn(), back: vi.fn(), prefetch: vi.fn() }),
  usePathname: () => "/studio",
  useSearchParams: () => new URLSearchParams(),
}));

const base = "/studio/du-an/northwind/van-hanh/du-lieu/crm_accounts";
const realFetch = global.fetch;

beforeEach(() => {
  push.mockClear();
  window.localStorage.setItem("nexus_remember", "1");
  window.localStorage.setItem("nexus_token", "test-token");
});
afterEach(() => {
  cleanup();
  global.fetch = realFetch;
  window.localStorage.clear();
});

const entry = () => EMBEDDED_REGISTRY.find((e) => e.id === "EntityRecordsPage")!;

describe("EntityRecordsPage embedded (real LayoutList)", () => {
  it("one h2, no card, no dialog on 'Tạo mới'; create and trash go to Studio routes", async () => {
    const { container } = await renderEmbedded(entry());
    expect(container.querySelectorAll("article.slds-card")).toHaveLength(0);
    // The filter dock stays mounted (slds-hide) so the toggle can name it; only the visible heading counts.
    expect([...container.querySelectorAll("h2")].filter((h) => !h.closest(".slds-hide"))).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: "Tạo mới" })).toHaveLength(1);
    await userEvent.click(screen.getByRole("button", { name: "Tạo mới" }));
    expect(push).toHaveBeenLastCalledWith(`${base}/moi`);
    expect(document.querySelectorAll("[role=dialog]")).toHaveLength(0);
    await userEvent.click(screen.getByRole("button", { name: "Thùng rác" }));
    expect(push).toHaveBeenLastCalledWith(`${base}/thung-rac`);
  }, 30000);

  // Work surface width -> where the filter panel lives (FR-052). jsdom has no layout, so the observer is stubbed.
  const withWidth = (w: number) => {
    class RO {
      constructor(private cb: ResizeObserverCallback) {}
      observe() { this.cb([{ contentRect: { width: w } } as ResizeObserverEntry], this as unknown as ResizeObserver); }
      disconnect() {}
      unobserve() {}
    }
    vi.stubGlobal("ResizeObserver", RO);
  };
  afterEach(() => vi.unstubAllGlobals());

  it(">= 1040px: the panel stays a column of the surface (no popover)", async () => {
    withWidth(1100);
    const { container } = await renderEmbedded(entry());
    await userEvent.click(screen.getByRole("button", { name: "Bộ lọc" }));
    expect(container.querySelector(".slds-filters")).not.toBeNull();
    expect(document.querySelectorAll("section.slds-popover")).toHaveLength(0);
  }, 30000);

  it("< 1040px: the panel opens in a Popover anchored to the 'Bộ lọc' button", async () => {
    withWidth(900);
    await renderEmbedded(entry());
    expect(document.querySelectorAll("section.slds-popover")).toHaveLength(0);
    await userEvent.click(screen.getByRole("button", { name: "Bộ lọc" }));
    expect(document.querySelectorAll("section.slds-popover .slds-filters")).toHaveLength(1);
  }, 30000);
});
