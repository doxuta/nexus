import { readFileSync } from "node:fs";
import { beforeEach, describe, expect, it, vi } from "vitest";

const startPreviewSession = vi.fn();
const endPreviewSession = vi.fn();
vi.mock("@/lib/api", () => ({
  startPreviewSession: (...a: unknown[]) => startPreviewSession(...a),
  endPreviewSession: (...a: unknown[]) => endPreviewSession(...a),
}));
vi.mock("@/lib/studioContext", () => ({ activeProjectSlug: () => "northwind" }));
const clearDraftPreview = vi.fn();
const setDraftPreview = vi.fn((_r: string) => true);
vi.mock("@/lib/previewMode", () => ({ clearDraftPreview: () => clearDraftPreview(), setDraftPreview: (r: string) => setDraftPreview(r) }));

import { getPreview } from "@/lib/previewSession";
import { openDraftPreview, previewInWorkspace, viewAsRole } from "./studioViewAs";

const session = { token: "t1", sessionId: "s1", role: { id: "r1", name: "crm_sales_rep" }, expiresAt: "2026-10-14T10:00:00Z", appId: "a1", draft: false };
const assign = vi.fn();
const args = { roleId: "r1", appId: "a1", appSlug: "crm" };

beforeEach(() => {
  vi.clearAllMocks();
  window.sessionStorage.clear();
  startPreviewSession.mockResolvedValue(session);
  endPreviewSession.mockResolvedValue("/studio");
  Object.defineProperty(window, "location", {
    configurable: true,
    value: { assign, pathname: "/studio/du-an/northwind/dung/chay-thu", search: "?app=x&next=//evil#y" },
  });
});

describe("previewInWorkspace", () => {
  it("is on: #400 (preview inside a customer workspace) has landed", () => {
    expect(previewInWorkspace).toBe(true);
  });
});

describe("viewAsRole", () => {
  it("starts the session, stores it with a rebuilt returnTo, then navigates same tab - in that order", async () => {
    const order: string[] = [];
    startPreviewSession.mockImplementation(async () => (order.push("start"), session));
    assign.mockImplementation(() => order.push(`assign:${getPreview() ? "stored" : "empty"}`));
    await viewAsRole(args);
    expect(startPreviewSession).toHaveBeenCalledWith("r1", "a1", undefined);
    expect(getPreview()?.returnTo).toBe("/studio/du-an/northwind/dung/chay-thu?app=crm");
    expect(order).toEqual(["start", "assign:stored"]);
    expect(assign).toHaveBeenCalledWith("/home?app=crm");
    expect(clearDraftPreview).toHaveBeenCalled();
  });

  it("never copies location.search/hash into returnTo", async () => {
    await viewAsRole(args);
    expect(getPreview()?.returnTo).not.toMatch(/evil|next/);
  });

  it("encodes the app slug in /home", async () => {
    await viewAsRole({ ...args, appSlug: "a b&c" });
    expect(assign).toHaveBeenCalledWith("/home?app=a%20b%26c");
  });

  it("session start error is rethrown and nothing navigates", async () => {
    startPreviewSession.mockRejectedValue(new Error("x"));
    await expect(viewAsRole(args)).rejects.toThrow("x");
    expect(assign).not.toHaveBeenCalled();
  });

  it("sessionStorage write failure: no navigation, session ended, error thrown", async () => {
    const spy = vi.spyOn(window.sessionStorage, "setItem").mockImplementation(() => {
      throw new Error("quota");
    });
    await expect(viewAsRole(args)).rejects.toThrow();
    spy.mockRestore();
    expect(assign).not.toHaveBeenCalled();
    expect(endPreviewSession).toHaveBeenCalledWith("exit");
  });

  it("read-back mismatch: no navigation, session ended", async () => {
    const spy = vi.spyOn(window.sessionStorage, "setItem").mockImplementation(() => {});
    await expect(viewAsRole(args)).rejects.toThrow();
    spy.mockRestore();
    expect(assign).not.toHaveBeenCalled();
    expect(endPreviewSession).toHaveBeenCalledWith("exit");
  });

  it("passes the draft choice to the session start", async () => {
    await viewAsRole({ ...args, draft: false });
    expect(startPreviewSession).toHaveBeenCalledWith("r1", "a1", false);
  });

  it("openDraftPreview: drops the role token, writes the draft flag, navigates same tab, starts no session", () => {
    window.sessionStorage.setItem("nexus_preview", "{}");
    openDraftPreview({ appSlug: "crm" });
    expect(setDraftPreview).toHaveBeenCalledWith("/studio/du-an/northwind/dung/chay-thu?app=crm");
    expect(window.sessionStorage.getItem("nexus_preview")).toBeNull();
    expect(assign).toHaveBeenCalledWith("/home?app=crm");
    expect(startPreviewSession).not.toHaveBeenCalled();
  });

  it("openDraftPreview: flag not stored => throws, no navigation", () => {
    setDraftPreview.mockReturnValueOnce(false);
    expect(() => openDraftPreview({ appSlug: "crm" })).toThrow();
    expect(assign).not.toHaveBeenCalled();
  });

  it("source never opens a tab or reads the home token", () => {
    const src = readFileSync("lib/studioViewAs.ts", "utf8");
    expect(src).not.toMatch(/window\.open|getHomeToken/);
  });
});
