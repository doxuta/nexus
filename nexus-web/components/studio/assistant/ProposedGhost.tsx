"use client";

// The ghost an editor draws for an assistant proposal (specs/074 lát f2-ai-preview-data-rules, FR-071 f).
// Purple `.nx-proposed` + the WORD "Đề xuất" (colour is never the only cue), an aria-label naming the proposal, and no
// control inside (the ghost is not editable; Áp dụng / Bỏ stay on the assistant's card). GhostLive says one polite
// sentence when a ghost appears on or leaves the page.
import clsx from "clsx";
import { useState, type ReactNode } from "react";
import { StatusText } from "@/components/studio/StatusText";
import type { ProposedChange } from "@/components/studio/assistant/ProposalOverlay";
import { t, tf } from "@/lib/i18n";

/** Short text of a change for the label: the server's own `label` (already plain Vietnamese), plus the menu it lands in. */
export function ghostText(c: ProposedChange): string {
  return c.appName ? `${c.label} (${tf("studio.build.ai.intoMenu", { app: c.appName })})` : c.label;
}

export function ProposedGhost({ change, children, className }: { change: ProposedChange; children?: ReactNode; className?: string }) {
  return (
    <span
      role="group"
      data-proposal-key={change.proposalKey}
      aria-label={tf("studio.build.ai.label", { text: ghostText(change) })}
      className={clsx("nx-proposed slds-p-horizontal_x-small", className)}
    >
      <StatusText tone="proposed">{t("studio.build.ai.tag")}</StatusText>
      <span className="slds-m-left_x-small">
        {change.op === "delete" ? `${t("studio.build.ai.willDelete")} · ` : null}
        {ghostText(change)}
      </span>
      {children}
    </span>
  );
}

export function GhostLive({ change }: { change: ProposedChange | null }) {
  const key = change?.proposalKey ?? null;
  const [seen, setSeen] = useState<{ key: string | null; msg: string }>({ key: null, msg: "" });
  if (seen.key !== key) {
    setSeen({ key, msg: change ? tf("studio.build.ai.appeared", { text: ghostText(change) }) : t("studio.build.ai.gone") });
  }
  return <span className="slds-assistive-text" aria-live="polite">{seen.msg}</span>;
}
