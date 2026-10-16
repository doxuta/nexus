// lib/workspace.ts — the builder's workspace-session state of ONE tab (specs/062-customer-workspace L2, T031).
// The contract: it lives in sessionStorage ONLY (localStorage is shared by every tab, even with
// "Ghi nhớ đăng nhập" on — a session token there would turn the builder's other tabs into the customer).
import { describe, it, expect, beforeEach, vi } from "vitest";
import {
  WORKSPACE_STORAGE_KEY, clearWorkspace, getWorkspace, inWorkspace, setWorkspace, subscribeWorkspace, type WorkspaceState,
} from "@/lib/workspace";

const STATE: WorkspaceState = {
  accessToken: "WS-TOK",
  expiresAt: "2099-01-01T00:00:00Z",
  homeUserId: "u-admin",
  session: {
    id: "ws-1", tenantId: "t-x", tenantSlug: "x", tenantName: "Khách X",
    openedAt: "2098-12-31T00:00:00Z", expiresAt: "2099-01-01T08:00:00Z",
  },
};

describe("lib/workspace", () => {
  beforeEach(() => {
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("stores the state in sessionStorage only — never localStorage, even with nexus_remember=1", () => {
    window.localStorage.setItem("nexus_remember", "1");
    setWorkspace(STATE);
    expect(window.sessionStorage.getItem(WORKSPACE_STORAGE_KEY)).not.toBeNull();
    expect(window.localStorage.getItem(WORKSPACE_STORAGE_KEY)).toBeNull();
    expect(getWorkspace()).toEqual(STATE);
  });

  it("inWorkspace() follows the state: false → true on set, true → false on clear", () => {
    expect(inWorkspace()).toBe(false);
    setWorkspace(STATE);
    expect(inWorkspace()).toBe(true);
    clearWorkspace();
    expect(inWorkspace()).toBe(false);
    expect(getWorkspace()).toBeNull();
  });

  it("garbage or token-less state reads as 'not in a workspace' (fail closed to home, never to a half state)", () => {
    window.sessionStorage.setItem(WORKSPACE_STORAGE_KEY, "{not json");
    expect(getWorkspace()).toBeNull();
    window.sessionStorage.setItem(WORKSPACE_STORAGE_KEY, JSON.stringify({ ...STATE, accessToken: "" }));
    expect(inWorkspace()).toBe(false);
  });

  it("notifies subscribers on set and clear (the banner re-arms its timer from this)", () => {
    const fn = vi.fn();
    const off = subscribeWorkspace(fn);
    setWorkspace(STATE);
    clearWorkspace();
    expect(fn).toHaveBeenCalledTimes(2);
    off();
    setWorkspace(STATE);
    expect(fn).toHaveBeenCalledTimes(2);
  });
});
