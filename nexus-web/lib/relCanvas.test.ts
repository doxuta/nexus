import { describe, it, expect } from "vitest";
import {
  deriveEdges,
  dagreLayout,
  layoutStorageKey,
  suggestFieldName,
  isValidIdent,
  nodeHeight,
  NODE_W,
  foldVi,
  matchEntities,
  neighbourhood,
  edgeKindOf,
  impactDependents,
} from "./relCanvas";
import type { AdminRelationship, FieldMeta, RelationshipImpact } from "@/lib/api";

const ENTITIES = new Set(["crm_accounts", "crm_opportunities", "crm_contacts"]);

const relField = (name: string, target: string, cardinality?: string): FieldMeta => ({
  name,
  label: name,
  fieldType: "relation",
  options: { relatedEntity: target, ...(cardinality ? { cardinality } : {}) },
});

const row = (over: Partial<AdminRelationship>): AdminRelationship => ({
  id: "rel-1",
  name: "crm_opportunities_account",
  parentEntityName: "crm_accounts",
  childEntityName: "crm_opportunities",
  relationshipType: "1-N",
  inverseField: "crm_opportunities",
  childFkColumn: "account_id",
  ...over,
});

describe("deriveEdges — re-key theo sys_relationships.id (W2 §3.1)", () => {
  it("registry row → edge keyed by REAL registry id, parent→child, field attached", () => {
    const fields = { crm_opportunities: [relField("account", "crm_accounts")] };
    const edges = deriveEdges(ENTITIES, fields, [row({ id: "uuid-77" })]);
    expect(edges).toHaveLength(1);
    expect(edges[0].id).toBe("uuid-77"); // NOT synthetic "1n-…"
    expect(edges[0].source).toBe("crm_accounts"); // "1" side
    expect(edges[0].target).toBe("crm_opportunities"); // "N" side
    expect(edges[0].legacy).toBe(false);
    expect(edges[0].rel?.name).toBe("crm_opportunities_account");
    expect(edges[0].field?.name).toBe("account");
  });

  it("relation field COVERED by a registry row is not drawn twice", () => {
    const fields = { crm_opportunities: [relField("account", "crm_accounts")] };
    const edges = deriveEdges(ENTITIES, fields, [row({})]);
    expect(edges).toHaveLength(1);
  });

  it("relation field WITHOUT registry row → honest legacy edge (no lifecycle id)", () => {
    const fields = { crm_opportunities: [relField("account", "crm_accounts")] };
    const edges = deriveEdges(ENTITIES, fields, []);
    expect(edges).toHaveLength(1);
    expect(edges[0].id).toBe("legacy:crm_opportunities.account");
    expect(edges[0].legacy).toBe(true);
    expect(edges[0].relType).toBe("1-N");
    expect(edges[0].source).toBe("crm_accounts");
  });

  it("legacy 1-1 cardinality is read from field options", () => {
    const fields = { crm_opportunities: [relField("account", "crm_accounts", "1-1")] };
    const edges = deriveEdges(ENTITIES, fields, []);
    expect(edges[0].relType).toBe("1-1");
  });

  it("N-N registry rows draw without field matching; rows referencing missing entities skip", () => {
    const edges = deriveEdges(ENTITIES, {}, [
      row({ id: "nn-1", relationshipType: "N-N", childFkColumn: undefined, inverseField: undefined, joinTableName: "rel_x" }),
      row({ id: "gone", parentEntityName: "not_here" }),
    ]);
    expect(edges).toHaveLength(1);
    expect(edges[0].id).toBe("nn-1");
    expect(edges[0].relType).toBe("N-N");
  });

  it("parallel edges between the same pair get increasing parallelIndex", () => {
    const fields = {
      crm_contacts: [
        relField("account", "crm_accounts"),
        relField("audit_company_ref", "crm_accounts"),
      ],
    };
    const edges = deriveEdges(ENTITIES, fields, [
      row({ id: "r1", name: "crm_contacts_account", childEntityName: "crm_contacts", childFkColumn: "account_id" }),
      row({ id: "r2", name: "crm_contacts_audit_company_ref", childEntityName: "crm_contacts", childFkColumn: "audit_company_ref_id" }),
    ]);
    expect(edges.map((e) => e.parallelIndex)).toEqual([0, 1]);
  });
});

