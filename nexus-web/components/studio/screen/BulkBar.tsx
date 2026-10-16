"use client";

// BulkBar (specs/075 FR-070, T125): the ONE row above the list-mode table. Nothing ticked: a hint. Ticked: "Đã chọn n" and
// the actions. It is a polite live region, so the outcome sentence (what was done, what was skipped and why) is announced.
// "Đặt bắt buộc" is not offered on the record page (a record page has no required switch).
import { useState } from "react";
import { Button } from "@/components/slds/Button";
import { Select } from "@/components/slds/Select";
import type { BulkSkip } from "@/lib/screenEditorModel";
import { t, tf } from "@/lib/i18n";

const SKIP_KEY = {
  required: "studio.build.screens.bulk.skipRequired",
  hasCondition: "studio.build.screens.bulk.skipHasCondition",
  controls: "studio.build.screens.bulk.skipControls",
  noSection: "studio.build.screens.bulk.skipNoSection",
} as const;

/** One sentence for the live region: the done part, then one clause per skip reason with the field names. */
export function bulkMessage(done: string | null, skipped: BulkSkip[], labelOf: (field: string) => string): string {
  const parts = done ? [done] : [];
  for (const reason of Object.keys(SKIP_KEY) as BulkSkip["reason"][]) {
    const hit = skipped.filter((s) => s.reason === reason);
    if (hit.length > 0) parts.push(tf(SKIP_KEY[reason], { n: hit.length, names: hit.map((s) => labelOf(s.field)).join(", ") }));
  }
  return parts.join(" ");
}

export interface BulkBarProps {
  count: number;
  sections: Array<{ id: string; title: string }>;
  /** Record page: no "Đặt bắt buộc". */
  detail?: boolean;
  disabled?: boolean;
  /** Outcome of the last action. */
  message: string | null;
  onRequired: () => void;
  onRemove: () => void;
  onMove: (sectionId: string) => void;
  onClear: () => void;
}

export function BulkBar({ count, sections, detail, disabled, message, onRequired, onRemove, onMove, onClear }: BulkBarProps) {
  const [target, setTarget] = useState("");
  const sectionId = sections.some((s) => s.id === target) ? target : (sections[0]?.id ?? "");
  const off = disabled || count === 0;
  return (
    <div
      className={`slds-grid slds-grid_vertical-align-center slds-wrap slds-p-around_small${count > 0 ? " slds-theme_shade" : ""}`}
      style={{ minHeight: "3.5rem", gap: "0.5rem 0.75rem" }}
      aria-live="polite"
      data-testid="bulk-bar"
    >
      {count === 0 ? (
        <span className="slds-text-color_weak">{message ?? t("studio.build.screens.bulk.hint")}</span>
      ) : (
        <>
          <strong>{tf("studio.build.screens.bulk.chosen", { n: count })}</strong>
          {!detail && <Button variant="neutral" disabled={off} onClick={onRequired}>{t("studio.build.screens.bulk.required")}</Button>}
          <Button variant="neutral" disabled={off} onClick={onRemove}>{t(detail ? "studio.build.screens.bulk.removeRecord" : "studio.build.screens.bulk.remove")}</Button>
          <Select
            label={t("studio.build.screens.bulk.toSection")}
            value={sectionId}
            placeholder={false}
            disabled={disabled || sections.length === 0}
            onChange={(e) => setTarget(e.target.value)}
          >
            {sections.map((s) => <option key={s.id} value={s.id}>{s.title}</option>)}
          </Select>
          <Button variant="neutral" disabled={off || !sectionId} onClick={() => onMove(sectionId)}>{t("studio.build.screens.bulk.move")}</Button>
          <Button variant="neutral" onClick={onClear}>{t("studio.build.screens.bulk.clear")}</Button>
          {message && <span className="slds-text-color_weak">{message}</span>}
        </>
      )}
    </div>
  );
}
