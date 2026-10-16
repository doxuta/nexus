// specs/062-customer-workspace L2b — the two "last path" keys (builder face / app face) are per scope
// `${tenantId}:${userId}`: where the builder left a face in Nexus is not where "App khách" returns to
// inside a customer workspace, and a pre-upgrade bare key is adopted once at home.
import { describe, it, expect, beforeEach, vi } from "vitest";
import { renderHook } from "@testing-library/react";
import { setWorkspace, clearWorkspace } from "@/lib/workspace";

const nav = vi.hoisted(() => ({ pathname: "/studio" }));
vi.mock("next/navigation", () => ({ usePathname: () => nav.pathname }));

import { useFacePaths } from "./ModeSwitch";

function signInHome() {
  window.localStorage.setItem("nexus_user", JSON.stringify({ id: "u-admin", tenantId: "t-northwind", email: "a@example.com" }));
  window.localStorage.setItem("nexus_token", "HOME-TOKEN");
}
function enterWorkspace() {
  setWorkspace({
    accessToken: "WS-1",
    expiresAt: "2099-01-01T00:00:00Z",
    homeUserId: "u-admin",
    session: { id: "ws-1", tenantId: "t-x", tenantSlug: "x", tenantName: "Khách X", openedAt: "2098-12-31T00:00:00Z", expiresAt: "2099-01-01T08:00:00Z" },
  });
}

beforeEach(() => {
  clearWorkspace();
  window.localStorage.clear();
  window.sessionStorage.clear();
  signInHome();
});

describe("useFacePaths — remembered face paths per scope", () => {
  it("a path remembered in Nexus is not offered inside a workspace (and is back at home)", () => {
    nav.pathname = "/studio/verticals";
    renderHook(() => useFacePaths("builder"));
    expect(window.localStorage.getItem("nexus.lastPath.builder:t-northwind:u-admin")).toBe("/studio/verticals");
    expect(window.localStorage.getItem("nexus.lastPath.builder")).toBeNull();

    enterWorkspace();
    nav.pathname = "/studio";
    const ws = renderHook(() => useFacePaths("builder"));
    expect(ws.result.current.builderHref).toBe("/studio");
    expect(window.localStorage.getItem("nexus.lastPath.builder:t-x:u-admin")).toBe("/studio");

    clearWorkspace();
    nav.pathname = "/home";
    const home = renderHook(() => useFacePaths("app"));
    expect(home.result.current.builderHref).toBe("/studio/verticals");
  });

  // specs/073 T426 / FR-088: the project code of a PREVIOUS session lives in that session's own key, so leaving the
  // session (or signing out) never makes the home "Studio" bridge point at a customer's page, and a customer page
  // remembered inside session A is not what the home bridge offers.
  it("the project page a session ended on is not where the home bridge returns", () => {
    window.localStorage.setItem("nexus.lastPath.builder:t-northwind:u-admin", "/studio/du-an/northwind/dung/du-lieu");
    enterWorkspace();
    nav.pathname = "/studio/du-an/x/dung/man-hinh/crm_accounts";
    renderHook(() => useFacePaths("builder"));
    expect(window.localStorage.getItem("nexus.lastPath.builder:t-x:u-admin")).toBe("/studio/du-an/x/dung/man-hinh/crm_accounts");

    clearWorkspace(); // "Về danh sách dự án"
    nav.pathname = "/home";
    expect(renderHook(() => useFacePaths("app")).result.current.builderHref).toBe("/studio/du-an/northwind/dung/du-lieu");
    expect(window.localStorage.getItem("nexus.lastPath.builder:t-northwind:u-admin")).toBe("/studio/du-an/northwind/dung/du-lieu");
  });

  it("signing out wipes the remembered builder path of every scope (it names customers), keeping the per-user recents", async () => {
    window.localStorage.setItem("nexus.lastPath.builder:t-x:u-admin", "/studio/du-an/x/dung/du-lieu");
    window.localStorage.setItem("nexus.studio.recentProjects:u-admin", JSON.stringify([{ slug: "x", name: "Khách X" }]));
    const { clearStudioLocalState } = await import("@/lib/studioLocalState");
    clearStudioLocalState();
    nav.pathname = "/home";
    expect(renderHook(() => useFacePaths("app")).result.current.builderHref).toBe("/studio");
    expect(window.localStorage.getItem("nexus.studio.recentProjects:u-admin")).not.toBeNull();
  });

  it("a pre-upgrade bare key is adopted once at home, never inside a workspace", () => {
    window.localStorage.setItem("nexus.lastPath.app", "/crm_accounts");
    enterWorkspace();
    nav.pathname = "/studio";
    expect(renderHook(() => useFacePaths("builder")).result.current.appHref).toBe("/home");
    expect(window.localStorage.getItem("nexus.lastPath.app")).toBe("/crm_accounts");

    clearWorkspace();
    expect(renderHook(() => useFacePaths("builder")).result.current.appHref).toBe("/crm_accounts");
    expect(window.localStorage.getItem("nexus.lastPath.app")).toBeNull();
  });
});

// specs/073 FR-053: the remembered builder path is checked against the LIVE routes, so a path that was deleted
// (an old /studio/apps/crm) can never become the "Studio" bridge target.
describe("useFacePaths — remembered builder path must still exist", () => {
  const remember = (p: string) => window.localStorage.setItem("nexus.lastPath.builder:t-northwind:u-admin", p);
  const bridge = () => {
    nav.pathname = "/home";
    return renderHook(() => useFacePaths("app")).result.current.builderHref;
  };

  it.each([
    "/studio",
    "/studio/verticals",
    "/studio/du-an/northwind/dung/du-lieu",
    "/studio/du-an/acme/dung/man-hinh?app=crm",
  ])("keeps %s", (p) => {
    remember(p);
    expect(bridge()).toBe(p);
  });

  it.each(["/studio/apps/crm", "/studio/model", "/studio/du-an/BAD SLUG/dung/du-lieu", "/studio/du-an", "/studio/verticals/../x", "/studio/nope", "/studio/moi", "/studio/tai-khoan", "/studio/du-an/northwind/khong-co"])(
    "falls back to /studio for %s",
    (p) => {
      remember(p);
      expect(bridge()).toBe("/studio");
    },
  );
});
