"use client";

// One ghost on the screen editor stage (specs/075 US9): purple `.nx-proposed`, the WORD "Đề xuất" (colour is never the only
// cue), an aria-label naming it, and NO control inside — Áp dụng / Bỏ stay on the assistant's card.
import { StatusText } from "@/components/studio/StatusText";
import type { ProposedChange } from "@/components/studio/assistant/ProposalOverlay";
import { t, tf } from "@/lib/i18n";

export function ScreenGhost({ change, label, before, willRemove }: { change: ProposedChange; label: string; before?: string; willRemove?: boolean }) {
  const text = willRemove ? `${t("studio.build.screens.proposed.willRemove")}: ${label}` : label;
  return (
    <span role="group" data-proposal-key={change.proposalKey} aria-label={tf("studio.build.ai.label", { text })} className="nx-proposed slds-p-horizontal_x-small slds-grid slds-wrap">
      <StatusText tone="proposed">{t("studio.build.ai.tag")}</StatusText>
      <span className="slds-m-left_x-small">{text}</span>
      {before && <span className="slds-m-left_x-small slds-text-color_weak">{tf("studio.build.ai.before", { text: before })}</span>}
    </span>
  );
}
