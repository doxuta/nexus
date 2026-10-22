// Rule on/off routing (specs/074 lát f2-draft-labels-rules T343/T348): on goes to the draft PUT, off for a rule the
// customer is running goes to "Tắt ngay cho khách" (disable-now), never a draft the customer would not feel.
import { describe, expect, it, vi } from "vitest";
import { ApiError, setWorkflowActive } from "@/lib/api";
import { onDraftsChanged } from "@/lib/draftEvents";
import { switchOffNeedsDisableNow, toggleRuleActive } from "./workflowToggle";

vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), setWorkflowActive: vi.fn() }));

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

// #53: on a delivered project PATCH /admin/workflows/:id/active writes a workflow draft (DraftWorkflowRepo.SetActive), so
// both rows that switch a rule (RuleListPage, RelatedRuleRow) must make the header recount: once the PATCH has answered,
// never before, never on a refusal.
describe("toggleRuleActive announces on the drafts bus", () => {
  it("after the switch has answered, never before, never on a refusal", async () => {
    const heard: string[] = [];
    const off = onDraftsChanged((s) => heard.push(s));
    try {
      let answer!: () => void;
      vi.mocked(setWorkflowActive).mockReturnValueOnce(new Promise((r) => { answer = () => r({ isActive: true } as never); }));
      const done = toggleRuleActive("w1", true);
      await new Promise((r) => setTimeout(r, 20));
      expect(heard).toEqual([]);
      answer();
      expect(await done).toEqual({ ok: true, isActive: true });
      expect(heard).toEqual(["rule-toggle"]);
      vi.mocked(setWorkflowActive).mockRejectedValueOnce(new ApiError(409, "WORKFLOW_LEGACY_SECRET", "x"));
      expect((await toggleRuleActive("w1", false)).ok).toBe(false);
      expect(heard).toEqual(["rule-toggle"]);
    } finally {
      off();
    }
  });
});
