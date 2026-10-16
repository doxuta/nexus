// specs/073 T435 / FR-087: what the Studio leaves in the browser about customers is wiped when the signed-in person
// changes (sign-out, a fresh sign-in), except the per-user "recent projects" list.
import { beforeEach, describe, expect, it } from "vitest";
import { clearStudioLocalState } from "./studioLocalState";

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
});

describe("clearStudioLocalState", () => {
  it("removes the remembered builder path (bare and per-scope), the assistant flag, the pending brief and other nexus.studio.* keys", () => {
    const ls = window.localStorage;
    ls.setItem("nexus.lastPath.builder", "/studio/du-an/acme/dung/du-lieu");
    ls.setItem("nexus.lastPath.builder:t-1:u-1", "/studio/du-an/acme/dung/du-lieu");
    ls.setItem("nexus.studio.assistantOpen", "1");
    ls.setItem("nexus.studio.somethingElse:acme", "x");
    ls.setItem("nexus.ai-apply-state.v1:acme:s1", "{}"); // specs/076 FR-110
    window.sessionStorage.setItem("nexus.studio.pendingBrief", "tả bằng lời");
    window.sessionStorage.setItem("nexus.studio.other", "x");
    clearStudioLocalState();
    expect(ls.getItem("nexus.lastPath.builder")).toBeNull();
    expect(ls.getItem("nexus.lastPath.builder:t-1:u-1")).toBeNull();
    expect(ls.getItem("nexus.studio.assistantOpen")).toBeNull();
    expect(ls.getItem("nexus.studio.somethingElse:acme")).toBeNull();
    expect(ls.getItem("nexus.ai-apply-state.v1:acme:s1")).toBeNull();
    expect(window.sessionStorage.getItem("nexus.studio.pendingBrief")).toBeNull();
    expect(window.sessionStorage.getItem("nexus.studio.other")).toBeNull();
  });

  it("keeps the recent-projects list and everything that is not the Studio's", () => {
    const ls = window.localStorage;
    ls.setItem("nexus.studio.recentProjects:u-1", "[]");
    ls.setItem("nexus.lastPath.app:t-1:u-1", "/home");
    ls.setItem("nexus_token", "T");
    window.sessionStorage.setItem("nexus_workspace", "{}");
    clearStudioLocalState();
    expect(ls.getItem("nexus.studio.recentProjects:u-1")).toBe("[]");
    expect(ls.getItem("nexus.lastPath.app:t-1:u-1")).toBe("/home");
    expect(ls.getItem("nexus_token")).toBe("T");
    expect(window.sessionStorage.getItem("nexus_workspace")).toBe("{}");
  });

  it("never throws when storage is blocked", () => {
    const orig = Storage.prototype.removeItem;
    Storage.prototype.removeItem = () => {
      throw new Error("blocked");
    };
    try {
      expect(() => clearStudioLocalState()).not.toThrow();
    } finally {
      Storage.prototype.removeItem = orig;
    }
  });
});
