// specs/062-customer-workspace L2b (T049/T050) — browser memory is keyed by `${tenantId}:${userId}`, so the
// builder's pins / recents / list prefs / active app / last paths in Nexus never show inside a customer
// workspace (and back), and a user who pinned before the upgrade keeps their pins in the HOME tenant.
import { describe, it, expect, beforeEach } from "vitest";
import { loadPins, savePins } from "./pins";
import { loadRecentRoutes, recordRouteVisit, loadRecentRecords, recordRecordVisit } from "./recents";
import { loadListPrefs, saveListPrefs, loadLastAppliedViewId, saveLastAppliedViewId, listPrefsKey } from "./listState";
import { loadStored, saveStored } from "./apps";
import { loadColWidths, saveColWidths } from "./dataGrid";
import { setWorkspace, clearWorkspace } from "./workspace";
import { setPreview } from "./previewSession";
import { applyBranding, clearBranding, readCachedBranding } from "./branding";

const HOME = { id: "u-admin", tenantId: "t-northwind", email: "admin@example.com" };

function signInHome(me: Record<string, unknown> = HOME) {
  window.localStorage.setItem("nexus_user", JSON.stringify(me));
  window.localStorage.setItem("nexus_token", "HOME-TOKEN");
}
function enterWorkspace(tenantId = "t-x") {
  setWorkspace({
    accessToken: "WS-1",
    expiresAt: "2099-01-01T00:00:00Z",
    homeUserId: HOME.id,
    // the workspace /auth/me profile carries the SAME user id as the builder — only the tenant differs
    me: { id: HOME.id, fullName: "Builder", email: HOME.email, isAdmin: true, tenantId },
    session: { id: "ws-1", tenantId, tenantSlug: "x", tenantName: "Khách X", openedAt: "2098-12-31T00:00:00Z", expiresAt: "2099-01-01T08:00:00Z" },
  });
}

beforeEach(() => {
  clearWorkspace();
  window.localStorage.clear();
  window.sessionStorage.clear();
  signInHome();
});

describe("scope = tenantId:userId — Nexus and a customer workspace do not share memory", () => {
  it("pins: pinned at Nexus are invisible in the workspace and vice versa", () => {
    savePins(["table_a"]);
    enterWorkspace();
    expect(loadPins()).toEqual([]);
    savePins(["table_b"]);
    clearWorkspace();
    expect(loadPins()).toEqual(["table_a"]);
  });

  it("recents (routes, records) are split the same way", () => {
    recordRouteVisit({ href: "/a", label: "A" });
    recordRecordVisit({ entity: "e", id: "1", label: "one" });
    enterWorkspace();
    expect(loadRecentRoutes()).toEqual([]);
    expect(loadRecentRecords()).toEqual([]);
    recordRouteVisit({ href: "/b", label: "B" });
    clearWorkspace();
    expect(loadRecentRoutes().map((r) => r.href)).toEqual(["/a"]);
  });

  it("list prefs, last applied view, active app and grid widths are split too", () => {
    saveListPrefs("accounts", { density: "compact" });
    saveLastAppliedViewId("accounts", "view-1");
    saveStored("crm");
    saveColWidths("g", { name: 200 });
    enterWorkspace();
    expect(loadListPrefs("accounts")).toEqual({});
    expect(loadLastAppliedViewId("accounts")).toBeNull();
    expect(loadStored()).toBeNull();
    expect(loadColWidths("g")).toEqual({});
    saveStored("ticketing");
    clearWorkspace();
    expect(loadStored()).toBe("crm");
    expect(loadListPrefs("accounts")).toEqual({ density: "compact" });
  });

  it("two workspaces of the same builder do not share either", () => {
    enterWorkspace("t-x");
    savePins(["x_table"]);
    enterWorkspace("t-y");
    expect(loadPins()).toEqual([]);
  });

  it("same userId in two tenants (home) ⇒ two memories", () => {
    savePins(["a"]);
    signInHome({ ...HOME, tenantId: "t-other" });
    expect(loadPins()).toEqual([]);
  });
});

