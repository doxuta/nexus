// lib/packUpgrade.ts — pure helpers of the "Nâng cấp một gói" page (specs/079 f7-nang-cap-*, plan.md 5a).
// The closed label table: every status/action/outcome/mode the server can send has a Vietnamese label; anything
// else falls to "Mục khác" and never prints a machine code. No React, no network.
import type { UpgradeDecision, UpgradeItem } from "@/lib/api";
import type { I18nKey } from "@/lib/i18n/vi";
import { t } from "@/lib/i18n";
import { comparePackVersions } from "@/lib/packStatus";
import { parsePackFile } from "@/lib/verticalPlan";
import type { Pack } from "@/lib/verticalPlan";

export type UpgradeGroup = "decide" | "safe" | "custom" | "blocked" | "settled";
export type UpgradeTone = "success" | "warn" | "weak";

/** Reading order: what needs a decision first, settled (counted, not listed) last. */
export const GROUP_ORDER: readonly UpgradeGroup[] = ["decide", "safe", "custom", "blocked", "settled"];

const META: Record<string, { group: UpgradeGroup; tone: UpgradeTone }> = {
  conflict: { group: "decide", tone: "warn" },
  conflict_no_base: { group: "decide", tone: "warn" },
  conflict_deleted: { group: "decide", tone: "warn" },
  removed_in_pack_modified: { group: "decide", tone: "warn" },
  added: { group: "safe", tone: "success" },
  update_safe: { group: "safe", tone: "success" },
  removed_in_pack: { group: "safe", tone: "weak" },
  kept_custom: { group: "custom", tone: "weak" },
  deleted_by_builder: { group: "custom", tone: "weak" },
  protected: { group: "custom", tone: "weak" },
  unsupported: { group: "blocked", tone: "warn" },
  ambiguous_name: { group: "blocked", tone: "warn" },
  draft_pending: { group: "blocked", tone: "warn" },
  undiffable: { group: "blocked", tone: "weak" },
  in_sync: { group: "settled", tone: "success" },
  unchanged: { group: "settled", tone: "weak" },
};
export const STATUSES = Object.keys(META);

export function statusMeta(status: string): { group: UpgradeGroup; tone: UpgradeTone; labelKey: I18nKey } {
  const m = META[status];
  if (!m) return { group: "blocked", tone: "weak", labelKey: "studio.maintain.upgrade.status.other" };
  return { ...m, labelKey: `studio.maintain.upgrade.status.${status}` as I18nKey };
}

export function groupUpgradeItems(items: UpgradeItem[]): { group: UpgradeGroup; items: UpgradeItem[] }[] {
  return GROUP_ORDER.map((group) => ({ group, items: items.filter((i) => statusMeta(i.status).group === group) })).filter((g) => g.items.length > 0);
}

const ACTIONS = new Set(["apply", "keep", "keep_always", "restore", "delete", "merge"]);
/** A protected ("Luôn giữ") item's apply/restore reads as lifting the hold. */
export function actionLabelKey(action: string, status?: string): I18nKey {
  if (!ACTIONS.has(action)) return "studio.maintain.upgrade.action.other";
  if (status === "protected" && (action === "apply" || action === "restore")) {
    return action === "apply" ? "studio.maintain.upgrade.action.applyProtected" : "studio.maintain.upgrade.action.restoreProtected";
  }
  return `studio.maintain.upgrade.action.${action}` as I18nKey;
}

const OUTCOMES = new Set(["applied", "adopted", "kept", "protected", "restored", "deleted", "merged", "drafted", "failed", "blocked", "not_run"]);
export function outcomeLabelKey(outcome: string): I18nKey {
  return OUTCOMES.has(outcome) ? (`studio.maintain.upgrade.outcome.${outcome}` as I18nKey) : "studio.maintain.upgrade.outcome.other";
}

const MODES = new Set(["draft", "immediate_traced", "immediate_untraced", "immediate_hidden"]);
export function applyModeLabelKey(mode: string | undefined): I18nKey | null {
  return mode && MODES.has(mode) ? (`studio.maintain.upgrade.applyMode.${mode}` as I18nKey) : null;
}

const ITEM_TYPES = new Set(["entity", "field", "deferred_field", "layout", "role", "role_grant", "field_acl", "nn_relationship", "workflow", "report", "dashboard", "team", "work_queue", "approval", "inbound_hook"]);
/** Unknown type: the raw code (a type we never heard of is still better named than hidden). */
export function itemTypeLabelKey(type: string): I18nKey | null {
  return ITEM_TYPES.has(type) ? (`studio.maintain.upgrade.itemType.${type}` as I18nKey) : null;
}

