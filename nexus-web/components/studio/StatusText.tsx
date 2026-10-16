// Studio status label (specs/073 FR-006, UI-ARCH 4.8): coloured TEXT, never a Badge/Pill (D-UI-14).
// Colour lives in the `.nx-status*` family of the Studio glue block in globals.css.
import clsx from "clsx";
import type { ReactNode } from "react";
import type { DraftState } from "@/lib/api";
import { t } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";

export type StatusTone = "success" | "error" | "warn" | "weak" | "proposed";

export function StatusText({ tone, className, children }: { tone: StatusTone; className?: string; children: ReactNode }) {
  return <span className={clsx("nx-status", `nx-status_${tone}`, className)}>{children}</span>;
}

/** States of table 4.8 beyond the plain draft overlay state. */
export type StatusState =
  | DraftState
  | "requiredOnPublish" | "deleteFailed" | "proposed"
  | "inUse" | "complete" | "connecting" | "locked" | "error" | "viewOnly" | "hidden";
/** How a kind of change reaches real users once the project is delivered (063). */
export type StatusKind = "draftable" | "direct" | "untracked";

const STATES: Record<Exclude<StatusState, "live">, { tone: StatusTone; key: I18nKey }> = {
  edited: { tone: "warn", key: "studio.status.draft" },
  new: { tone: "warn", key: "studio.status.new" },
  deleted: { tone: "warn", key: "studio.status.deleted" },
  requiredOnPublish: { tone: "warn", key: "studio.status.requiredOnPublish" },
  deleteFailed: { tone: "error", key: "studio.status.deleteFailed" },
  proposed: { tone: "proposed", key: "studio.status.proposed" },
  inUse: { tone: "success", key: "studio.status.inUse" },
  complete: { tone: "success", key: "studio.status.complete" },
  connecting: { tone: "success", key: "studio.status.connecting" },
  locked: { tone: "error", key: "studio.status.locked" },
  error: { tone: "error", key: "studio.status.error" },
  viewOnly: { tone: "weak", key: "studio.status.viewOnly" },
  hidden: { tone: "warn", key: "studio.status.hidden" },
};

/** Map a draft/permission state to its label + tone; null = show nothing (no draft label before delivery).
 *  A `live` row of a `direct`/`untracked` kind still carries the "applies immediately" warning. */
export function draftStateLabel(state: StatusState | undefined, kind: StatusKind = "draftable"): { tone: StatusTone; label: string } | null {
  if (!state || state === "live") {
    if (kind === "direct") return { tone: "warn", label: t("studio.status.liveDirect") };
    if (kind === "untracked") return { tone: "warn", label: t("studio.status.liveDirectNoHistory") };
    return null;
  }
  const s = STATES[state];
  return { tone: s.tone, label: t(s.key) };
}
