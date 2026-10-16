// specs/048 — "before" workflow triggers (before_save / before_delete): rules that run inside a save
// or delete and can refuse it. Mirror of the server's config-time rules
// (nexus-platform/internal/services/workflow_before.go) so the Studio only offers what the server
// accepts, and says where a refusal belongs on the editor.
import { t } from "@/lib/i18n";

export const BEFORE_TRIGGERS = ["before_save", "before_delete"] as const;
export type BeforeTrigger = (typeof BEFORE_TRIGGERS)[number];

export function isBeforeTrigger(trigger: string | null | undefined): trigger is BeforeTrigger {
  return (BEFORE_TRIGGERS as readonly string[]).includes(trigger ?? "");
}

const LABEL_KEYS = {
  before_save: "workflow.trigger.before_save",
  before_delete: "workflow.trigger.before_delete",
} as const;

export function beforeTriggerLabel(trigger: BeforeTrigger): string {
  return t(LABEL_KEYS[trigger]);
}

// Actions a before rule may carry — the server's beforeAllowedActions narrowed by
// beforeMutationShipped, i.e. what it ACCEPTS today. Nothing with an outside effect ever joins.
// S4/S5 shipped: before_save also takes field_update (tự điền) and normalize (chuẩn hoá).
export const BEFORE_MUTATIONS: readonly string[] = ["field_update", "normalize"];
const BEFORE_ACTIONS: Record<BeforeTrigger, readonly string[]> = {
  before_save: ["veto", ...BEFORE_MUTATIONS],
  before_delete: ["veto"],
};
// Actions that only make sense inside a save (the server refuses them on after_* rules).
const BEFORE_ONLY_ACTIONS = new Set(["veto", "normalize"]);

export function beforeActionsFor(trigger: BeforeTrigger): readonly string[] {
  return BEFORE_ACTIONS[trigger];
}

/** Whether a rule with this trigger may carry this action type. */
export function actionAllowed(trigger: string, actionType: string): boolean {
  // A deleted record has nothing left to approve (WF_START_APPROVAL_INVALID on after_delete).
  if (actionType === "start_approval" && trigger === "after_delete") return false;
  return isBeforeTrigger(trigger) ? BEFORE_ACTIONS[trigger].includes(actionType) : !BEFORE_ONLY_ACTIONS.has(actionType);
}

/** condition.saveOn: "" = create and update (absent on the wire), "create", "update". before_save only. */
export type SaveOn = "" | "create" | "update";
export const saveOnOffered = (trigger: string) => trigger === "before_save";

/** "changed"/"changedTo" compare with the record before this write, and only an update has one:
 *  after_update, or a before_save rule that also runs on updates. A create, a delete or a timed run
 *  has none, so the server refuses them there (WORKFLOW_CONDITION_NEVER_TRUE, checkBeforeRule). */
export const EDGE_OPS: readonly string[] = ["changed", "changedTo"];
export function edgeOpsOff(trigger: string, saveOn: string): boolean {
  return !(trigger === "after_update" || (trigger === "before_save" && saveOn !== "create"));
}

/** A veto message is 1–500 characters after trimming, counted as the server does (runes). */
export const MAX_VETO_MESSAGE = 500;
export const vetoMessageLength = (msg: string) => Array.from(msg.trim()).length;

/** A before rule is either one refusal (veto) or a list of value-changing actions (≤10), never both
 *  (server: WORKFLOW_BEFORE_ACTIONS_MIXED) — so the picker hides the other family once one is present. */
export const MAX_BEFORE_ACTIONS = 10;
export function beforeActionOffered(existing: readonly string[], type: string): boolean {
  const hasVeto = existing.includes("veto");
  const hasMutation = existing.some((x) => BEFORE_MUTATIONS.includes(x));
  if (type === "veto") return !hasMutation && !hasVeto;
  return !hasVeto && existing.length < MAX_BEFORE_ACTIONS;
}

/** Where a before-rule config refusal belongs on the editor; null = the footer. */
export type BeforeErrorSlot = "vetoMessage" | "saveOn" | "conditions";
const ERROR_SLOTS: Record<string, BeforeErrorSlot> = {
  WORKFLOW_VETO_MESSAGE_INVALID: "vetoMessage",
  WORKFLOW_SAVE_ON_INVALID: "saveOn",
  WORKFLOW_CONDITION_NEVER_TRUE: "conditions",
};
export function beforeErrorSlot(code: string): BeforeErrorSlot | null {
  return ERROR_SLOTS[code] ?? null;
}

// Mirrors of the server's beforeTargetUnsupported / beforeTextTarget / beforeTemplateSource
// (services/workflow_before.go) so the editor only offers what the server accepts.
const TARGET_BLOCKED = new Set(["relation", "polymorphic_relation", "user", "file", "image", "formula", "lookup", "rollup", "auto_number"]);
const TEXT_TARGET = new Set(["text", "email", "rich_text"]);
const TEMPLATE_SOURCE = new Set(["text", "email", "rich_text", "number", "rating", "boolean", "date", "datetime", "picklist", "auto_number"]);
export const beforeTargetOk = (fieldType: string) => !TARGET_BLOCKED.has(fieldType);
export const beforeTemplateTarget = (fieldType: string) => TEXT_TARGET.has(fieldType);
export const beforeTemplateSourceOk = (fieldType: string) => TEMPLATE_SOURCE.has(fieldType);

// ── Dry-run of a before rule (FE-before-save-hooks-4) ───────────────────────────────────────
export const NUMERIC_FIELD_TYPES = new Set(["number", "currency", "rating", "percent"]);

/** One simulated change as the wire carries it: numbers as numbers, booleans as booleans, the rest as typed. */
export function parseChangeValue(fieldType: string, raw: string): unknown {
  if (NUMERIC_FIELD_TYPES.has(fieldType)) return raw.trim() === "" ? null : Number(raw);
  if (fieldType === "boolean") return raw === "true";
  return raw;
}

export interface ChangeRow { field: string; value: string }

/** Rows → the `changes` object of POST /admin/workflows/:id/test; a row without a field is skipped. */
export function changesFromRows(rows: ChangeRow[], typeOf: (field: string) => string): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const r of rows) if (r.field) out[r.field] = parseChangeValue(typeOf(r.field), r.value);
  return out;
}
