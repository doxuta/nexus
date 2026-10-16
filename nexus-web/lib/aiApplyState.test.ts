import { describe, it, expect, beforeEach, vi } from "vitest";
import { loadApplyState, saveApplyState, type ApplyState } from "./aiApplyState";

const FULL: Record<string, ApplyState> = {
  "t1:0": { status: "done", message: "Đã thêm vào menu Lịch hẹn", entityName: "Lịch hẹn", createdId: "d1", foot: "page.studio.ai.foot.live", changeIds: ["c1"], headVersionNo: 7, updatedAt: "2026-10-14T00:00:00Z" },
  "t1:1": { status: "warn", message: "Còn thiếu Lịch hẹn" },
  "t1:2": { status: "error", message: "lỗi" },
  "t1:3": { status: "applying" },
  "t1:4": { status: "sealed" },
};

beforeEach(() => window.localStorage.clear());

describe("aiApplyState", () => {
  it("stores only terminal outcomes under nexus.ai-apply-state.v1:<slug>:<sessionId>", () => {
    saveApplyState("acme", "s1", FULL);
    const raw = JSON.parse(window.localStorage.getItem("nexus.ai-apply-state.v1:acme:s1")!);
    expect(Object.keys(raw).sort()).toEqual(["t1:0", "t1:1", "t1:4"]);
    expect(raw["t1:0"]).toEqual({ status: "done", changeIds: ["c1"], headVersionNo: 7, updatedAt: "2026-10-14T00:00:00Z" });
  });

  it("never persists customer text: no table name, message, createdId or foot reaches localStorage", () => {
    saveApplyState("acme", "s1", FULL);
    const dump = JSON.stringify({ ...window.localStorage });
    for (const leak of ["Lịch hẹn", "message", "entityName", "createdId", "foot", "lỗi"]) expect(dump).not.toContain(leak);
  });

  it("round-trips the persisted slice", () => {
    saveApplyState("acme", "s1", FULL);
    expect(loadApplyState("acme", "s1")["t1:4"]).toEqual({ status: "sealed" });
    expect(loadApplyState("acme", "s1")["t1:0"].headVersionNo).toBe(7);
  });

  it("two projects with the same session id do not read each other's cards", () => {
    saveApplyState("acme", "s1", FULL);
    expect(loadApplyState("globex", "s1")).toEqual({});
  });

  it("a session with no key, a corrupt value, or a throwing storage reads as {}", () => {
    expect(loadApplyState("acme", "none")).toEqual({});
    window.localStorage.setItem("nexus.ai-apply-state.v1:acme:bad", "{not json");
    expect(loadApplyState("acme", "bad")).toEqual({});
    const spy = vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => { throw new Error("denied"); });
    expect(loadApplyState("acme", "s1")).toEqual({});
    spy.mockRestore();
  });

  it("save swallows a storage that throws", () => {
    const spy = vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new Error("quota"); });
    expect(() => saveApplyState("acme", "s1", FULL)).not.toThrow();
    spy.mockRestore();
  });

  it("falls back to the pre-slug key so cards applied before this release stay applied", () => {
    window.localStorage.setItem("nexus.ai-apply-state.v1:s9", JSON.stringify({ "t:0": { status: "done", message: "x" } }));
    expect(loadApplyState("acme", "s9")).toEqual({ "t:0": { status: "done" } });
  });
});
