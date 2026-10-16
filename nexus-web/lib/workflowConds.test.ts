// Tests for lib/workflowConds — WorkflowBuilder condition ops ↔ backend rule payload.
// BE contract (workflow.go validOperators + workflow_service.go buildConditionJSON):
// 11 ops eq/neq/gt/gte/lt/lte/contains/isnull/notnull/changed/changedTo; isnull/notnull/changed must NOT carry a value
// (422 WORKFLOW_CONDITION_INVALID), every other op requires one.
import { describe, expect, it } from "vitest";
import { OPS, OP_MAP, condToRule, isNullOp, ruleToCond } from "./workflowConds";

describe("OPS / OP_MAP", () => {
  it("offers ALL 11 backend operators, each mapped to a valid backend token", () => {
    // changed / changedTo are the edge operators: they compare the record against its own
    // before-image. Keeping this list exhaustive is the point of the test — an operator the
    // engine accepts but the builder never offers is a capability nobody can reach.
    const backend = [
      "eq", "neq", "gt", "gte", "lt", "lte", "contains", "isnull", "notnull",
      "changed", "changedTo",
    ];
    expect(OPS).toHaveLength(11);
    expect(OPS.map((o) => OP_MAP[o]).sort()).toEqual([...backend].sort());
  });

  it("changed carries NO value; changedTo does", () => {
    // The backend enforces both halves (WORKFLOW_CONDITION_INVALID), so a mismatch here is a rule
    // the builder can compose and the server refuses to save.
    expect(condToRule({ field: "status", op: "vừa đổi", val: "x" })).toEqual({
      field: "status", op: "changed",
    });
    expect(condToRule({ field: "status", op: "vừa đổi thành", val: "Chấp nhận" })).toEqual({
      field: "status", op: "changedTo", value: "Chấp nhận",
    });
  });

  it("maps the 4 previously-missing ops correctly", () => {
    expect(OP_MAP[">"]).toBe("gt");
    expect(OP_MAP["<"]).toBe("lt");
    expect(OP_MAP["trống"]).toBe("isnull");
    expect(OP_MAP["không trống"]).toBe("notnull");
  });
});

describe("isNullOp", () => {
  it("is true only for the null-operators (UI or backend token)", () => {
    expect(isNullOp("trống")).toBe(true);
    expect(isNullOp("không trống")).toBe(true);
    expect(isNullOp("isnull")).toBe(true);
    expect(isNullOp("notnull")).toBe(true);
    for (const o of ["=", "≠", ">", "≥", "<", "≤", "chứa", "eq", "contains"]) {
      expect(isNullOp(o)).toBe(false);
    }
  });
});

describe("condToRule", () => {
  it("value-bearing ops include the value", () => {
    expect(condToRule({ field: "amount", op: ">", val: "100" })).toEqual({ field: "amount", op: "gt", value: "100" });
    expect(condToRule({ field: "amount", op: "<", val: "5" })).toEqual({ field: "amount", op: "lt", value: "5" });
    expect(condToRule({ field: "stage", op: "=", val: "won" })).toEqual({ field: "stage", op: "eq", value: "won" });
    expect(condToRule({ field: "name", op: "chứa", val: "vip" })).toEqual({ field: "name", op: "contains", value: "vip" });
  });

  it("isnull/notnull OMIT the value key entirely (BE rejects a value on null-operators)", () => {
    const isnull = condToRule({ field: "phone", op: "trống", val: "typed-then-switched" });
    expect(isnull).toEqual({ field: "phone", op: "isnull" });
    expect("value" in isnull).toBe(false);
    const notnull = condToRule({ field: "phone", op: "không trống", val: "" });
    expect(notnull).toEqual({ field: "phone", op: "notnull" });
    expect("value" in notnull).toBe(false);
  });
});

describe("ruleToCond (loadWorkflow restore)", () => {
  it("reverse-maps backend tokens to UI tokens and round-trips", () => {
    expect(ruleToCond({ field: "amount", op: "gt", value: "100" })).toEqual({ field: "amount", op: ">", val: "100" });
    expect(ruleToCond({ field: "amount", op: "lte", value: "9" })).toEqual({ field: "amount", op: "≤", val: "9" });
    for (const op of OPS) {
      const round = ruleToCond(condToRule({ field: "f", op, val: "x" }));
      expect(round.op).toBe(op);
    }
  });

  it("renders a null-operator's absent value as an empty string", () => {
    expect(ruleToCond({ field: "phone", op: "isnull" })).toEqual({ field: "phone", op: "trống", val: "" });
    expect(ruleToCond({ field: "phone", op: "notnull", value: null })).toEqual({ field: "phone", op: "không trống", val: "" });
  });

  it("passes an unknown backend op through untouched (forward-compat)", () => {
    expect(ruleToCond({ field: "f", op: "in", value: "a,b" })).toEqual({ field: "f", op: "in", val: "a,b" });
  });
});

describe("field reference value (queue #286)", () => {
  it("round-trips @field:<name> through the stored {field} object", () => {
    const rule = condToRule({ field: "a", op: "gte", val: "@field:b" });
    expect(rule).toEqual({ field: "a", op: "gte", value: { field: "b" } });
    expect(ruleToCond(rule).val).toBe("@field:b");
  });
});
