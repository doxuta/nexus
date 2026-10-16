import { describe, expect, it } from "vitest";
import { classOf } from "./draftClass";

const base = { delivered: true, draftableTypes: ["workflow", "report"], directTypes: ["role", "permission"] };

describe("classOf (specs/073 FR-113)", () => {
  it.each([
    ["workflow", "draft"],
    ["report", "draft"],
    ["role", "direct"],
    ["permission", "direct"],
    ["something-new", "untraced"],
  ] as const)("delivered project: %s -> %s", (docType, want) => {
    expect(classOf(docType, base)).toBe(want);
  });

  it("not delivered: every type is 'chưa giao' (writes go straight through, no draft label)", () => {
    expect(classOf("workflow", { ...base, delivered: false })).toBe("chưa giao");
    expect(classOf("role", { ...base, delivered: false })).toBe("chưa giao");
  });

  it("state not loaded yet: 'chưa giao' (never guess a draft label)", () => {
    expect(classOf("workflow", undefined)).toBe("chưa giao");
  });

  it("a type listed in both lists is a draft (draft wins)", () => {
    expect(classOf("x", { ...base, draftableTypes: ["x"], directTypes: ["x"] })).toBe("draft");
  });
});
