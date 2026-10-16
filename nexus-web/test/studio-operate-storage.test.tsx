// specs/078 T012a(ii) + FR-132: the Vận hành screens run inside a frame that is shared between customers (one browser, one
// origin). A screen that writes customer data (record labels, names) to local/sessionStorage leaks it from customer A to
// the next project the builder opens. Only these keys may be written while a Vận hành screen renders:
//   list:<scope>:<entity>:view (view mode + density only), list:<scope>:<entity>:lastView (SavedViews), nexus.gridColW.v1:* (column widths).
// `nexus.recent-records.v1:*` (record labels) is the one that must never appear here.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup } from "@testing-library/react";
import { EMBEDDED_REGISTRY } from "./studio-embedded-registry";
import { renderEmbedded } from "./studioEmbeddedHarness";

vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { id: "u-1", isAdmin: true }, isAuthenticated: true }) }));
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), back: vi.fn(), prefetch: vi.fn() }),
  usePathname: () => "/studio",
  useSearchParams: () => new URLSearchParams(),
}));

// Keys a Vận hành screen may write. Anything else is a failure; adding a pattern here needs a reason in the spec (FR-132).
const ALLOWED = [/^list:(.+:)?[a-z][a-z0-9_]*:(view|lastView)$/, /^nexus\.gridColW\.v1:/];
// Auth bootstrap keys written by the test harness itself, not by a screen.
const HARNESS = new Set(["nexus_remember", "nexus_token"]);

const SCREENS = ["UserList", "AuditLog", "SsoProviders", "BackgroundJobs", "OperateOverview", "ApiKeys", "EmailLog", "LayoutList", "RecordPage", "RecycleBinScreen", "UserSessionsCard", "WorkflowRunHistory"];

const originalFetch = global.fetch;
beforeEach(() => {
  window.localStorage.setItem("nexus_remember", "1");
  window.localStorage.setItem("nexus_token", "test-token");
});
afterEach(() => {
  cleanup();
  global.fetch = originalFetch;
  window.localStorage.clear();
  window.sessionStorage.clear();
  vi.restoreAllMocks();
});

describe("Vận hành screens write only the allowed storage keys", () => {
  it("the screens named here are all in the registry (a rename cannot silently empty this test)", () => {
    const ids = EMBEDDED_REGISTRY.map((e) => e.id);
    for (const id of SCREENS) expect(ids, id).toContain(id);
  });

  it.each(SCREENS)("%s", async (id) => {
    const entry = EMBEDDED_REGISTRY.find((e) => e.id === id)!;
    const spy = vi.spyOn(Storage.prototype, "setItem");
    await renderEmbedded(entry);
    const keys = spy.mock.calls.map((c) => String(c[0])).filter((k) => !HARNESS.has(k));
    const stray = keys.filter((k) => !ALLOWED.some((re) => re.test(k)));
    expect(stray, `${id} wrote storage keys outside the allowed set`).toEqual([]);
    expect(keys.some((k) => k.startsWith("nexus.recent-records.v1:"))).toBe(false);
  });

  it("self-check: the filter has teeth (a record-label key is flagged)", () => {
    const k = "nexus.recent-records.v1:u-1";
    expect(ALLOWED.some((re) => re.test(k))).toBe(false);
  });
});