describe("legacy keys (before the upgrade) are read ONCE for the home tenant", () => {
  it("pins/recents/list prefs/active app/grid widths made before the upgrade survive at home, then move", () => {
    window.localStorage.setItem(`nexus.pins.v1:${HOME.id}`, JSON.stringify(["old_pin"]));
    window.localStorage.setItem(`nexus.recent-routes.v1:${HOME.id}`, JSON.stringify([{ href: "/old", label: "Old", at: 1 }]));
    window.localStorage.setItem("list:accounts:view", JSON.stringify({ density: "relaxed" }));
    window.localStorage.setItem("list:accounts:lastView", "v-old");
    window.localStorage.setItem("nexus.activeApp", "legacy_app");
    window.localStorage.setItem(`nexus.gridColW.v1:${HOME.id}:g`, JSON.stringify({ name: 150 }));

    expect(loadPins()).toEqual(["old_pin"]);
    expect(loadRecentRoutes().map((r) => r.href)).toEqual(["/old"]);
    expect(loadListPrefs("accounts")).toEqual({ density: "relaxed" });
    expect(loadLastAppliedViewId("accounts")).toBe("v-old");
    expect(loadStored()).toBe("legacy_app");
    expect(loadColWidths("g")).toEqual({ name: 150 });

    // moved, not copied: the old key is gone and the new key carries the value
    expect(window.localStorage.getItem(`nexus.pins.v1:${HOME.id}`)).toBeNull();
    expect(window.localStorage.getItem(`nexus.pins.v1:${HOME.tenantId}:${HOME.id}`)).not.toBeNull();
    expect(window.localStorage.getItem("nexus.activeApp")).toBeNull();
    expect(window.localStorage.getItem(listPrefsKey("accounts"))).not.toBe("list:accounts:view");
  });

  it("a workspace never reads (or consumes) the legacy keys", () => {
    window.localStorage.setItem(`nexus.pins.v1:${HOME.id}`, JSON.stringify(["old_pin"]));
    window.localStorage.setItem("nexus.activeApp", "legacy_app");
    enterWorkspace();
    expect(loadPins()).toEqual([]);
    expect(loadStored()).toBeNull();
    expect(window.localStorage.getItem(`nexus.pins.v1:${HOME.id}`)).not.toBeNull();
    clearWorkspace();
    expect(loadPins()).toEqual(["old_pin"]);
  });

  it("an existing new-key value is never overwritten by a stale legacy key", () => {
    savePins(["new_pin"]);
    window.localStorage.setItem(`nexus.pins.v1:${HOME.id}`, JSON.stringify(["old_pin"]));
    expect(loadPins()).toEqual(["new_pin"]);
  });

  it("WRITE before READ still adopts the old recents (AppShell records a visit before ⌘K ever reads)", () => {
    window.localStorage.setItem(`nexus.recent-routes.v1:${HOME.id}`, JSON.stringify([{ href: "/old", label: "Old", at: 1 }]));
    window.localStorage.setItem(`nexus.recent-records.v1:${HOME.id}`, JSON.stringify([{ entity: "e", id: "0", label: "old", at: 1 }]));

    recordRouteVisit({ href: "/new", label: "New" });
    recordRecordVisit({ entity: "e", id: "1", label: "one" });

    expect(loadRecentRoutes().map((r) => r.href)).toEqual(["/new", "/old"]);
    expect(loadRecentRecords().map((r) => r.id)).toEqual(["1", "0"]);
    // the old keys are gone — nothing left behind as garbage
    expect(window.localStorage.getItem(`nexus.recent-routes.v1:${HOME.id}`)).toBeNull();
    expect(window.localStorage.getItem(`nexus.recent-records.v1:${HOME.id}`)).toBeNull();
  });

  it("a preview tab never reads (or consumes) the legacy keys", () => {
    window.localStorage.setItem(`nexus.pins.v1:${HOME.id}`, JSON.stringify(["old_pin"]));
    window.localStorage.setItem(`nexus.recent-routes.v1:${HOME.id}`, JSON.stringify([{ href: "/old", label: "Old", at: 1 }]));
    setPreview({
      token: "PV", sessionId: "pv-1", role: { id: "r", name: "rep" }, expiresAt: "2099-01-01T00:00:00Z", returnTo: "/studio",
      me: { id: HOME.id, email: HOME.email, tenantId: HOME.tenantId } as never,
    });
    expect(loadPins()).toEqual([]);
    expect(loadRecentRoutes()).toEqual([]);
    recordRouteVisit({ href: "/p", label: "P" });
    expect(window.localStorage.getItem(`nexus.pins.v1:${HOME.id}`)).not.toBeNull();
    expect(window.localStorage.getItem(`nexus.recent-routes.v1:${HOME.id}`)).not.toBeNull();
  });

  it("a cached user that gains its tenantId later (/auth/me) adopts the old keys then", () => {
    signInHome({ id: "u-old" });
    window.localStorage.setItem("nexus.pins.v1:u-old", JSON.stringify(["p"]));
    expect(loadPins()).toEqual(["p"]); // no tenant yet: old key kept as-is
    signInHome({ id: "u-old", tenantId: "t-northwind" });
    expect(loadPins()).toEqual(["p"]);
    expect(window.localStorage.getItem("nexus.pins.v1:u-old")).toBeNull();
    expect(window.localStorage.getItem("nexus.pins.v1:t-northwind:u-old")).not.toBeNull();
  });

  it("a cached user without tenantId keeps its old keys (nothing to scope by, nothing lost)", () => {
    signInHome({ id: "u-old" });
    window.localStorage.setItem("nexus.pins.v1:u-old", JSON.stringify(["p"]));
    expect(loadPins()).toEqual(["p"]);
  });
});

describe("branding cache (T050)", () => {
  it("inside a workspace the customer's branding is NOT written to nexus_branding", () => {
    applyBranding({ appName: "Nexus", accentColor: "#112233" });
    enterWorkspace();
    applyBranding({ appName: "Khách X", accentColor: "#445566" });
    expect(readCachedBranding().appName).toBe("Nexus");
    clearWorkspace();
    clearBranding();
  });
});
