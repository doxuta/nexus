// specs/048 T147 — the Studio's before-rule table mirrors what the server accepts today.
// Server side (nexus-platform/internal/services/workflow_before.go): beforeAllowedActions narrowed by
// beforeMutationShipped — only `veto` is accepted until S4/S5 ship field_update/normalize — and
// TestBuildWorkflowRule_Before_RejectsSideEffectActions (T007) refuses every side-effect action on
// both before triggers. saveOn is before_save-only; "changed"/"changedTo" fire only on an update —
// never on a create, a delete or a timed run (WORKFLOW_CONDITION_NEVER_TRUE).
import { describe, it, expect } from "vitest";
import {
  BEFORE_TRIGGERS, actionAllowed, beforeActionOffered, beforeActionsFor, beforeErrorSlot, edgeOpsOff, isBeforeTrigger, saveOnOffered, vetoMessageLength,
} from "./workflowBefore";

// T007's list: every action with an outside effect.
const SIDE_EFFECTS = ["email_alert", "outbound_webhook", "in_app_notification", "assign", "create_record", "set_deadline", "set_elapsed"];
const AFTER = ["after_create", "after_update", "after_delete", "scheduled", "date_offset"];

describe("before triggers", () => {
  it("recognises exactly before_save and before_delete", () => {
    expect([...BEFORE_TRIGGERS]).toEqual(["before_save", "before_delete"]);
    expect(isBeforeTrigger("before_save")).toBe(true);
    expect(isBeforeTrigger("after_update")).toBe(false);
    expect(isBeforeTrigger(undefined)).toBe(false);
  });
});

describe("trigger → action table", () => {
  it("before_delete offers the refusal only; before_save also the two value-changing actions (S4/S5)", () => {
    expect(beforeActionsFor("before_delete")).toEqual(["veto"]);
    expect(beforeActionsFor("before_save")).toEqual(["veto", "field_update", "normalize"]);
    for (const trigger of BEFORE_TRIGGERS) {
      expect(actionAllowed(trigger, "veto")).toBe(true);
      for (const a of SIDE_EFFECTS) expect(actionAllowed(trigger, a), a).toBe(false);
    }
    expect(actionAllowed("before_save", "field_update")).toBe(true);
    expect(actionAllowed("before_save", "normalize")).toBe(true);
    expect(actionAllowed("before_delete", "field_update")).toBe(false);
    expect(actionAllowed("before_delete", "normalize")).toBe(false);
  });

  it("a veto and the value-changing actions never share a rule; at most 10 actions", () => {
    expect(beforeActionOffered([], "veto")).toBe(true);
    expect(beforeActionOffered(["normalize"], "veto")).toBe(false);
    expect(beforeActionOffered(["veto"], "field_update")).toBe(false);
    expect(beforeActionOffered(["field_update", "normalize"], "normalize")).toBe(true);
    expect(beforeActionOffered(Array(10).fill("normalize"), "field_update")).toBe(false);
  });

  it.each(AFTER)("%s keeps every after action and refuses the before-only ones", (trigger) => {
    for (const a of [...SIDE_EFFECTS, "field_update"]) expect(actionAllowed(trigger, a), a).toBe(true);
    expect(actionAllowed(trigger, "veto")).toBe(false);
    expect(actionAllowed(trigger, "normalize")).toBe(false);
  });
});

describe("saveOn and the edge operators", () => {
  it("saveOn is offered on before_save only", () => {
    expect(saveOnOffered("before_save")).toBe(true);
    for (const t of ["before_delete", ...AFTER]) expect(saveOnOffered(t), t).toBe(false);
  });

  it.each([
    ["before_save", "", false],
    ["before_save", "update", false],
    ["before_save", "create", true],
    ["before_delete", "", true],
    ["after_update", "", false],
    // No before-image at all: the dispatcher evaluates a create, a delete and a timed run with none.
    ["after_create", "", true],
    ["after_delete", "", true],
    ["scheduled", "", true],
    ["date_offset", "", true],
  ])("%s saveOn=%j → edge operators off: %s", (trigger, saveOn, off) => {
    expect(edgeOpsOff(trigger, saveOn)).toBe(off);
  });
});

describe("veto message length counts characters the way the server does", () => {
  it("trims and counts code points", () => {
    expect(vetoMessageLength("  Chốt Thắng phải có Ngày chốt  ")).toBe(28);
    expect(vetoMessageLength("😀")).toBe(1);
    expect(vetoMessageLength("   ")).toBe(0);
  });
});

describe("config error code → editor slot", () => {
  it.each([
    ["WORKFLOW_VETO_MESSAGE_INVALID", "vetoMessage"],
    ["WORKFLOW_SAVE_ON_INVALID", "saveOn"],
    ["WORKFLOW_CONDITION_NEVER_TRUE", "conditions"],
    ["WORKFLOW_ACTION_NOT_ALLOWED_FOR_TRIGGER", null],
    ["WORKFLOW_BEFORE_NOT_ENFORCED", null],
    ["SOMETHING_ELSE", null],
  ])("%s → %s", (code, slot) => {
    expect(beforeErrorSlot(code)).toBe(slot);
  });
});
