import { beforeEach, describe, expect, it, vi } from "vitest";
import { assistantReadsBrief, clearPendingBrief, PENDING_BRIEF_KEY, savePendingBrief, takePendingBrief } from "./studioAssistantBrief";

beforeEach(() => sessionStorage.clear());

describe("studioAssistantBrief", () => {
  it("pins the key T435 clears and keeps the flag off until ai4", () => {
    expect(PENDING_BRIEF_KEY).toBe("nexus.studio.pendingBrief");
    expect(assistantReadsBrief).toBe(false);
  });
  it("returns the brief only for its own slug and removes it on read", () => {
    savePendingBrief({ slug: "x", brief: "phòng khám" });
    expect(takePendingBrief("y")).toBeNull();
    expect(sessionStorage.getItem(PENDING_BRIEF_KEY)).toBeNull(); // the mismatched read removed it too
    savePendingBrief({ slug: "x", brief: "phòng khám" });
    expect(takePendingBrief("x")).toBe("phòng khám");
    expect(takePendingBrief("x")).toBeNull();
  });
  it("clearPendingBrief removes it; garbage reads as none", () => {
    savePendingBrief({ slug: "x", brief: "b" });
    clearPendingBrief();
    expect(takePendingBrief("x")).toBeNull();
    sessionStorage.setItem(PENDING_BRIEF_KEY, "{not json");
    expect(takePendingBrief("x")).toBeNull();
  });
  it("never throws when storage is unavailable", () => {
    const g = vi.spyOn(window, "sessionStorage", "get").mockImplementation(() => { throw new Error("denied"); });
    expect(() => savePendingBrief({ slug: "x", brief: "b" })).not.toThrow();
    expect(takePendingBrief("x")).toBeNull();
    expect(() => clearPendingBrief()).not.toThrow();
    g.mockRestore();
  });
});

describe("pendingBrief key vs clearStudioLocalState (T435/T526)", () => {
  it("clearStudioLocalState removes the brief", async () => {
    const { clearStudioLocalState } = await import("./studioLocalState");
    savePendingBrief({ slug: "x", brief: "b" });
    clearStudioLocalState();
    expect(sessionStorage.getItem(PENDING_BRIEF_KEY)).toBeNull();
  });
});
