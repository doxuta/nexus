import { describe, expect, it } from "vitest";
import type { RoleGrant, RoleActionVerb, RoleAccessLevel } from "@/lib/api";
import { applyLevel, levelOf } from "./accessLevels";

const g = (entityName: string, action: RoleActionVerb, accessLevel: RoleAccessLevel = "all"): RoleGrant => ({ entityName, action, accessLevel });
const T = "invoice";

describe("levelOf: 8 view/create/edit combinations on the exact table", () => {
  const cases: [string, boolean, boolean, boolean, string][] = [
    ["nothing", false, false, false, "none"],
    ["view only", true, false, false, "view"],
    ["view+create+edit", true, true, true, "edit"],
    ["create only", false, true, false, "custom"],
    ["edit only", false, false, true, "custom"],
    ["view+create", true, true, false, "custom"],
    ["view+edit", true, false, true, "custom"],
    ["create+edit without view", false, true, true, "custom"],
  ];
  it.each(cases)("%s", (_n, v, c, e, tier) => {
    const grants = [v && g(T, "view"), c && g(T, "create"), e && g(T, "edit")].filter(Boolean) as RoleGrant[];
    expect(levelOf(grants, T)).toEqual({ tier, inherited: false });
  });
  it("delete does not move the tier", () => {
    expect(levelOf([g(T, "view"), g(T, "delete")], T).tier).toBe("view");
  });
});

describe("levelOf: delete without view is never 'Không thấy'", () => {
  it("exact or inherited delete with no view/create/edit reads custom", () => {
    expect(levelOf([g(T, "delete")], T).tier).toBe("custom");
    expect(levelOf([g("*", "delete")], T)).toEqual({ tier: "custom", inherited: true });
    expect(levelOf([g("*", "delete"), g(T, "delete", "none")], T).tier).toBe("none");
  });
});

describe("levelOf: effective action = exact grant > * > nothing (ACLService.Authorize)", () => {
  it("an explicit none on the exact table beats * all: Không thấy, NOT inherited", () => {
    const grants = [g("*", "view"), g("*", "create"), g("*", "edit"), g(T, "view", "none"), g(T, "create", "none"), g(T, "edit", "none")];
    expect(levelOf(grants, T)).toEqual({ tier: "none", inherited: false });
  });
  it("a table with no grant at all follows * and is inherited", () => {
    expect(levelOf([g("*", "view")], T)).toEqual({ tier: "view", inherited: true });
    expect(levelOf([g("*", "view"), g("*", "create"), g("*", "edit")], T)).toEqual({ tier: "edit", inherited: true });
  });
  it("inherited when at least one of view/create/edit comes from *", () => {
    const grants = [g("*", "edit"), g(T, "view"), g(T, "create")];
    expect(levelOf(grants, T)).toEqual({ tier: "edit", inherited: true });
  });
  it("a * grant of level none supplies nothing", () => {
    expect(levelOf([g("*", "view", "none")], T)).toEqual({ tier: "none", inherited: false });
  });
  it("no role grant and no * is Không thấy", () => {
    expect(levelOf([], T)).toEqual({ tier: "none", inherited: false });
  });
  it("other tables do not leak into this one", () => {
    expect(levelOf([g("customer", "view"), g("customer", "edit")], T)).toEqual({ tier: "none", inherited: false });
  });
});

describe("applyLevel", () => {
  it("raising from nothing writes view/create/edit with scope 'all'; create/edit take the view scope", () => {
    expect(applyLevel([], T, "edit")).toEqual([g(T, "view", "all"), g(T, "create", "all"), g(T, "edit", "all")]);
    expect(applyLevel([], T, "view")).toEqual([g(T, "view", "all")]);
  });
  it("keeps an existing view scope and hands it to create/edit", () => {
    const out = applyLevel([g(T, "view", "team")], T, "edit");
    expect(out).toEqual([g(T, "view", "team"), g(T, "create", "team"), g(T, "edit", "team")]);
  });
  it("lowering drops create/edit grants of this table only; delete and scope stay", () => {
    const before = [g(T, "view", "owner"), g(T, "create", "owner"), g(T, "edit", "owner"), g(T, "delete", "owner"), g("other", "edit")];
    expect(applyLevel(before, T, "view")).toEqual([g(T, "view", "owner"), g(T, "delete", "owner"), g("other", "edit")]);
  });
  it("Không thấy drops view/create/edit AND this table's delete, writes no none", () => {
    const out = applyLevel([g(T, "view"), g(T, "create"), g(T, "edit"), g(T, "delete"), g("other", "delete")], T, "none");
    expect(out).toEqual([g("other", "delete")]);
  });
  it("a change on another table does not drop this table's explicit none (the PUT array = the input array except the changed cell)", () => {
    const before = [g("*", "view"), g("blocked", "view", "none"), g("blocked", "create", "none"), g("blocked", "edit", "none")];
    const out = applyLevel(before, T, "edit");
    for (const keep of before) expect(out).toContainEqual(keep);
    expect(out).toHaveLength(before.length + 3);
    expect(levelOf(out, "blocked")).toEqual({ tier: "none", inherited: false });
  });
  it("Không thấy on a table already blocked by explicit none keeps those none grants (dropping them would hand the table back to *)", () => {
    const before = [g("*", "view"), g(T, "view", "none")];
    expect(applyLevel(before, T, "none")).toEqual(before);
  });
  it("replaces an explicit none by the new level instead of duplicating the grant", () => {
    const out = applyLevel([g(T, "view", "none")], T, "view");
    expect(out).toEqual([g(T, "view", "all")]);
  });
  it("preserves field:<name> scopes", () => {
    const out = applyLevel([g(T, "view", "field:owner_user")], T, "edit");
    expect(out).toEqual([g(T, "view", "field:owner_user"), g(T, "create", "field:owner_user"), g(T, "edit", "field:owner_user")]);
  });
  it("does not mutate the input", () => {
    const before = [g(T, "view")];
    const copy = structuredClone(before);
    applyLevel(before, T, "edit");
    expect(before).toEqual(copy);
  });
  it("custom is not a target: the grants come back unchanged", () => {
    const before = [g(T, "create")];
    expect(applyLevel(before, T, "custom")).toEqual(before);
  });
  it("the result reads back as the tier asked for", () => {
    for (const tier of ["none", "view", "edit"] as const) {
      expect(levelOf(applyLevel([g(T, "view"), g(T, "edit")], T, tier), T)).toEqual({ tier, inherited: false });
    }
  });
});
