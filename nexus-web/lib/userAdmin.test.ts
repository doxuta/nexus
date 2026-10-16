import { afterEach, describe, expect, it, vi } from "vitest";
import { describeUserTier, generatePassword, userLede, userRowActions } from "./userAdmin";

afterEach(() => vi.restoreAllMocks());

describe("userLede", () => {
  it.each([
    [[], { n: 0, m: 0, k: 0 }],
    [[{ status: "active" }], { n: 1, m: 1, k: 0 }],
    [[{ status: "active" }, { status: "inactive" }, { status: "active" }], { n: 3, m: 2, k: 1 }],
    [[{ status: "inactive" }], { n: 1, m: 0, k: 1 }],
  ])("%j", (users, want) => expect(userLede(users)).toEqual(want));
});

describe("generatePassword", () => {
  it("is 16 characters, meets the server policy, and differs between calls", () => {
    const a = generatePassword();
    const b = generatePassword();
    expect(a).toHaveLength(16);
    expect(a.trim()).toBe(a);
    expect(a).not.toBe(b);
  });
  it("draws from crypto.getRandomValues, never Math.random", () => {
    const rnd = vi.spyOn(Math, "random");
    const spy = vi.spyOn(crypto, "getRandomValues");
    generatePassword();
    expect(spy).toHaveBeenCalled();
    expect(rnd).not.toHaveBeenCalled();
  });
  it("skips bytes that would bias the pick (a byte at or above the limit is rejected)", () => {
    let calls = 0;
    vi.spyOn(crypto, "getRandomValues").mockImplementation(((arr: Uint8Array) => {
      arr.fill(calls++ === 0 ? 255 : 0);
      return arr;
    }) as typeof crypto.getRandomValues);
    expect(generatePassword()).toBe("A".repeat(16));
    expect(calls).toBe(2);
  });
});

describe("userRowActions", () => {
  it("locks an active person and unlocks anyone else", () => {
    expect(userRowActions({ status: "active" })).toEqual({ lock: "lock", next: "inactive" });
    expect(userRowActions({ status: "inactive" })).toEqual({ lock: "unlock", next: "active" });
  });
});

describe("describeUserTier", () => {
  it("is null when canManageUsers is absent, owner/admin when set, null for an ordinary employee", () => {
    expect(describeUserTier({ isAdmin: true })).toBeNull();
    expect(describeUserTier({ isAdmin: false, canManageUsers: true })).toBe("owner");
    expect(describeUserTier({ isAdmin: true, canManageUsers: false })).toBe("admin");
    expect(describeUserTier({ isAdmin: false, canManageUsers: false })).toBeNull();
  });
});
