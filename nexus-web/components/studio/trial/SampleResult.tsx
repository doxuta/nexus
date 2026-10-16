"use client";

// SampleResult — the per-table report of one generate run (specs/077 T094): created, refused, skipped, plus the
// parent tables the server added on top of the choice. Plain dividers list, no card inside the form strip.
import type { SampleGenerateResult } from "@/lib/api";
import { tf } from "@/lib/i18n";
import { skipReason } from "@/lib/sampleLabels";

export function SampleResult({ result, label }: { result: SampleGenerateResult; label: (entity: string) => string }) {
  const failedBy = new Map<string, SampleGenerateResult["failed"]>();
  for (const f of result.failed) failedBy.set(f.entity, [...(failedBy.get(f.entity) ?? []), f]);
  return (
    <ul className="slds-has-dividers_bottom slds-m-top_small">
      {result.addedParents.length > 0 && (
        <li className="slds-item" data-testid="sample-gen-parents">
          {tf("studio.build.trial.generate.parents", { names: result.addedParents.map(label).join(", ") })}
        </li>
      )}
      {result.created.map((c) => (
        <li key={c.entity} className="slds-item" data-testid={`sample-gen-created-${c.entity}`}>
          {tf("studio.build.trial.generate.created", { label: label(c.entity), n: c.count })}
        </li>
      ))}
      {[...failedBy].map(([entity, rows]) => (
        <li key={`f-${entity}`} className="slds-item slds-text-color_error" data-testid={`sample-gen-failed-${entity}`}>
          {tf("studio.build.trial.generate.failed", { label: label(entity), n: rows.length, msg: rows[0].error })}
        </li>
      ))}
      {result.skipped.map((s) => (
        <li key={`s-${s.entity}`} className="slds-item slds-text-color_weak" data-testid={`sample-gen-skipped-${s.entity}`}>
          {tf("studio.build.trial.generate.skipped", { label: label(s.entity), reason: skipReason(s.code) })}
        </li>
      ))}
    </ul>
  );
}
