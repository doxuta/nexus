import { describe, expect, it } from "vitest";
import type { FieldMeta } from "@/lib/api";
import { isFromTriggerFor, sourceFieldName, triggerSourceOptions } from "@/lib/workflowValueSource";

const f = (name: string, fieldType: string, extra: Partial<FieldMeta> = {}): FieldMeta =>
  ({ name, label: name.toUpperCase(), fieldType, ...extra }) as FieldMeta;

describe("triggerSourceOptions — what a rule value may be taken from on the triggering record", () => {
  const ruleFields = [
    f("due_date", "date"), f("paid_at", "date"), f("note", "text"), f("total", "currency"),
    f("created_at", "datetime"), f("customer", "relation", { options: { relatedEntity: "customers" } }),
  ];

  it("offers only same-type fields, never system columns", () => {
    const opts = triggerSourceOptions(f("visit_date", "date"), ruleFields, "invoices");
    expect(opts.map((o) => o.value)).toEqual(["$record.due_date", "$record.paid_at"]);
  });

  it("offers the record itself to a relation that points back at the rule's entity", () => {
    const opts = triggerSourceOptions(f("invoice", "relation", { options: { relatedEntity: "invoices" } }), ruleFields, "invoices");
    expect(opts.map((o) => o.value)).toContain("$record");
  });

  it("a relation to another entity only gets relation fields to the SAME entity, and not the record itself", () => {
    const opts = triggerSourceOptions(f("buyer", "relation", { options: { relatedEntity: "customers" } }), ruleFields, "invoices");
    expect(opts.map((o) => o.value)).toEqual(["$record.customer"]);
  });

  it("nothing fits ⇒ empty (the picker is not shown)", () => {
    expect(triggerSourceOptions(f("qty", "number"), ruleFields, "invoices")).toEqual([]);
  });
});

describe("sourceFieldName", () => {
  it("reads a $record.<field> value and nothing else", () => {
    expect(sourceFieldName("$record.due_date")).toBe("due_date");
    expect(sourceFieldName("$record")).toBeUndefined();
    expect(sourceFieldName("2026-01-01")).toBeUndefined();
    expect(sourceFieldName(42)).toBeUndefined();
  });
});

describe("isFromTriggerFor — typing '$record.' into a text box is not a source yet", () => {
  const rf = [f("name", "text"), f("due", "date")];
  it("keeps the input while a half-typed reference names no real field", () => {
    expect(isFromTriggerFor("$record.", f("memo", "text"), rf)).toBe(false);
    expect(isFromTriggerFor("$record.na", f("memo", "text"), rf)).toBe(false);
    expect(isFromTriggerFor("$record.name", f("memo", "text"), rf)).toBe(true);
  });
  it("non-text targets have no typed form, so the prefix alone counts", () => {
    expect(isFromTriggerFor("$record.gone", f("visit", "date"), rf)).toBe(true);
  });
});