const REASONS = new Set(["ADAPTER_NOT_READY", "FIELD_TYPE_CHANGE", "FIELD_UNIQUE_CHANGE", "NN_UPDATE_UNSUPPORTED", "ROLE_WILDCARD_REFUSED", "HARD_DELETE_KIND", "EXPORT_UNDIFFABLE", "AMBIGUOUS_NAME", "DRAFT_PENDING", "BASE_EXPECTED_ONLY", "ROLE_NAME_RESERVED"]);
/** Unknown reason: null, print nothing beyond the status label. */
export function reasonLabelKey(reason: string | undefined): I18nKey | null {
  return reason && REASONS.has(reason) ? (`studio.maintain.upgrade.reason.${reason}` as I18nKey) : null;
}

const WARNINGS = new Set(["GRANT_NARROWING", "FIELD_ACL_OPENING", "REQUIRED_ON_EXISTING_DATA", "NEW_RELATION_ON_EXISTING_TABLE", "NEW_HOOK_SECRET"]);
/** Unknown warning: null, the caller prints the server's own `text`. */
export function warningLabelKey(code: string): I18nKey | null {
  return WARNINGS.has(code) ? (`studio.maintain.upgrade.warn.${code}` as I18nKey) : null;
}

const ERRORS = new Set(["PLAN_STALE", "IN_PROGRESS", "NOTHING_TO_APPLY", "SLUG_MISMATCH", "DOWNGRADE", "NO_SLUG", "UNKNOWN_FIELD", "DECISION_INVALID", "OPEN_WORKSPACE", "IMMEDIATE_UNCONFIRMED"]);
export function upgradeErrorKey(code: string): I18nKey | null {
  const short = code.startsWith("PACK_UPGRADE_") ? code.slice("PACK_UPGRADE_".length) : "";
  return ERRORS.has(short) ? (`studio.maintain.err.${code}` as I18nKey) : null;
}

/** Actions to draw: merge only when the server offers it AND the two sides touch no common path. */
export function offeredActions(i: UpgradeItem): string[] {
  return i.actions.filter((a) => a !== "merge" || i.overlap.length === 0);
}

/** Which paths a merge takes from the pack and which stay as the builder's; null when no merge is on offer. */
export function mergePreview(i: UpgradeItem): { fromPack: string[]; mine: string[] } | null {
  if (i.merged === undefined || !offeredActions(i).includes("merge")) return null;
  return { fromPack: i.theirsTakenPaths ?? i.theirsPaths, mine: i.oursPaths };
}

export const itemKeyOf = (i: Pick<UpgradeItem, "itemType" | "itemKey">) => `${i.itemType}/${i.itemKey}`;

/** keep_always only writes a flag on the baseline, but it is a change the builder confirms like the others. */
const WRITE_ACTIONS = new Set(["apply", "restore", "delete", "keep_always", "merge"]);
type Choices = Record<string, string>;
export const chosenAction = (i: UpgradeItem, choices: Choices) => choices[itemKeyOf(i)] ?? i.defaultAction;

export function countWrites(items: UpgradeItem[], choices: Choices): { writes: number; adoptable: number } {
  return {
    writes: items.filter((i) => WRITE_ACTIONS.has(chosenAction(i, choices))).length,
    adoptable: items.filter((i) => i.adopt).length,
  };
}

/** Explicit decision for every item that offers an action, so what the builder saw is what runs. */
export function decisionsFrom(items: UpgradeItem[], choices: Choices): UpgradeDecision[] {
  return items.filter((i) => i.actions.length > 0).map((i) => ({ itemType: i.itemType, itemKey: i.itemKey, action: chosenAction(i, choices) }));
}

/** A write that reaches a delivered customer at once, with no trace, needs the builder's tick (069 L7). */
export function needsImmediateTick(items: UpgradeItem[], choices: Choices, delivered: boolean): boolean {
  return delivered && items.some((i) => {
    const a = chosenAction(i, choices);
    return WRITE_ACTIONS.has(a) && a !== "keep_always" && i.applyMode === "immediate_untraced";
  });
}

/** "Nâng cấp bằng tệp": checks a pack file in the browser, before any request (the file never enters a URL). */
export function checkUpgradeFile(text: string, slug: string, installedVersion: string): { ok: true; pack: Pack } | { ok: false; error: string } {
  const parsed = parsePackFile(text);
  if (!parsed.ok) return { ok: false, error: parsed.error };
  const { slug: fileSlug, version } = parsed.manifest;
  if (!fileSlug) return { ok: false, error: t("studio.maintain.upgrade.fromFile.noSlug") };
  if (fileSlug !== slug) return { ok: false, error: t("studio.maintain.upgrade.fromFile.otherPack").replace("{file}", fileSlug).replace("{slug}", slug) };
  if (version && comparePackVersions(version, installedVersion) === -1) {
    return { ok: false, error: t("studio.maintain.upgrade.fromFile.older").replace("{file}", version).replace("{cur}", installedVersion) };
  }
  return { ok: true, pack: parsed.pack };
}
