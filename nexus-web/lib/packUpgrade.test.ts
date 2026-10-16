// specs/079 f7-nang-cap-* : the closed label table of the upgrade plan (plan.md 5a) and the pure helpers around it.
import { describe, expect, it } from "vitest";
import vi from "@/lib/i18n/vi";
import type { UpgradeItem, UpgradeStatus } from "@/lib/api";
import {
  STATUSES, countWrites, decisionsFrom, groupUpgradeItems, itemKeyOf, needsImmediateTick, statusMeta, upgradeErrorKey,
  actionLabelKey, outcomeLabelKey, applyModeLabelKey, offeredActions, mergePreview,
} from "./packUpgrade";

describe("f7-nang-cap-gop: merge only when the server offers it and the sides do not overlap", () => {
  const mk = (extra: Partial<UpgradeItem>): UpgradeItem => ({
    itemType: "workflow", itemKey: "k", label: "k", status: "conflict" as UpgradeStatus, actions: ["keep"], defaultAction: "keep",
    theirsPaths: [], oursPaths: [], overlap: [], sharedWith: [], warnings: [], ...extra,
  });
  it("offeredActions drops merge from an item whose sides touch the same path", () => {
    expect(offeredActions(mk({ actions: ["keep", "apply", "merge"], overlap: ["conditions.0"] }))).toEqual(["keep", "apply"]);
  });
  it("offeredActions keeps merge only when the server offers it and nothing overlaps", () => {
    expect(offeredActions(mk({ actions: ["keep", "apply", "merge"] }))).toContain("merge");
    expect(offeredActions(mk({ actions: ["keep", "apply"] }))).not.toContain("merge");
  });
  it("mergePreview splits pack-taken paths from the builder's own", () => {
    expect(mergePreview(mk({ actions: ["merge"], merged: { a: 1 }, theirsTakenPaths: ["b"], oursPaths: ["a"] }))).toEqual({ fromPack: ["b"], mine: ["a"] });
    expect(mergePreview(mk({ actions: ["keep"] }))).toBeNull();
  });
  it("merged outcome reads Đã gộp", () => expect(vi[outcomeLabelKey("merged")]).toBe("Đã gộp"));
});

const it_ = (status: string, extra: Partial<UpgradeItem> = {}): UpgradeItem => ({
  itemType: "workflow", itemKey: `k-${status}`, label: status, status: status as UpgradeStatus, actions: ["keep"], defaultAction: "keep",
  theirsPaths: [], oursPaths: [], overlap: [], sharedWith: [], warnings: [], ...extra,
});

describe("statusMeta: 16 real statuses of 069 map to group + label + tone", () => {
  const TABLE: [string, string, string][] = [
    ["conflict", "decide", "warn"], ["conflict_no_base", "decide", "warn"], ["conflict_deleted", "decide", "warn"],
    ["removed_in_pack_modified", "decide", "warn"], ["added", "safe", "success"], ["update_safe", "safe", "success"],
    ["removed_in_pack", "safe", "weak"], ["kept_custom", "custom", "weak"], ["deleted_by_builder", "custom", "weak"],
    ["protected", "custom", "weak"], ["unsupported", "blocked", "warn"], ["ambiguous_name", "blocked", "warn"],
    ["draft_pending", "blocked", "warn"], ["undiffable", "blocked", "weak"], ["in_sync", "settled", "success"],
    ["unchanged", "settled", "weak"],
  ];
  it.each(TABLE)("%s -> %s / %s", (status, group, tone) => {
    const m = statusMeta(status);
    expect(m.group).toBe(group);
    expect(m.tone).toBe(tone);
    expect(vi[m.labelKey]).toBeTruthy();
  });
  it("covers exactly the 16 statuses", () => expect(STATUSES.length).toBe(16));
  it("an unknown status falls to 'blocked' with the neutral label, never the machine code", () => {
    const m = statusMeta("user_modified");
    expect(m.group).toBe("blocked");
    expect(vi[m.labelKey]).toBe("Mục khác");
  });
});

