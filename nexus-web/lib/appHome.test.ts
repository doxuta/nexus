import { describe, it, expect } from "vitest";
import { selectAppHome, scopeToAppEntities, pickAppQueues, STAT_ENTITY_LIMIT } from "./appHome";
import type { AppNavEntry } from "./api";

function entity(name: string, label = name): AppNavEntry {
  return { type: "entity", refId: `uuid-${name}`, label, href: `/${name}` };
}
function dashboard(id: string, label = id): AppNavEntry {
  return { type: "dashboard", refId: id, label, href: `/dashboards/${id}` };
}

describe("selectAppHome — app nav → app-scoped Home selection", () => {
  it("null nav (still loading) → empty selection, NOT isEmpty", () => {
    const sel = selectAppHome(null);
    expect(sel.statEntities).toEqual([]);
    expect(sel.recentEntity).toBeNull();
    expect(sel.dashboards).toEqual([]);
    expect(sel.isEmpty).toBe(false);
  });

  it("resolved empty nav → isEmpty (ACL hid everything / unconfigured app)", () => {
    expect(selectAppHome([]).isEmpty).toBe(true);
  });

  it("entity name derives from href, not refId (refId is the sys_entities UUID)", () => {
    const sel = selectAppHome([entity("orders", "Đơn hàng")]);
    expect(sel.statEntities).toEqual([{ name: "orders", label: "Đơn hàng" }]);
  });

  it("statEntities = first 4 entities in nav order; the 5th is dropped", () => {
    const nav = ["a", "b", "c", "d", "e"].map((n) => entity(n));
    const sel = selectAppHome(nav);
    expect(sel.statEntities.map((s) => s.name)).toEqual(["a", "b", "c", "d"]);
    expect(sel.statEntities).toHaveLength(STAT_ENTITY_LIMIT);
  });

  it("recentEntity = FIRST entity even when a dashboard sits before it in the nav", () => {
    const sel = selectAppHome([dashboard("d1"), entity("tickets", "Phiếu hỗ trợ"), entity("agents")]);
    expect(sel.recentEntity).toEqual({ name: "tickets", label: "Phiếu hỗ trợ" });
  });

  it("dashboards keep nav order and entities are excluded", () => {
    const sel = selectAppHome([dashboard("d2", "Doanh thu"), entity("orders"), dashboard("d1", "Vận hành")]);
    expect(sel.dashboards.map((d) => d.refId)).toEqual(["d2", "d1"]);
    expect(sel.dashboards.every((d) => d.type === "dashboard")).toBe(true);
  });

  it("dashboard-only nav → no stat entities, no recentEntity, not empty", () => {
    const sel = selectAppHome([dashboard("d1")]);
    expect(sel.statEntities).toEqual([]);
    expect(sel.recentEntity).toBeNull();
    expect(sel.dashboards).toHaveLength(1);
    expect(sel.isEmpty).toBe(false);
  });
});

describe("scopeToAppEntities — work queues / approvals belong to the app whose tables they read", () => {
  const rows = [
    { id: "q1", entityName: "leads" },
    { id: "q2", entityName: "appointments" },
    { id: "q3", entityName: "tickets" },
  ];
  const by = (r: { entityName: string }) => r.entityName;

  it("keeps only rows whose entity is in the app nav", () => {
    const nav = [entity("appointments"), entity("patients"), dashboard("d1")];
    expect(scopeToAppEntities(rows, nav, by).map((r) => r.id)).toEqual(["q2"]);
  });

  it("nav still loading (null) → nothing yet, never the tenant-wide list", () => {
    expect(scopeToAppEntities(rows, null, by)).toEqual([]);
  });

  it("app with no entity in its nav → nothing (not the whole tenant)", () => {
    expect(scopeToAppEntities(rows, [dashboard("d1")], by)).toEqual([]);
  });
});

describe("pickAppQueues — explicit queues win over the guess from the menu", () => {
  const rows = [
    { id: "q1", entityName: "leads" },
    { id: "q2", entityName: "appointments" },
    { id: "q3", entityName: "treatments" },
  ];
  const by = (r: { entityName: string }) => r.entityName;
  it("explicit ids → exactly those, in the builder's order, even for tables outside the menu", () => {
    const nav = [entity("appointments")];
    expect(pickAppQueues(rows, nav, ["q3", "q1"], by).map((r) => r.id)).toEqual(["q3", "q1"]);
  });
  it("explicit ids pointing at deleted queues are ignored", () => {
    expect(pickAppQueues(rows, [entity("leads")], ["q2", "gone"], by).map((r) => r.id)).toEqual(["q2"]);
  });
  it("no explicit ids → derived from the menu tables", () => {
    expect(pickAppQueues(rows, [entity("appointments")], [], by).map((r) => r.id)).toEqual(["q2"]);
    expect(pickAppQueues(rows, [entity("appointments")], undefined, by).map((r) => r.id)).toEqual(["q2"]);
  });
  it("nav loading and nothing explicit → nothing", () => {
    expect(pickAppQueues(rows, null, [], by)).toEqual([]);
  });
  it("explicit ids do not need the nav", () => {
    expect(pickAppQueues(rows, null, ["q1"], by).map((r) => r.id)).toEqual(["q1"]);
  });
});
