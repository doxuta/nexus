// ChangeList — "+ ~ − =" list of changes (specs/073 UI-ARCH 5.3). The mark is decoration (aria-hidden); the word beside it is
// for screen readers. Used by PublishReview and, with tone="proposed", by assistant proposals. A long line wraps (a rule's
// condition, a grant, a path): cut to one line it hid the part the builder had to read (issue #23).
import clsx from "clsx";
import type { ReactNode } from "react";
import { t } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";

export type ChangeOp = "add" | "change" | "remove" | "same";
export interface ChangeItem { op: ChangeOp; text: ReactNode; key?: string }

const MARK: Record<ChangeOp, { glyph: string; word: I18nKey }> = {
  add: { glyph: "+", word: "studio.publish.mark.add" },
  change: { glyph: "~", word: "studio.publish.mark.change" },
  remove: { glyph: "−", word: "studio.publish.mark.remove" },
  same: { glyph: "=", word: "studio.publish.mark.same" },
};

export function ChangeList({ items, tone = "neutral", label }: { items: readonly ChangeItem[]; tone?: "neutral" | "proposed"; label?: string }) {
  return (
    <ul aria-label={label ?? t("studio.publish.list")} className={clsx("slds-list_vertical slds-has-dividers_bottom-space", tone === "proposed" && "nx-proposed")}>
      {items.map((it, i) => (
        <li key={it.key ?? i} className="slds-item slds-grid slds-grid_vertical-align-start">
          <span data-mark aria-hidden="true" className="slds-m-right_x-small tabular-nums">{MARK[it.op].glyph}</span>
          <span className="slds-assistive-text">{t(MARK[it.op].word)}</span>
          <span className="slds-col slds-hyphenate min-w-0">{it.text}</span>
        </li>
      ))}
    </ul>
  );
}
