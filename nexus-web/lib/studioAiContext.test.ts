// specs/076 ai5/ai6 (FR-129): screen -> assistant context. Keys only, validated, never a project.
import { describe, expect, it } from "vitest";
import { parseStudioPath } from "@/lib/studioRoutes";
import { vi } from "vitest";
import { assistantEligible, resolveAppForEntity, studioAiContext } from "./studioAiContext";

// specs/076 ai3 T056: who may see the docked assistant at all.
describe("assistantEligible", () => {
  const route = (p: string) => parseStudioPath(p)?.route ?? null;
  it("is true on a project page when the token belongs to the project", () => {
    expect(assistantEligible(route("/studio/du-an/acme/dung/du-lieu"), true)).toBe(true);
    expect(assistantEligible(route("/studio/du-an/acme/van-hanh/nguoi-dung"), true)).toBe(true);
  });
  it("is false when the token does not match, on operator-only (nha) pages and outside the frame", () => {
    expect(assistantEligible(route("/studio/du-an/acme/dung/du-lieu"), false)).toBe(false);
    expect(assistantEligible(route("/studio/du-an/acme/cai-dat"), true)).toBe(false);
    expect(assistantEligible(route("/studio/du-an/acme/bao-tri/sao-luu"), true)).toBe(false);
    expect(assistantEligible(null, true)).toBe(false);
  });
});

const ctx = (path: string) => {
  const p = parseStudioPath(path)!;
  return studioAiContext(p.route, p.params, p.query);
};

describe("studioAiContext", () => {
  it("takes panel from the route, entity from the path param and app from ?app", () => {
    expect(ctx("/studio/du-an/northwind/dung/du-lieu/lich_hen")).toEqual({ panel: "bang", entity: "lich_hen" });
    expect(ctx("/studio/du-an/northwind/dung/man-hinh?app=phong_kham&bang=lich_hen")).toEqual({
      panel: "man-hinh", app: "phong_kham", entity: "lich_hen",
    });
  });
  it("never carries a project and is empty on a page with no assistant code", () => {
    expect(Object.keys(ctx("/studio/du-an/northwind/dung/du-lieu/lich_hen"))).not.toContain("project");
    expect(ctx("/studio/du-an/northwind/cai-dat")).toEqual({});
  });
  it("drops hostile or malformed keys instead of sending them", () => {
    const route = parseStudioPath("/studio/du-an/northwind/dung/man-hinh")!.route;
    for (const bad of ["../x", "a?b=c", "//e", "javascript:", "\u0000", "a".repeat(65), "Hoa"]) {
      expect(studioAiContext(route, {}, { bang: bad, app: bad })).toEqual({ panel: "man-hinh" });
    }
  });
  it("adds recordId only when it matches the id shape", () => {
    const route = parseStudioPath("/studio/du-an/northwind/dung/du-lieu/lich_hen")!.route;
    const params = { bang: "lich_hen" };
    expect(studioAiContext(route, params, {}, "rec-1_A")).toMatchObject({ recordId: "rec-1_A" });
    expect(studioAiContext(route, params, {}, "a b")).not.toHaveProperty("recordId");
    expect(studioAiContext(route, params, {}, "x".repeat(65))).not.toHaveProperty("recordId");
  });
});

describe("resolveAppForEntity", () => {
  const mk = (menus: Record<string, string[]>, ids: Record<string, string> = { lich_hen: "e1" }) => ({
    apps: vi.fn(async () => Object.keys(menus).map((n) => ({ id: "id-" + n, name: n }))),
    entityId: vi.fn(async (n: string) => ids[n]),
    menu: vi.fn(async (id: string) => (menus[id.slice(3)] ?? []).map((refId) => ({ itemType: "entity", refId }))),
  });
  it("one app ⇒ that app, no menu reads", async () => {
    const l = mk({ phong_kham: [] });
    expect(await resolveAppForEntity("lich_hen", l)).toBe("phong_kham");
    expect(l.menu).not.toHaveBeenCalled();
  });
  it("several apps ⇒ the one whose menu lists the table, one menu read per app", async () => {
    const l = mk({ a: ["x"], b: ["e1"] });
    expect(await resolveAppForEntity("lich_hen", l)).toBe("b");
    expect(l.menu).toHaveBeenCalledTimes(2);
  });
  it("ambiguous, absent or failing lookups ⇒ undefined", async () => {
    expect(await resolveAppForEntity("lich_hen", mk({ a: ["e1"], b: ["e1"] }))).toBeUndefined();
    expect(await resolveAppForEntity("lich_hen", mk({ a: [], b: [] }))).toBeUndefined();
    const bad = mk({ a: [], b: [] });
    bad.apps.mockRejectedValue(new Error("x"));
    expect(await resolveAppForEntity("lich_hen", bad)).toBeUndefined();
  });
  it("studioAiContext uses the resolved app only when ?app is absent", () => {
    const route = parseStudioPath("/studio/du-an/northwind/dung/du-lieu/lich_hen")!.route;
    expect(studioAiContext(route, { bang: "lich_hen" }, {}, undefined, "b")).toMatchObject({ app: "b" });
    expect(studioAiContext(route, { bang: "lich_hen" }, { app: "a" }, undefined, "b")).toMatchObject({ app: "a" });
    expect(studioAiContext(route, {}, {}, undefined, "b")).not.toHaveProperty("app");
  });
});
