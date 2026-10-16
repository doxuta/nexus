// Relation picker dependent filter (queue #304): options.filterBy on a relation field narrows its
// picker by a sibling relation on the same form (ticket.device by the chosen customer).
import { describe, expect, it } from "vitest";
import type { FieldMeta } from "@/lib/api";
import { clearedDependents, relationFilterChoice, relationFilterOf } from "./dependentPicklist";

const device: FieldMeta = {
  name: "device", label: "Thiết bị", fieldType: "relation",
  options: { relatedEntity: "device", filterBy: { field: "customer", parentField: "customer" } },
};
const customer: FieldMeta = { name: "customer", label: "Khách", fieldType: "relation", options: { relatedEntity: "customer" } };
const plain: FieldMeta = { name: "owner", label: "Người", fieldType: "relation", options: { relatedEntity: "user" } };

describe("relationFilterOf", () => {
  it("reads a well-formed filterBy", () => {
    expect(relationFilterOf(device.options)).toEqual({ field: "customer", parentField: "customer" });
  });
  it.each([undefined, {}, { filterBy: "x" }, { filterBy: { field: "a" } }, { filterBy: { field: "", parentField: "b" } }])(
    "ignores %j", (o) => expect(relationFilterOf(o as never)).toBeNull(),
  );
});

describe("relationFilterChoice — what the picker lists given its sibling values", () => {
  it("no filterBy: flat", () => {
    expect(relationFilterChoice(plain.options, { owner: "" })).toEqual({ kind: "flat" });
  });
  it("parent not on this form: flat, nothing to filter by", () => {
    expect(relationFilterChoice(device.options, { title: "x" })).toEqual({ kind: "flat" });
    expect(relationFilterChoice(device.options, undefined)).toEqual({ kind: "flat" });
  });
  it("parent empty: blocked, naming the parent", () => {
    expect(relationFilterChoice(device.options, { customer: "" })).toEqual({ kind: "blocked", parentField: "customer" });
  });
  it("parent chosen: filtered by target.field = parent id", () => {
    expect(relationFilterChoice(device.options, { customer: "c-1" })).toEqual({ kind: "filtered", field: "customer", value: "c-1" });
  });
  it("a parent that holds an expanded {id,label} object still filters by its id", () => {
    expect(relationFilterChoice(device.options, { customer: { id: "c-2", label: "A" } })).toEqual({ kind: "filtered", field: "customer", value: "c-2" });
  });
});

describe("clearedDependents — a relation parent change blanks the picker it filtered", () => {
  const fields = Object.fromEntries([device, customer, plain].map((f) => [f.name, f]));
  it("changing the customer blanks a chosen device", () => {
    expect(clearedDependents(fields, { customer: "c-2", device: "d-1" }, "customer")).toEqual({ device: "" });
  });
  it("an empty device is left alone; an unrelated field clears nothing", () => {
    expect(clearedDependents(fields, { customer: "c-2", device: "" }, "customer")).toEqual({});
    expect(clearedDependents(fields, { owner: "u", device: "d-1" }, "owner")).toEqual({});
  });
});
