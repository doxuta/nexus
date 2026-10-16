"use client";

// The health-check findings as a checklist (specs/079 f7-kham-ve-lai, demo A `.checklist`): one ruled row per kind of
// drift: severity glyph, a sentence in the builder's words, the count, and ONE button at the end of the row ("Sửa", or
// the words "sửa tay" when no automatic fix exists). The detail ("Xem {n} khoản") hangs under the sentence.
import { CircleCheck, CircleX, Info, TriangleAlert, type LucideIcon } from "lucide-react";
import { useEffect, useId, useRef, useState } from "react";
import { Button } from "@/components/slds/Button";
import { Icon } from "@/components/slds/Icon";
import type { DoctorFinding, DoctorSeverity } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import { InlineConfirm } from "../InlineConfirm";

const GLYPH: Record<DoctorSeverity, { icon: LucideIcon; tone: "error" | "warning" | "default" }> = {
  critical: { icon: CircleX, tone: "error" },
  warn: { icon: TriangleAlert, tone: "warning" },
  info: { icon: Info, tone: "default" },
};

export interface DoctorFindingListProps {
  rows: DoctorFinding[];
  /** Name and consequence of a check, from the dictionary. */
  metaFor: (checkId: string) => { label: string; desc: string };
  busy: boolean;
  /** Check being fixed right now. */
  fixing: Set<string> | null;
  /** Row waiting for an answer, with the question to show; null = none. */
  ask: { checkId: string; message: string; tone: "warning" | "info" } | null;
  onFix: (finding: DoctorFinding) => void;
  onConfirm: () => void;
  onCancel: () => void;
}

export function DoctorFindingList({ rows, metaFor, busy, fixing, ask, onFix, onConfirm, onCancel }: DoctorFindingListProps) {
  const [open, setOpen] = useState<Set<string>>(new Set());
  const buttons = useRef(new Map<string, HTMLButtonElement | null>());
  // After "Huỷ"/Esc the focus goes back to the "Sửa" button that asked.
  const back = useRef<string | null>(null);
  const prefix = useId();
  useEffect(() => {
    if (ask === null && back.current) {
      buttons.current.get(back.current)?.focus();
      back.current = null;
    }
  }, [ask]);

  const toggle = (id: string) =>
    setOpen((prev) => {
      const next = new Set(prev);
      if (!next.delete(id)) next.add(id);
      return next;
    });

  return (
    <ul className="slds-has-dividers_bottom">
      {rows.map((f) => {
        const meta = metaFor(f.checkId);
        const g = GLYPH[f.severity];
        const shown = open.has(f.checkId);
        const detailId = `${prefix}-${f.checkId}`;
        const manualNote = f.reportOnly && !meta.desc.includes(t("studio.maintain.kham.manualPrefix"));
        return (
          <li key={f.checkId} className="slds-item">
            <div className="flex items-start gap-3">
              <Icon icon={g.icon} tone={g.tone} size="small" className="mt-0.5" />
              <div className="min-w-0 flex-1">
                <p className="slds-text-heading_small break-words">{meta.label}</p>
                <p className="slds-text-body_small slds-text-color_weak break-words">
                  {manualNote ? `${t("studio.maintain.kham.manualPrefix")} ` : ""}{meta.desc}
                </p>
                {f.items.length > 0 && (
                  <p className="slds-text-body_small slds-m-top_xx-small">
                    <button type="button" className="slds-button" aria-expanded={shown} aria-controls={detailId} onClick={() => toggle(f.checkId)}>
                      {tf(shown ? "studio.maintain.kham.hide" : "studio.maintain.kham.view", { n: f.count })}
                    </button>
                  </p>
                )}
              </div>
              <span className="slds-text-body_regular tabular-nums whitespace-nowrap">
                {tf(f.truncated ? "page.doctor.countTruncated" : "page.doctor.count", { n: f.count })}
              </span>
              {f.reportOnly ? (
                <span className="slds-text-body_small slds-text-color_weak whitespace-nowrap">{t("studio.maintain.kham.manual")}</span>
              ) : (
                <Button
                  ref={(el) => { buttons.current.set(f.checkId, el); }}
                  variant="neutral"
                  disabled={busy}
                  aria-label={tf("page.doctor.fixLabel", { label: meta.label })}
                  onClick={() => onFix(f)}
                >
                  {fixing?.has(f.checkId) ? t("page.doctor.fixing") : t("page.doctor.fix")}
                </Button>
              )}
            </div>
            {ask?.checkId === f.checkId && (
              <div className="slds-m-top_small">
                <InlineConfirm
                  tone={ask.tone}
                  message={ask.message}
                  confirmLabel={t("studio.maintain.kham.fixOk")}
                  onConfirm={onConfirm}
                  onCancel={() => { back.current = f.checkId; onCancel(); }}
                  busy={busy}
                />
              </div>
            )}
            {shown && (
              <div id={detailId} className="slds-m-top_small slds-scrollable_x">
                <table className="slds-table slds-table_cell-buffer slds-table_bordered slds-table_fixed-layout" aria-label={tf("page.doctor.items.table", { label: meta.label })}>
                  <thead>
                    <tr className="slds-line-height_reset">
                      <th scope="col" className="w-1/3"><div className="slds-truncate">{t("page.doctor.items.colKey")}</div></th>
                      <th scope="col"><div className="slds-truncate">{t("page.doctor.items.colDetail")}</div></th>
                    </tr>
                  </thead>
                  <tbody>
                    {f.items.map((it, i) => (
                      <tr key={i} className="slds-hint-parent">
                        <th scope="row" className="slds-cell-wrap">{it.key}</th>
                        <td className="slds-cell-wrap">{it.detail}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
                {f.truncated && <p className="slds-text-body_small slds-text-color_weak slds-m-top_x-small">{tf("page.doctor.items.truncated", { n: f.count })}</p>}
              </div>
            )}
          </li>
        );
      })}
    </ul>
  );
}

/** The one line shown instead of a list when every check came back clean. */
export function DoctorCleanRow({ text }: { text: string }) {
  return (
    <div className="flex items-center gap-2">
      <Icon icon={CircleCheck} tone="success" size="small" />
      <span>{text}</span>
    </div>
  );
}
