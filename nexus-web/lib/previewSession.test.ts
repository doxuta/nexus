// lib/previewSession.ts — specs/064 T041: the preview state of ONE tab. It lives in sessionStorage
// only (never localStorage, which every tab of the browser shares — a preview token there would turn
// the builder's other tabs into the previewed role, or leak it past the tab).
import { describe, it, expect, beforeEach } from "vitest";
import {
  getPreview, getPreviewToken, setPreview, setPreviewMe, clearPreview,
  beginPreviewExit, endPreviewExit, isPreviewExiting,
  setPreviewNotice, takePreviewNotice, PREVIEW_STORAGE_KEY,
  type PreviewState,
} from "./previewSession";

const sample: PreviewState = {
  token: "preview-jwt",
  sessionId: "s-1",
  role: { id: "r-1", name: "crm_sales_rep", label: "Nhân viên kinh doanh" },
  expiresAt: "2030-01-01T00:00:00Z",
  returnTo: "/studio/apps/crm",
};

describe("previewSession", () => {
  beforeEach(() => {
    window.localStorage.clear();
    window.sessionStorage.clear();
    endPreviewExit();
  });

  it("is empty by default", () => {
    expect(getPreview()).toBeNull();
    expect(getPreviewToken()).toBeNull();
  });

  it("set/get/clear touch only sessionStorage['nexus_preview'] — never localStorage, even with nexus_remember=1", () => {
    window.localStorage.setItem("nexus_remember", "1");
    const before = { ...window.localStorage };
    setPreview(sample);
    expect(window.sessionStorage.getItem("nexus_preview")).toBe(JSON.stringify(sample));
    expect(PREVIEW_STORAGE_KEY).toBe("nexus_preview");
    expect(getPreview()).toEqual(sample);
    expect(getPreviewToken()).toBe("preview-jwt");
    expect({ ...window.localStorage }).toEqual(before); // localStorage untouched
    clearPreview();
    expect(getPreview()).toBeNull();
    expect(window.sessionStorage.getItem("nexus_preview")).toBeNull();
    expect({ ...window.localStorage }).toEqual(before);
  });

  it("a corrupt or token-less record reads as no preview", () => {
    window.sessionStorage.setItem("nexus_preview", "{not json");
    expect(getPreview()).toBeNull();
    window.sessionStorage.setItem("nexus_preview", JSON.stringify({ role: sample.role }));
    expect(getPreview()).toBeNull();
    expect(getPreviewToken()).toBeNull();
  });

  it("setPreviewMe merges the synthetic profile into the live preview and is a no-op without one", () => {
    setPreviewMe({ id: "p", fullName: "Xem như: X", email: "", isAdmin: false });
    expect(getPreview()).toBeNull();
    setPreview(sample);
    setPreviewMe({ id: "p", fullName: "Xem như: X", email: "", isAdmin: false });
    expect(getPreview()?.me?.id).toBe("p");
    expect(getPreview()?.token).toBe("preview-jwt");
  });

  it("the exit latch is per page load (memory), not storage", () => {
    expect(isPreviewExiting()).toBe(false);
    beginPreviewExit();
    expect(isPreviewExiting()).toBe(true);
    expect(window.sessionStorage.length).toBe(0);
    expect(window.localStorage.length).toBe(0);
    endPreviewExit();
    expect(isPreviewExiting()).toBe(false);
  });

  it("the 'preview ended' notice is read once", () => {
    expect(takePreviewNotice()).toBeNull();
    setPreviewNotice("ended");
    expect(takePreviewNotice()).toBe("ended");
    expect(takePreviewNotice()).toBeNull();
  });
});
