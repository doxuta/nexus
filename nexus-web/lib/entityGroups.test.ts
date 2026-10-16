import { describe, it, expect } from "vitest";
import { groupEntities, GROUP_THRESHOLD } from "./entityGroups";
import type { EntityMeta } from "@/lib/api";

const ent = (name: string): EntityMeta => ({ name, label: name.toUpperCase(), tableName: `obj_${name}` });

describe("groupEntities — pinned float to top", () => {
  it("pinned entities come out in PIN order, removed from the groups", () => {
    const entities = [ent("a"), ent("b"), ent("c")];
    const g = groupEntities(entities, ["c", "a"]);
    expect(g.pinned.map((e) => e.name)).toEqual(["c", "a"]); // pin order, not metadata order
    expect(g.groups.flatMap((x) => x.items.map((e) => e.name))).toEqual(["b"]);
  });

  it("stale pins (entity deleted / no permission) are silently dropped", () => {
    const g = groupEntities([ent("a")], ["ghost", "a"]);
    expect(g.pinned.map((e) => e.name)).toEqual(["a"]);
  });
});

describe(`groupEntities — flat when ≤${GROUP_THRESHOLD} unpinned`, () => {
  it("keeps a single flat group under the threshold", () => {
    const entities = Array.from({ length: GROUP_THRESHOLD }, (_, i) => ent(`crm_e${i}`));
    const g = groupEntities(entities, []);
    expect(g.grouped).toBe(false);
    expect(g.groups).toHaveLength(1);
    expect(g.groups[0].key).toBe("__flat");
    expect(g.groups[0].items).toHaveLength(GROUP_THRESHOLD);
  });

  it("pinning below the threshold keeps the rest flat", () => {
    const entities = Array.from({ length: GROUP_THRESHOLD + 1 }, (_, i) => ent(`crm_e${i}`));
    const g = groupEntities(entities, ["crm_e0"]); // 8 left unpinned → flat
    expect(g.grouped).toBe(false);
  });
});

describe(`groupEntities — module groups when >${GROUP_THRESHOLD}`, () => {
  const many = [
    ent("crm_accounts"), ent("crm_contacts"), ent("crm_opportunities"),
    ent("clinic_patients"), ent("clinic_visits"),
    ent("tickets"), ent("courses"), ent("invoices"), ent("lonely_thing"),
  ]; // 9 > 8

  it("groups by prefix; known prefixes get display labels; ungrouped → Khác (last)", () => {
    const g = groupEntities(many, []);
    expect(g.grouped).toBe(true);
    const labels = g.groups.map((x) => x.label);
    expect(labels).toEqual(["CRM", "Clinic", "Khác"]);
    const crm = g.groups.find((x) => x.key === "crm")!;
    expect(crm.items.map((e) => e.name)).toEqual(["crm_accounts", "crm_contacts", "crm_opportunities"]);
    const other = g.groups.find((x) => x.key === "__other")!;
    // singleton prefix (lonely_*) + un-prefixed names all land in Khác
    expect(other.items.map((e) => e.name)).toEqual(["tickets", "courses", "invoices", "lonely_thing"]);
  });

  it("unknown shared prefixes still group, labelled by uppercased prefix", () => {
    const g = groupEntities(
      [...many, ent("vet_animals"), ent("vet_owners")],
      [],
    );
    expect(g.groups.map((x) => x.label)).toContain("VET");
  });
});
