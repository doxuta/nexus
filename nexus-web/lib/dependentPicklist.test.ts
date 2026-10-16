// Dependent picklists on the client (specs/052-dependent-picklist): what a child picklist may list
// for the value its controlling field holds, which children a parent change blanks, and what the
// Studio inspector warns about while the pair is being configured.
import { describe, expect, it } from "vitest";
import type { FieldMeta } from "@/lib/api";
import {
  clearedDependents, controllingCandidates, dependencyWarnings, dependentChoice, pruneDependentValues,
  parseDependentCsv, visibilityMismatch,
} from "./dependentPicklist";

const MAP = { "Hà Nội": ["Ba Đình", "Hoàn Kiếm"], "Đà Nẵng": ["Hải Châu", "Sơn Trà"], "Huế": [] };
const districtOpts = { values: ["Ba Đình", "Hải Châu", "Hoàn Kiếm", "Sơn Trà"], controllingField: "province", dependentValues: MAP };

const province: FieldMeta = { name: "province", label: "Tỉnh/TP", fieldType: "picklist", options: { values: ["Hà Nội", "Đà Nẵng", "Huế"] } };
const district: FieldMeta = { name: "district", label: "Quận/Huyện", fieldType: "picklist", options: districtOpts };
const ward: FieldMeta = {
  name: "ward", label: "Phường/Xã", fieldType: "picklist",
  options: { values: ["Phúc Xá", "Hàng Bạc"], controllingField: "district", dependentValues: { "Ba Đình": ["Phúc Xá"], "Hoàn Kiếm": ["Hàng Bạc"] } },
};
const stage: FieldMeta = { name: "stage", label: "Giai đoạn", fieldType: "picklist", options: { values: ["new", "won"] } };
const byName = (...fs: FieldMeta[]) => Object.fromEntries(fs.map((f) => [f.name, f]));

describe("dependentChoice — what a picklist lists given its sibling values", () => {
  it("a picklist with no controlling field stays flat", () => {
    expect(dependentChoice(stage.options, { stage: "", province: "Hà Nội" })).toEqual({ kind: "flat" });
  });

  it("the controlling field is not on this form (hidden from the role, left off the layout): flat, the server judges", () => {
    expect(dependentChoice(districtOpts, { name: "x" })).toEqual({ kind: "flat" });
    expect(dependentChoice(districtOpts, undefined)).toEqual({ kind: "flat" });
  });

  it("an empty controlling field blocks the child until it is chosen", () => {
    expect(dependentChoice(districtOpts, { province: "" })).toEqual({ kind: "blocked", controllingField: "province" });
    expect(dependentChoice(districtOpts, { province: null })).toEqual({ kind: "blocked", controllingField: "province" });
  });

  it("a chosen parent lists only its children, in the child's own value order", () => {
    expect(dependentChoice(districtOpts, { province: "Hà Nội" })).toEqual({ kind: "filtered", values: ["Ba Đình", "Hoàn Kiếm"] });
    expect(dependentChoice(districtOpts, { province: "Đà Nẵng" })).toEqual({ kind: "filtered", values: ["Hải Châu", "Sơn Trà"] });
  });

  it("a parent value the map does not list, or lists with no child, allows nothing", () => {
    expect(dependentChoice(districtOpts, { province: "Huế" })).toEqual({ kind: "filtered", values: [] });
    expect(dependentChoice(districtOpts, { province: "Cần Thơ" })).toEqual({ kind: "filtered", values: [] });
  });
});

describe("clearedDependents — a parent change blanks the children it no longer allows", () => {
  const fields = byName(province, district, ward, stage);

  it("switching the province blanks a district of the old one", () => {
    expect(clearedDependents(fields, { province: "Đà Nẵng", district: "Ba Đình", ward: "" }, "province")).toEqual({ district: "" });
  });

  it("a district the new province still allows is kept", () => {
    expect(clearedDependents(fields, { province: "Hà Nội", district: "Ba Đình" }, "province")).toEqual({});
  });

  it("clearing the province blanks the district, and the ward under it (the chain cascades)", () => {
    expect(clearedDependents(fields, { province: "", district: "Ba Đình", ward: "Phúc Xá" }, "province")).toEqual({ district: "", ward: "" });
  });

  it("a field nobody depends on clears nothing; an already empty child is left alone", () => {
    expect(clearedDependents(fields, { stage: "won", district: "Ba Đình" }, "stage")).toEqual({});
    expect(clearedDependents(fields, { province: "Huế", district: "" }, "province")).toEqual({});
  });

  it("a child locked for the caller's role is never blanked on screen (it is not sent either)", () => {
    const locked = byName(province, { ...district, readOnly: true });
    expect(clearedDependents(locked, { province: "Đà Nẵng", district: "Ba Đình" }, "province")).toEqual({});
  });

  it("a child not on the form is not invented into the values", () => {
    expect(clearedDependents(fields, { province: "Đà Nẵng" }, "province")).toEqual({});
  });
});

