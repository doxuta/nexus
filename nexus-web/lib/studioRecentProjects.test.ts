// specs/073 T422 / FR-083: "Gần đây" of the project switcher in a build session. Only {slug, name}, newest first,
// at most 8, per home user, and storage that throws or holds junk never breaks the switcher.
import { beforeEach, describe, expect, it } from "vitest";
import { MAX_RECENT_PROJECTS, readRecentProjects, recentProjectsKey, rememberProject } from "./studioRecentProjects";

beforeEach(() => window.localStorage.clear());

describe("studioRecentProjects", () => {
  it("is keyed by the home user and stores nothing but slug and name", () => {
    rememberProject("u-1", { slug: "nhakhoa", name: "Nha khoa An Tâm", token: "SECRET", tenantId: "t-9" } as never);
    expect(recentProjectsKey("u-1")).toBe("nexus.studio.recentProjects:u-1");
    const raw = window.localStorage.getItem("nexus.studio.recentProjects:u-1") ?? "";
    expect(JSON.parse(raw)).toEqual([{ slug: "nhakhoa", name: "Nha khoa An Tâm" }]);
    expect(raw).not.toMatch(/SECRET|t-9/);
    expect(readRecentProjects("u-2")).toEqual([]); // another operator on the same machine sees none
  });

  it("puts the newest first, moves a repeat to the front, and keeps at most 8", () => {
    for (let i = 1; i <= 10; i++) rememberProject("u-1", { slug: `k${i}`, name: `Khách ${i}` });
    rememberProject("u-1", { slug: "k5", name: "Khách 5 (đổi tên)" });
    const list = readRecentProjects("u-1");
    expect(MAX_RECENT_PROJECTS).toBe(8);
    expect(list).toHaveLength(8);
    expect(list[0]).toEqual({ slug: "k5", name: "Khách 5 (đổi tên)" });
    expect(list.filter((p) => p.slug === "k5")).toHaveLength(1);
    expect(list.map((p) => p.slug)).not.toContain("k1");
  });

  it("drops junk on read: bad codes, non-strings, extra fields, a corrupt value", () => {
    window.localStorage.setItem(
      "nexus.studio.recentProjects:u-1",
      JSON.stringify([{ slug: "ok", name: "Tốt", extra: 1 }, { slug: "../evil", name: "x" }, { slug: "UP", name: "x" }, { slug: "n", name: 3 }, "str", null]),
    );
    expect(readRecentProjects("u-1")).toEqual([{ slug: "ok", name: "Tốt" }]);
    window.localStorage.setItem("nexus.studio.recentProjects:u-1", "{not json");
    expect(readRecentProjects("u-1")).toEqual([]);
  });

  it("refuses to remember an invalid code or an empty name, and survives storage that throws", () => {
    rememberProject("u-1", { slug: "Bad Code", name: "x" });
    rememberProject("u-1", { slug: "good", name: "   " });
    expect(readRecentProjects("u-1")).toEqual([]);
    const orig = Storage.prototype.getItem;
    Storage.prototype.getItem = () => {
      throw new Error("blocked");
    };
    try {
      expect(readRecentProjects("u-1")).toEqual([]);
      expect(() => rememberProject("u-1", { slug: "good", name: "Tốt" })).not.toThrow();
    } finally {
      Storage.prototype.getItem = orig;
    }
  });

  it("an empty home user id is never a key (nothing remembered, nothing read)", () => {
    rememberProject("", { slug: "good", name: "Tốt" });
    expect(window.localStorage.length).toBe(0);
    expect(readRecentProjects("")).toEqual([]);
  });
});
