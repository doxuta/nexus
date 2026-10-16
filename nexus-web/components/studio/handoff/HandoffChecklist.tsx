"use client";

// "Giao cho khách" checklist (specs/077 f5-handoff-checklist): one SLDS divided list, each row an icon AND one
// sentence AND at most one button. Blocking rows use Circle, reminders Info. No card per row, no progress ring.
import { useState } from "react";
import { Circle, CircleCheck, Info } from "lucide-react";
import Link from "next/link";
import { Button, buttonClassName } from "@/components/slds/Button";
import { Checkbox } from "@/components/slds/Checkbox";
import { Icon } from "@/components/slds/Icon";
import type { BuildStepKey } from "@/lib/buildSteps";
import type { RoleSummary, SampleSummary, SampleWipeKept, SampleWipeResult } from "@/lib/api";
import { blockingCount, type HandoffAction, type HandoffRow } from "@/lib/handoffChecklist";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { studioHref } from "@/lib/studioRoutes";
import { SampleKeptList } from "../trial/SampleKeptList";
import { SampleWipeConfirm } from "../trial/SampleWipeConfirm";
import { OwnerAccountRow } from "./OwnerAccountRow";

/** Sample-data wipe wiring of the "samples" row. `kept` = what the last wipe left, held in page memory by the caller. */
export interface HandoffSamplesProps {
  summary: SampleSummary;
  entityNames: string[];
  label: (entity: string) => string;
  kept: SampleWipeKept[];
  keptAck: boolean;
  onKeptAck: (v: boolean) => void;
  onWiped: (r: SampleWipeResult) => void;
}

const stepName = (k: string) => t(`studio.step.${k as BuildStepKey}.label` as I18nKey);

function sentence(row: HandoffRow): string {
  const p = { ...row.msg.params } as Record<string, string | number>;
  // The steps row carries step keys; the screen shows their names.
  if (typeof p.steps === "string") p.steps = p.steps.split(",").map(stepName).join(", ");
  return tf(row.msg.key, p);
}

function RowAction({ slug, action, subject, onRetry, onWipe }: { slug: string; action: HandoffAction; subject?: string; onRetry: (source: string) => void; onWipe: () => void }) {
  if (action.kind === "wipe") return <Button variant="text-destructive" onClick={onWipe}>{t(action.labelKey)}</Button>;
  if (action.kind === "retry") {
    const label = t(action.labelKey);
    return (
      <Button variant="neutral" aria-label={subject ? `${label} ${subject}` : undefined} onClick={() => onRetry(action.source)}>
        {label}
      </Button>
    );
  }
  const label = tf(action.labelKey, action.labelParams?.step ? { step: stepName(action.labelParams.step) } : {});
  return (
    <Link
      className={buttonClassName("neutral")}
      aria-label={subject ? `${label} ${subject}` : undefined}
      href={studioHref(slug, action.routeId, action.params ?? {}, action.query)}
    >
      {label}
    </Link>
  );
}

export function HandoffChecklist({
  slug,
  rows,
  roles = [],
  onRetry,
  onOwnerChanged,
  samples,
}: {
  slug: string;
  rows: HandoffRow[];
  roles?: RoleSummary[];
  onRetry: (source: string) => void;
  onOwnerChanged?: () => void;
  samples?: HandoffSamplesProps;
}) {
  const [wiping, setWiping] = useState(false);
  const n = blockingCount(rows);
  return (
    <div>
      {n > 0 && <p className="slds-text-body_small slds-text-color_weak slds-m-bottom_x-small">{tf("studio.build.handover.blockedCount", { n })}</p>}
      <ul className="slds-has-dividers_bottom">
        {rows.map((r) => {
          const subject = r.id.startsWith("app:") ? String(r.msg.params?.app ?? "") : undefined;
          return (
            <li key={r.id} className="slds-item" data-row={r.id}>
              {r.owner ? (
                <OwnerAccountRow owner={r.owner} roles={roles} onChanged={() => onOwnerChanged?.()} />
              ) : (
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                <Icon
                  icon={r.tone === "ok" ? CircleCheck : r.blocking ? Circle : Info}
                  size="x-small"
                  tone={r.tone === "ok" ? "success" : r.blocking ? "warning" : "default"}
                  label={r.tone === "ok" ? t("page.appOverview.readiness.pass") : undefined}
                />
                <span className="flex-1" style={{ minWidth: "12rem" }}>
                  {sentence(r)}
                  {r.note && <span className="slds-text-color_weak"> {t(r.note.key)}</span>}
                </span>
                {r.action && <RowAction slug={slug} action={r.action} subject={subject} onRetry={onRetry} onWipe={() => setWiping(true)} />}
              </div>
              )}
              {r.id === "samples" && samples && (
                <>
                  {r.keptHeld && (
                    <div className="slds-m-top_x-small">
                      <Checkbox label={t("studio.build.handover.row.samples.keptAck")} checked={samples.keptAck} onChange={(e) => samples.onKeptAck(e.target.checked)} />
                    </div>
                  )}
                  {r.tone === "todo" && <SampleKeptList kept={samples.kept} label={samples.label} />}
                  {wiping && r.action?.kind === "wipe" && (
                    <div className="slds-m-top_x-small">
                      <SampleWipeConfirm
                        summary={samples.summary}
                        entityNames={samples.entityNames}
                        onCancel={() => setWiping(false)}
                        onDone={(res) => {
                          setWiping(false);
                          samples.onWiped(res);
                        }}
                      />
                    </div>
                  )}
                </>
              )}
            </li>
          );
        })}
      </ul>
    </div>
  );
}
