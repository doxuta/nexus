"use client";

// "Lịch sử cài ({n})" (specs/079 FR-046): the whole append-only install ledger, newest first, folded. Takes the rows
// the page already read (one listPackInstalls call). Replaces the "Lịch sử" tab of the old VerticalLibrary.
import { fill } from "@/components/slds/DataTable";
import { formatDate } from "@/lib/format";
import { t } from "@/lib/i18n";
import { sortInstallsNewestFirst, type InstallRecord } from "@/lib/verticalPlan";

function counts(r: InstallRecord): string {
  const n = Array.isArray(r.result?.entitiesCreated) ? r.result.entitiesCreated.length : undefined;
  const m = typeof r.result?.samplesInserted === "number" ? r.result.samplesInserted : undefined;
  return [n !== undefined && fill(t("studio.maintain.nangCap.history.tables"), { n }), m !== undefined && fill(t("studio.maintain.nangCap.history.samples"), { m })]
    .filter(Boolean).join(" · ");
}

export function PackInstallHistory({ installs }: { installs: InstallRecord[] }) {
  if (installs.length === 0) return null;
  return (
    <details className="slds-m-top_large">
      <summary className="cursor-pointer font-semibold">{fill(t("studio.maintain.nangCap.history.title"), { n: installs.length })}</summary>
      <ul className="slds-has-dividers_bottom slds-m-top_x-small">
        {sortInstallsNewestFirst(installs).map((r) => {
          const c = counts(r);
          return (
            <li key={r.id} className="slds-item" data-testid="install-history-row">
              <span className="font-semibold">{r.slug || t("studio.maintain.pack.fromFile")}</span>{" "}
              <span className="tabular-nums">v{r.version}</span>{" "}
              <span className="slds-text-color_weak">· {formatDate(r.installedAt)}{c && ` · ${c}`}</span>
            </li>
          );
        })}
      </ul>
    </details>
  );
}
