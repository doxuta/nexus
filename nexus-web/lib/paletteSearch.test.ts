import { describe, it, expect } from "vitest";
import { pickSearchEntities, shapeRecordResults, shapeServerSearch, SEARCH_ENTITY_CAP } from "./paletteSearch";
import type { ApiRecord, EntityMeta } from "@/lib/api";

const ent = (name: string, label = name.toUpperCase()): EntityMeta => ({ name, label, tableName: `obj_${name}` });
const rec = (id: string, data: Record<string, unknown> = {}): ApiRecord => ({ id, data, version: 1 });

describe("pickSearchEntities — current first, then pins, then rest, cap 5", () => {
  const entities = [ent("a"), ent("b"), ent("c"), ent("d"), ent("e"), ent("f"), ent("g")];

  it("orders current → pinned → metadata order, deduped", () => {
    const out = pickSearchEntities(entities, { pins: ["c", "a"], current: "e" });
    expect(out.map((x) => x.name)).toEqual(["e", "c", "a", "b", "d"]);
  });

  it(`caps at ${SEARCH_ENTITY_CAP}`, () => {
    expect(pickSearchEntities(entities)).toHaveLength(SEARCH_ENTITY_CAP);
  });

  it("ignores a current/pin that is not in the accessible list (no permission)", () => {
    const out = pickSearchEntities([ent("a"), ent("b")], { pins: ["ghost"], current: "phantom" });
    expect(out.map((x) => x.name)).toEqual(["a", "b"]);
  });
});

describe("shapeRecordResults — grouped rows with VIP labels + deep links", () => {
  it("drops empty entities, keeps hit order, links to /[entity]/[id]", () => {
    const groups = shapeRecordResults([
      { entity: ent("crm_accounts", "Khách hàng"), records: [rec("r1", { name: "ACME Corp" })] },
      { entity: ent("tickets", "Phiếu hỗ trợ"), records: [] },
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0]).toMatchObject({ entity: "crm_accounts", entityLabel: "Khách hàng" });
    expect(groups[0].hits[0]).toEqual({ id: "r1", label: "ACME Corp", href: "/crm_accounts/r1" });
  });

  it("labels fall back to the VIP heuristic (#shortid when no name-ish field)", () => {
    const groups = shapeRecordResults([
      { entity: ent("things"), records: [rec("0123456789abcdef", {})] },
    ]);
    expect(groups[0].hits[0].label).toBe("#01234567");
  });
});

describe("shapeServerSearch — SEARCH-UNIFIED server groups → palette rows", () => {
  it("maps entity/label/hits with VIP labels + deep links, drops empty groups", () => {
    const out = shapeServerSearch([
      { entity: "crm_accounts", label: "Khách hàng", hits: [{ id: "r1", data: { name: "ACME Corp" } }] },
      { entity: "tickets", label: "Phiếu", hits: [] },
    ]);
    expect(out).toHaveLength(1);
    expect(out[0]).toMatchObject({ entity: "crm_accounts", entityLabel: "Khách hàng" });
    expect(out[0].hits[0]).toEqual({ id: "r1", label: "ACME Corp", href: "/crm_accounts/r1" });
  });
});
