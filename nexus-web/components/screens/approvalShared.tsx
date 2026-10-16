// Shared by /approvals and /approvals/[id] (SLDS, specs/038 T031): approval tones → SLDS badge
// themes and timeline tiles, and an approval's history → activity-timeline items. Same mapping the
// record's "Duyệt" tab imports (components/layout/detail/ApprovalsTab), so the three surfaces agree.
import { Check, CircleDot, Send, TriangleAlert, X, type LucideIcon } from "lucide-react";
import type { ActivityTimelineItem } from "@/components/slds/ActivityTimeline";
import { historyActionMeta, type ApprovalHistoryItem } from "@/lib/approvalDetail";
import { formatDateTime } from "@/lib/format";
import { t, tf } from "@/lib/i18n";

export const BADGE: Record<string, "success" | "warning" | "error" | undefined> = { ok: "success", wn: "warning", dg: "error" };

export const TILE: Record<string, { icon: LucideIcon; name: string }> = {
  ok: { icon: Check, name: "approval" },
  dg: { icon: X, name: "record-delete" },
  wn: { icon: TriangleAlert, name: "change-request" },
  ac: { icon: Send, name: "submit-for-approval" },
  // Grey: a recall or any other neutral end is neither approved (-record is the same green as
  // -approval) nor rejected. "default" is also ActivityTimeline's own fallback tile.
  neu: { icon: CircleDot, name: "default" },
};

/**
 * One timeline item per history entry: "Đã duyệt — bước 1 · Trưởng phòng", by whom, to whom (a
 * delegation), the comment as the item's details. `stepName(i, recorded)` returns " · <name>" or "".
 */
export function historyTimeline(
  history: ApprovalHistoryItem[],
  personName: (id?: string) => string,
  stepName: (step: number, recorded?: string) => string,
  idPrefix: string,
): ActivityTimelineItem[] {
  return history.map((h, i) => {
    const m = historyActionMeta(h.action);
    const tile = TILE[m.tone] ?? TILE.neu;
    return {
      id: `${idPrefix}-${i}`,
      icon: tile.icon,
      iconName: tile.name,
      // step is 0-indexed — display 1-based like every other approval surface
      title: tf("page.approvals.historyItem", { action: m.label, step: h.step + 1, name: stepName(h.step, h.stepName) }),
      date: h.at ? formatDateTime(h.at) : "",
      summary: (
        <>
          {tf("page.approvals.by", { name: personName(h.actor) })}
          {h.target && <> · {tf("page.approvals.to", { name: personName(h.target) })}</>}
          {h.via === "override" && <> · {t("page.approvals.viaOverride")}</>}
          {h.partial && <> · {t("page.approvals.partialVote")}</>}
        </>
      ),
      body: h.comment ? { label: t("page.approvals.comment"), text: `“${h.comment}”` } : undefined,
    };
  });
}

/** Ids of the items that carry a comment — opened by default so a reason is read, not hunted for. */
export function withComments(items: ActivityTimelineItem[]): string[] {
  return items.filter((i) => i.body).map((i) => i.id);
}