describe("dagreLayout", () => {
  it("returns top-left anchored, non-overlapping positions for every node", () => {
    const nodes = [
      { id: "a", width: NODE_W, height: nodeHeight(3) },
      { id: "b", width: NODE_W, height: nodeHeight(5) },
      { id: "c", width: NODE_W, height: nodeHeight(0) },
    ];
    const pos = dagreLayout(nodes, [
      { source: "a", target: "b" },
      { source: "a", target: "c" },
    ]);
    expect(Object.keys(pos).sort()).toEqual(["a", "b", "c"]);
    // LR ranking: parent strictly left of children.
    expect(pos.a.x).toBeLessThan(pos.b.x);
    expect(pos.a.x).toBeLessThan(pos.c.x);
    // children on the same rank must not overlap vertically.
    const [top, bottom] = [pos.b, pos.c].sort((p, q) => p.y - q.y);
    expect(top.y + nodeHeight(5)).toBeLessThanOrEqual(bottom.y + nodeHeight(5) + nodeHeight(0));
  });

  it("self-loop edges do not crash the layout", () => {
    const pos = dagreLayout([{ id: "a", width: 200, height: 100 }], [{ source: "a", target: "a" }]);
    expect(pos.a).toBeDefined();
  });
});

describe("layoutStorageKey", () => {
  it("stable across entity ORDER, distinct across entity SET and tenant", () => {
    const k1 = layoutStorageKey("t1", ["b", "a"]);
    const k2 = layoutStorageKey("t1", ["a", "b"]);
    const k3 = layoutStorageKey("t1", ["a", "b", "c"]);
    const k4 = layoutStorageKey("t2", ["a", "b"]);
    expect(k1).toBe(k2);
    expect(k1).not.toBe(k3);
    expect(k1).not.toBe(k4);
  });
});

describe("suggestFieldName", () => {
  it("strips shared prefix + plural s (crm_accounts → account)", () => {
    expect(suggestFieldName("crm_accounts", "crm_opportunities", [])).toBe("account");
  });
  it("no shared prefix → parent name minus plural s", () => {
    expect(suggestFieldName("clinic_patients", "crm_contacts", [])).toBe("clinic_patient");
  });
  it("dedupes against existing child fields", () => {
    expect(suggestFieldName("crm_accounts", "crm_contacts", [{ name: "account" }])).toBe("account_2");
  });
});

describe("isValidIdent", () => {
  it("accepts snake_case, rejects the rest", () => {
    expect(isValidIdent("account")).toBe(true);
    expect(isValidIdent("account_ref_2")).toBe(true);
    expect(isValidIdent("Account")).toBe(false);
    expect(isValidIdent("1abc")).toBe(false);
    expect(isValidIdent("a-b")).toBe(false);
    expect(isValidIdent("")).toBe(false);
  });
});

// ── relation-map-read-manage (specs/075 T140) ──
describe("relation map helpers (075 L4a)", () => {
  it("foldVi strips diacritics and đ", () => {
    expect(foldVi("Khách Hàng Đỏ")).toBe("khach hang do");
  });
  it("matchEntities matches label or name without diacritics", () => {
    const ents = [
      { name: "crm_accounts", label: "Khách hàng" },
      { name: "crm_events", label: "Lịch hẹn" },
    ];
    expect(matchEntities(ents, "khach").map((e) => e.name)).toEqual(["crm_accounts"]);
    expect(matchEntities(ents, "EVENTS").map((e) => e.name)).toEqual(["crm_events"]);
    expect(matchEntities(ents, "  ")).toEqual(ents);
    expect(matchEntities(ents, "zzz")).toEqual([]);
  });
  it("neighbourhood keeps the table and tables joined to it directly", () => {
    const edges = [
      { source: "a", target: "b" },
      { source: "c", target: "a" },
      { source: "d", target: "e" },
    ];
    expect([...neighbourhood("a", edges)].sort()).toEqual(["a", "b", "c"]);
  });
  it("edgeKindOf maps cardinality to marker + dash + text label, never colour", () => {
    expect(edgeKindOf("1-N")).toMatchObject({ dashed: false, endArrow: true, startArrow: false, labelKey: "studio.build.relMap.kind.oneMany" });
    expect(edgeKindOf("N-N")).toMatchObject({ dashed: true, endArrow: true, startArrow: true, labelKey: "studio.build.relMap.kind.manyMany" });
    expect(edgeKindOf("1-1")).toMatchObject({ dashed: false, endArrow: false, startArrow: false, labelKey: "studio.build.relMap.kind.oneOne" });
    for (const k of ["1-N", "N-N", "1-1"] as const) expect(edgeKindOf(k)).not.toHaveProperty("color");
  });
  it("impactDependents reads RelationshipImpact directly (no ImpactRow)", () => {
    const imp = {
      relationship: { id: "r", name: "n", parentEntityName: "a", childEntityName: "b", relationshipType: "1-N" },
      affectedRecords: 4,
      dependentRollups: [{ entityName: "a", fieldName: "total" }],
      dependentLookups: [{ entityName: "b", fieldName: "pick" }],
      deletable: false,
    } as RelationshipImpact;
    expect(impactDependents(imp)).toEqual({
      records: 4,
      blocking: [{ entityName: "a", fieldName: "total" }],
      inert: [{ entityName: "b", fieldName: "pick" }],
    });
  });
});
