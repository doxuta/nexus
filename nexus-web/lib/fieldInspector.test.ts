import { describe, expect, it } from "vitest";
import type { FieldMeta } from "@/lib/api";
import { relabelRow } from "@/lib/picklistRows";
import { changedUpdate, draftFromField, fullUpdate } from "./fieldInspector";

const text: FieldMeta = { id: "f1", name: "phone", label: "Điện thoại", fieldType: "text", isRequired: false, options: { maxLength: 20 } };
const status: FieldMeta = {
  id: "f2", name: "status", label: "Trạng thái", fieldType: "picklist", isRequired: true,
  options: { values: ["new", "won"], valueLabels: { new: "Mới" }, indexed: true },
};
const relation: FieldMeta = {
  id: "f3", name: "account", label: "Khách hàng", fieldType: "relation",
  options: { relatedEntity: "crm_accounts", cardinality: "1-N" },
};

describe("changedUpdate — only what the maker changed since opening the field (#92)", () => {
  it("an untouched inspector saves nothing", () => {
    for (const f of [text, status, relation]) expect(changedUpdate(f, draftFromField(f))).toEqual({});
  });

  it("a rename sends the label alone", () => {
    expect(changedUpdate(text, { ...draftFromField(text), label: "Số điện thoại" })).toEqual({ label: "Số điện thoại" });
  });

  it("the Bắt buộc switch sends isRequired alone", () => {
    expect(changedUpdate(text, { ...draftFromField(text), isRequired: true })).toEqual({ isRequired: true });
  });

  it("the nhạy cảm switch sends the options (full replacement) and nothing else", () => {
    expect(changedUpdate(text, { ...draftFromField(text), sensitive: true })).toEqual({
      options: { maxLength: 20, sensitive: true },
    });
  });

  it("a picklist label edit keeps the stored keys the inspector does not own", () => {
    const d = draftFromField(status);
    const next = { ...d, rows: relabelRow(d.rows, d.rows[1].id, "Thắng") };
    expect(changedUpdate(status, next)).toEqual({
      options: { values: ["new", "won"], valueLabels: { new: "Mới", won: "Thắng" }, indexed: true },
    });
  });

  it("an edit reverted by hand is not a change", () => {
    const d = { ...draftFromField(text), label: "X" };
    expect(changedUpdate(text, { ...d, label: "Điện thoại" })).toEqual({});
  });

  it("relation options are never sent (immutable after create)", () => {
    const u = fullUpdate(relation, { ...draftFromField(relation), label: "KH" });
    expect(u).not.toHaveProperty("options");
    expect(changedUpdate(relation, { ...draftFromField(relation), label: "KH" })).toEqual({ label: "KH" });
  });

  it("clearing a default value is sent as an explicit empty string", () => {
    const withDefault: FieldMeta = { ...text, defaultValue: "0900" };
    expect(changedUpdate(withDefault, { ...draftFromField(withDefault), defaultScalar: "" })).toEqual({ defaultValue: "" });
  });
});

// specs/052 #37: the inspector OWNS controllingField + dependentValues on a single picklist.
describe("fieldInspector — dependent picklist config (#37)", () => {
  const district: FieldMeta = {
    id: "f-d", name: "district", label: "Quận/Huyện", fieldType: "picklist",
    options: {
      values: ["Ba Đình", "Hoàn Kiếm", "Hải Châu"],
      controllingField: "province",
      dependentValues: { "Hà Nội": ["Ba Đình", "Hoàn Kiếm"], "Đà Nẵng": ["Hải Châu"] },
      indexed: true,
    },
  };

  it("the draft reads the pair, and an untouched inspector saves nothing", () => {
    const d = draftFromField(district);
    expect(d.controllingField).toBe("province");
    expect(d.dependentValues).toEqual({ "Hà Nội": ["Ba Đình", "Hoàn Kiếm"], "Đà Nẵng": ["Hải Châu"] });
    expect(changedUpdate(district, d)).toEqual({});
    expect(draftFromField(status).controllingField).toBe("");
  });

  it("a new mapping is sent in the options, every other stored key carried", () => {
    const d = draftFromField(status);
    expect(changedUpdate(status, { ...d, controllingField: "stage", dependentValues: { open: ["new"] } })).toEqual({
      options: { values: ["new", "won"], valueLabels: { new: "Mới" }, indexed: true, controllingField: "stage", dependentValues: { open: ["new"] } },
    });
  });

  it("removing a child value takes it out of the map in the same save (else 422 FR-2)", () => {
    const d = draftFromField(district);
    const rows = d.rows.filter((r) => r.value !== "Hoàn Kiếm");
    expect(changedUpdate(district, { ...d, rows }).options).toEqual({
      values: ["Ba Đình", "Hải Châu"],
      controllingField: "province",
      dependentValues: { "Hà Nội": ["Ba Đình"], "Đà Nẵng": ["Hải Châu"] },
      indexed: true,
    });
  });

  it("'Không phụ thuộc' drops both keys", () => {
    const d = draftFromField(district);
    expect(changedUpdate(district, { ...d, controllingField: "", dependentValues: {} }).options).toEqual({
      values: ["Ba Đình", "Hoàn Kiếm", "Hải Châu"], indexed: true,
    });
  });
});

describe("fieldInspector — relation filterBy (#304)", () => {
  it("setting the filter sends the stored options whole plus filterBy; nothing else is dropped", () => {
    const d = { ...draftFromField(relation), filterParent: "customer", filterField: "customer" };
    expect(changedUpdate(relation, d).options).toEqual({
      relatedEntity: "crm_accounts", cardinality: "1-N", filterBy: { field: "customer", parentField: "customer" },
    });
  });

  it("an existing filter round-trips untouched and clearing it removes only that key", () => {
    const filtered: FieldMeta = { ...relation, options: { ...relation.options, filterBy: { field: "customer", parentField: "customer" } } };
    const d = draftFromField(filtered);
    expect([d.filterParent, d.filterField]).toEqual(["customer", "customer"]);
    expect(changedUpdate(filtered, d)).toEqual({});
    expect(changedUpdate(filtered, { ...d, filterParent: "", filterField: "" }).options).toEqual({
      relatedEntity: "crm_accounts", cardinality: "1-N",
    });
  });

  it("a half-filled pair is not a filter", () => {
    expect(changedUpdate(relation, { ...draftFromField(relation), filterParent: "customer" })).toEqual({});
  });
});

describe("stored roll-up (specs/083)", () => {
  const stored: FieldMeta = {
    id: "f9", name: "si_so", label: "Sĩ số", fieldType: "rollup",
    options: { childEntity: "enrollments", viaRelation: "class", metric: "count", stored: true, valueType: "number" },
  };
  it("reads stored from options and resends it on a relabel (the BE refuses a change of definition)", () => {
    const d = draftFromField(stored);
    expect(d.rollup.stored).toBe(true);
    expect(fullUpdate(stored, d).options).toMatchObject({ childEntity: "enrollments", viaRelation: "class", metric: "count", stored: true });
  });
  it("a plain roll-up stays unstored", () => {
    const plain: FieldMeta = { ...stored, options: { childEntity: "enrollments", viaRelation: "class", metric: "count" } };
    expect(draftFromField(plain).rollup.stored).toBe(false);
    expect(fullUpdate(plain, draftFromField(plain)).options).not.toHaveProperty("stored");
  });
});