describe("pruneDependentValues — the map never outlives the child's own values", () => {
  it("drops a child value removed from the list (else 422 FR-2 on save) and keeps every key", () => {
    expect(pruneDependentValues(MAP, ["Ba Đình", "Hải Châu", "Sơn Trà"])).toEqual({
      "Hà Nội": ["Ba Đình"], "Đà Nẵng": ["Hải Châu", "Sơn Trà"], "Huế": [],
    });
  });
});

describe("controllingCandidates — which fields may control this one", () => {
  it("other single picklists only — never itself, a multi_select or a text field", () => {
    const ms: FieldMeta = { name: "tags", fieldType: "multi_select", options: { values: ["a"] } };
    const txt: FieldMeta = { name: "note", fieldType: "text" };
    expect(controllingCandidates([province, district, stage, ms, txt], "district").map((f) => f.name)).toEqual(["province", "stage"]);
  });

  it("a field that already depends on this one (directly or down the chain) would close a cycle", () => {
    expect(controllingCandidates([province, district, ward, stage], "province").map((f) => f.name)).toEqual(["stage"]);
  });
});

describe("dependencyWarnings — what the Studio inspector flags", () => {
  const parentValues = ["Hà Nội", "Huế"];
  const childValues = ["Ba Đình", "Hải Châu", "Hoàn Kiếm", "Sơn Trà", "Cầu Giấy"];

  it("child values no parent value allows (nobody can ever pick them) and keys the parent dropped", () => {
    const w = dependencyWarnings({ childValues, dependentValues: MAP, parentValues });
    // Đà Nẵng was dropped from the parent, so its districts are orphans too.
    expect(w.orphanChildren).toEqual(["Hải Châu", "Sơn Trà", "Cầu Giấy"]);
    expect(w.orphanKeys).toEqual(["Đà Nẵng"]);
  });
});

describe("visibilityMismatch — roles that see the pair at different levels", () => {
  it("names the roles where the two fields differ (hidden / read-only / default)", () => {
    const grants = [
      { roleId: "r-sales", entityName: "crm_contacts", fieldName: "district", canView: true, canEdit: false },
      { roleId: "r-ops", entityName: "crm_contacts", fieldName: "province", canView: false, canEdit: false },
      { roleId: "r-ops", entityName: "crm_contacts", fieldName: "district", canView: false, canEdit: false },
      { roleId: "r-mkt", entityName: "crm_contacts", fieldName: "province", canView: true, canEdit: false },
      { roleId: "r-mkt", entityName: "crm_contacts", fieldName: "stage", canView: false, canEdit: false },
    ];
    expect(visibilityMismatch(grants, "province", "district")).toEqual(["r-sales", "r-mkt"]);
  });
});

describe("parseDependentCsv (queue #254)", () => {
  const parents = ["HN", "HCM"];
  const label = (v: string) => (v === "HN" ? "Hà Nội" : v);
  it("builds the map, merges duplicates, matches parent by code or label", () => {
    const r = parseDependentCsv('HN,Ba Đình\nHà Nội,Cầu Giấy\nHN,Ba Đình\nHCM,"Quận 1"', parents, label);
    expect(r.errors).toEqual([]);
    expect(r.map).toEqual({ HN: ["Ba Đình", "Cầu Giấy"], HCM: ["Quận 1"] });
    expect(r.children).toEqual(["Ba Đình", "Cầu Giấy", "Quận 1"]);
  });
  it("reports unknown parent / malformed line by line number", () => {
    const r = parseDependentCsv("HN,A\nXX,B\nonlyone\n\nHCM,", parents, label);
    expect(r.errors.map((e) => e.line)).toEqual([2, 3, 5]);
  });
});
