// Rule on/off routing (specs/074 lát f2-draft-labels-rules T343/T348): on goes to the draft PUT, off for a rule the
// customer is running goes to "Tắt ngay cho khách" (disable-now), never a draft the customer would not feel.
import { describe, expect, it } from "vitest";
import { switchOffNeedsDisableNow } from "./workflowToggle";

describe("switchOffNeedsDisableNow", () => {
  it.each([
    ["live", true, true],
    ["edited", true, true],
    ["live", false, false], // already off: nothing live to stop
    ["new", true, false], // draft only: the customer has nothing running
    ["deleted", true, false],
    [undefined, true, false], // workspace not delivered: plain write
  ] as const)("draftState %s active %s -> %s", (draftState, isActive, want) => {
    expect(switchOffNeedsDisableNow({ draftState, isActive })).toBe(want);
  });
});
