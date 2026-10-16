"use client";

// Sample rows the wipe left alone, each with the reason and a same-tab link to open it (specs/077 f5-sample-wipe).
import Link from "next/link";
import type { SampleWipeKept } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import { keptReason, recordHref, shortRecord } from "@/lib/sampleLabels";

export function SampleKeptList({ kept, label }: { kept: SampleWipeKept[]; label: (entity: string) => string }) {
  if (kept.length === 0) return null;
  return (
    <div className="slds-m-top_small">
      <h3 className="slds-text-heading_small">{tf("studio.build.trial.wipe.kept.title", { k: kept.length })}</h3>
      <ul className="slds-has-dividers_bottom">
        {kept.map((k) => (
          <li key={`${k.entity}/${k.recordId}`} className="slds-item" data-testid={`sample-kept-${k.recordId}`}>
            <div className="slds-grid slds-grid_align-spread">
              <span>
                {shortRecord(label(k.entity), k.recordId)} <span className="slds-text-color_weak">{keptReason(k, label)}</span>
              </span>
              <Link href={recordHref(k.entity, k.recordId)} aria-label={`${t("studio.build.trial.wipe.openRecord")} ${shortRecord(label(k.entity), k.recordId)}`}>
                {t("studio.build.trial.wipe.openRecord")}
              </Link>
            </div>
            {k.reason === "referenced_by_real" &&
              k.blockedBy.map((b) => (
                <div key={`${b.entity}.${b.field}`} className="slds-text-body_small slds-text-color_weak">
                  {tf("studio.build.trial.wipe.kept.blockedBy", { what: `${label(b.entity)} › ${b.field}`, n: b.count })}
                </div>
              ))}
          </li>
        ))}
      </ul>
    </div>
  );
}
