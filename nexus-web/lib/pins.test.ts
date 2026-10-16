import { describe, it, expect, beforeEach } from "vitest";
import { loadPins, savePins, togglePin, isPinned } from "./pins";

// happy-dom provides window/localStorage.
beforeEach(() => {
  window.localStorage.clear();
});

describe("togglePin — pure toggle, append-at-end order", () => {
  it("pins a new entity at the END (pin order = display order)", () => {
    expect(togglePin([], "crm_accounts")).toEqual(["crm_accounts"]);
    expect(togglePin(["crm_accounts"], "tickets")).toEqual(["crm_accounts", "tickets"]);
  });

  it("unpins an already-pinned entity, keeping the others' order", () => {
    expect(togglePin(["a", "b", "c"], "b")).toEqual(["a", "c"]);
  });

  it("isPinned reflects membership", () => {
    expect(isPinned(["a"], "a")).toBe(true);
    expect(isPinned(["a"], "b")).toBe(false);
  });
});

describe("loadPins/savePins — per-user localStorage namespacing", () => {
  it("round-trips through localStorage for a given user key", () => {
    savePins(["crm_accounts", "tickets"], "user-1");
    expect(loadPins("user-1")).toEqual(["crm_accounts", "tickets"]);
  });

  it("two users on the same browser do NOT share pins", () => {
    savePins(["crm_accounts"], "user-1");
    savePins(["tickets"], "user-2");
    expect(loadPins("user-1")).toEqual(["crm_accounts"]);
    expect(loadPins("user-2")).toEqual(["tickets"]);
  });

  it("returns [] for corrupt or missing storage", () => {
    expect(loadPins("nobody")).toEqual([]);
    window.localStorage.setItem("nexus.pins.v1:bad", "{not json");
    expect(loadPins("bad")).toEqual([]);
    window.localStorage.setItem("nexus.pins.v1:mixed", JSON.stringify(["ok", 7, null]));
    expect(loadPins("mixed")).toEqual(["ok"]); // non-strings filtered
  });
});
