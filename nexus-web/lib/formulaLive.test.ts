// formulaLive — unit tests for the debounced live-validation state machine (W5 §3.2).
import { describe, it, expect } from "vitest";
import {
  formulaReducer, initialFormulaState, shapeValidateRequest, type FormulaState,
} from "./formulaLive";

describe("formulaLive reducer", () => {
  it("empty input resets to idle (no request for an empty formula)", () => {
    let s: FormulaState = { kind: "valid", seq: 3, fields: ["a"], resultType: "number" };
    s = formulaReducer(s, { type: "input", formula: "   ", seq: 4 });
    expect(s).toEqual({ kind: "idle" });
  });

  it("happy path: input → debounce-fire → response-ok", () => {
    let s = initialFormulaState();
    s = formulaReducer(s, { type: "input", formula: "a * b", seq: 1 });
    expect(s).toEqual({ kind: "typing", seq: 1 });
    s = formulaReducer(s, { type: "debounce-fire", seq: 1 });
    expect(s).toEqual({ kind: "checking", seq: 1 });
    s = formulaReducer(s, { type: "response-ok", seq: 1, fields: ["a", "b"], resultType: "number" });
    expect(s).toEqual({ kind: "valid", seq: 1, fields: ["a", "b"], resultType: "number" });
  });

  it("error path carries the exact backend code + message", () => {
    let s = initialFormulaState();
    s = formulaReducer(s, { type: "input", formula: "missing * 2", seq: 1 });
    s = formulaReducer(s, { type: "debounce-fire", seq: 1 });
    s = formulaReducer(s, { type: "response-err", seq: 1, code: "FIELD_FORMULA_UNKNOWN_REF", message: "field không tồn tại" });
    expect(s).toEqual({ kind: "invalid", seq: 1, code: "FIELD_FORMULA_UNKNOWN_REF", message: "field không tồn tại" });
  });

  it("stale response is DROPPED when a newer keystroke arrived (out-of-order network)", () => {
    let s = initialFormulaState();
    s = formulaReducer(s, { type: "input", formula: "a", seq: 1 });
    s = formulaReducer(s, { type: "debounce-fire", seq: 1 });
    // User types again while request 1 is in flight.
    s = formulaReducer(s, { type: "input", formula: "a + b", seq: 2 });
    // Request 1's response lands late — must NOT override the newer typing state.
    const after = formulaReducer(s, { type: "response-ok", seq: 1, fields: ["a"], resultType: "number" });
    expect(after).toEqual({ kind: "typing", seq: 2 });
  });

  it("an old debounce timer firing after a newer keystroke is ignored", () => {
    let s = initialFormulaState();
    s = formulaReducer(s, { type: "input", formula: "a", seq: 1 });
    s = formulaReducer(s, { type: "input", formula: "ab", seq: 2 });
    const after = formulaReducer(s, { type: "debounce-fire", seq: 1 });
    expect(after).toEqual({ kind: "typing", seq: 2 }); // still waiting on seq 2's timer
  });
});

describe("shapeValidateRequest", () => {
  it("returns null for empty formula or missing entity", () => {
    expect(shapeValidateRequest("deals", "  ")).toBeNull();
    expect(shapeValidateRequest("", "a * 2")).toBeNull();
  });

  it("trims the formula and forwards resultType only for number|currency|text", () => {
    expect(shapeValidateRequest("deals", " a * 2 ", "currency")).toEqual({
      entityName: "deals", formula: "a * 2", resultType: "currency",
    });
    // specs/053: a text formula validated without its hint is judged as a number → a false 422
    // FIELD_FORMULA_TYPE_MISMATCH that blocks the inspector's Save on a valid stored field.
    expect(shapeValidateRequest("deals", 'IF(a > 1, "x", "y")', "text")).toEqual({
      entityName: "deals", formula: 'IF(a > 1, "x", "y")', resultType: "text",
    });
    expect(shapeValidateRequest("deals", "a", "boolean")).toEqual({ entityName: "deals", formula: "a" });
    expect(shapeValidateRequest("deals", "a")).toEqual({ entityName: "deals", formula: "a" });
  });
});
