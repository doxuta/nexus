// specs/048 S4 keys of a before_save field_update survive open → save (T156 round-trip part).
import { describe, it, expect } from "vitest";
import { fuParamsFromAction, fuPayload, fuValueMissing } from "./workflowFieldUpdate";

const roundTrip = (a: Record<string, unknown>) => fuPayload(fuParamsFromAction(a), []);

describe("before_save field_update keys round-trip", () => {
  it("a {{template}} and a copied field travel back as the sentence, with valueSource and onlyIfEmpty", () => {
    const tpl = { type: "field_update", field: "full_name", value: "{{ho}} {{ten}}", valueSource: "template", onlyIfEmpty: true };
    expect(roundTrip(tpl)).toEqual(tpl);
    const copy = { type: "field_update", field: "billing_email", value: "email", valueSource: "field" };
    expect(roundTrip(copy)).toEqual(copy);
  });
  it("an empty template counts as a missing value; a constant stays typed", () => {
    expect(fuValueMissing({ field: "x", valueSource: "template", srcValue: " " }, [])).toBe(true);
    expect(fuValueMissing({ field: "x", valueSource: "field", srcValue: "email" }, [])).toBe(false);
    expect(roundTrip({ type: "field_update", field: "probability", value: 100, onlyIfEmpty: true }))
      .toEqual({ type: "field_update", field: "probability", value: 100, onlyIfEmpty: true });
  });
});