describe("groupUpgradeItems", () => {
  it("returns the groups in reading order, drops empty ones, settled last", () => {
    const g = groupUpgradeItems([it_("unchanged"), it_("added"), it_("conflict"), it_("kept_custom"), it_("unsupported"), it_("weird")]);
    expect(g.map((x) => x.group)).toEqual(["decide", "safe", "custom", "blocked", "settled"]);
    expect(g.find((x) => x.group === "blocked")!.items.map((i) => i.status)).toEqual(["unsupported", "weird"]);
  });
  it("empty plan -> no groups", () => expect(groupUpgradeItems([])).toEqual([]));
});

describe("labels", () => {
  it("every action, outcome, apply mode has a Vietnamese label; unknown -> no machine code", () => {
    for (const a of ["apply", "keep", "keep_always", "restore", "delete", "merge"]) expect(vi[actionLabelKey(a)]).toBeTruthy();
    for (const o of ["applied", "adopted", "kept", "protected", "restored", "deleted", "merged", "drafted", "failed", "blocked", "not_run"]) {
      expect(vi[outcomeLabelKey(o)]).toBeTruthy();
    }
    for (const m of ["draft", "immediate_traced", "immediate_untraced", "immediate_hidden"]) expect(vi[applyModeLabelKey(m)!]).toBeTruthy();
    expect(applyModeLabelKey(undefined)).toBeNull();
    expect(vi[outcomeLabelKey("zzz")]).toBe("Mục khác");
  });
  it("a protected item's 'apply'/'restore' reads as lifting the hold", () => {
    expect(vi[actionLabelKey("apply", "protected")]).toBe("Bỏ giữ, theo bản gói");
    expect(vi[actionLabelKey("apply", "update_safe")]).toBe("Áp bản gói");
  });
});

describe("countWrites / decisionsFrom / needsImmediateTick", () => {
  const items = [
    it_("update_safe", { itemKey: "a", actions: ["apply", "keep"], defaultAction: "apply", applyMode: "immediate_untraced" }),
    it_("conflict", { itemKey: "b", actions: ["keep", "keep_always", "apply"], defaultAction: "keep" }),
    it_("unchanged", { itemKey: "c", actions: [], defaultAction: "none" }),
    it_("in_sync", { itemKey: "d", actions: [], defaultAction: "none", adopt: true }),
  ];
  it("counts the default choice and recounts when the builder changes one; adoptable counted separately", () => {
    expect(countWrites(items, {})).toEqual({ writes: 1, adoptable: 1 });
    expect(countWrites(items, { "workflow/b": "apply", "workflow/a": "keep" })).toEqual({ writes: 1, adoptable: 1 });
  });
  it("decisions are explicit for every item that offers an action, never for the ones that offer none", () => {
    expect(decisionsFrom(items, { "workflow/b": "keep_always" })).toEqual([
      { itemType: "workflow", itemKey: "a", action: "apply" },
      { itemType: "workflow", itemKey: "b", action: "keep_always" },
    ]);
  });
  it("tick needed only when a chosen write is immediate_untraced on a delivered plan", () => {
    expect(needsImmediateTick(items, {}, true)).toBe(true);
    expect(needsImmediateTick(items, {}, false)).toBe(false);
    expect(needsImmediateTick(items, { "workflow/a": "keep_always" }, true)).toBe(false);
  });
  it("itemKeyOf joins type and key", () => expect(itemKeyOf(items[0])).toBe("workflow/a"));
});

describe("upgradeErrorKey", () => {
  it("each known server code has its own sentence, unknown -> null", () => {
    for (const c of ["PACK_UPGRADE_PLAN_STALE", "PACK_UPGRADE_IN_PROGRESS", "PACK_UPGRADE_NOTHING_TO_APPLY", "PACK_UPGRADE_SLUG_MISMATCH",
      "PACK_UPGRADE_DOWNGRADE", "PACK_UPGRADE_NO_SLUG", "PACK_UPGRADE_UNKNOWN_FIELD", "PACK_UPGRADE_DECISION_INVALID",
      "PACK_UPGRADE_OPEN_WORKSPACE", "PACK_UPGRADE_IMMEDIATE_UNCONFIRMED"]) {
      const k = upgradeErrorKey(c);
      expect(k && vi[k], c).toBeTruthy();
    }
    expect(upgradeErrorKey("NOPE")).toBeNull();
  });
});
