import { describe, it, expect } from "vitest";
import { contrastOnWhite, CONTRAST_MIN } from "./contrast";

describe("contrastOnWhite", () => {
  it("passes the stock brand colour", () => expect(contrastOnWhite("#0176d3")!).toBeGreaterThanOrEqual(CONTRAST_MIN));
  it("flags gold", () => expect(contrastOnWhite("#ffd700")!).toBeCloseTo(1.4, 1));
  it("black on white is 21", () => expect(contrastOnWhite("#000000")!).toBeCloseTo(21, 5));
  it("invalid input yields null, never throws", () => {
    for (const bad of ["", "red", "#12", "#gggggg", undefined]) expect(contrastOnWhite(bad)).toBeNull();
  });
  it("the classic 4.54:1 grey is not below the threshold", () => {
    expect(contrastOnWhite("#767676")!).toBeGreaterThanOrEqual(CONTRAST_MIN);
  });
});
