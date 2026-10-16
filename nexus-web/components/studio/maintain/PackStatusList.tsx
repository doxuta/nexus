"use client";

// "Gói mẫu đã cài" (specs/079 f7-nang-cap-hien-co): one row per pack, newest install only — version in use, install
// date, a status word against the version shipped with Nexus, and the row's actions. "Đối chiếu" unfolds the diff
// right under its row; "Xem nâng cấp" is drawn only when Nexus ships a version of that pack to upgrade to.
import Link from "next/link";
import { useState } from "react";
import { Button } from "@/components/slds/Button";
import { fill } from "@/components/slds/DataTable";
import type { BuiltinVertical } from "@/lib/api";
import { formatDate } from "@/lib/format";
import { t } from "@/lib/i18n";
import { latestInstallBySlug, packStatus, type PackStatus } from "@/lib/packStatus";
import { studioHref } from "@/lib/studioRoutes";
import type { InstallRecord } from "@/lib/verticalPlan";
import { PackFileUpgrade } from "./PackFileUpgrade";
import { PackDiffPanel } from "../PackDiffPanel";
import { StatusText, type StatusTone } from "../StatusText";

function statusWord(s: PackStatus): { tone: StatusTone; text: string } | null {
  switch (s.kind) {
    case "newer": return { tone: "warn", text: fill(t("studio.maintain.pack.newer"), { v: s.available }) };
    case "same": return { tone: "success", text: t("studio.maintain.pack.same") };
    case "local-newer": return { tone: "weak", text: t("studio.maintain.pack.localNewer") };
    case "from-file": return { tone: "weak", text: t("studio.maintain.pack.fromFile") };
    case "not-available": return { tone: "weak", text: t("studio.maintain.pack.notAvailable") };
    default: return null;
  }
}

export function PackStatusList({ project, installs, builtins }: { project: string; installs: InstallRecord[]; builtins: BuiltinVertical[] }) {
  const [open, setOpen] = useState<string | null>(null);
  const [fileFor, setFileFor] = useState<string | null>(null);
  return (
    <ul className="slds-has-dividers_bottom slds-m-top_x-small">
      {latestInstallBySlug(installs).map((r) => {
        const shipped = builtins.find((b) => b.slug === r.slug);
        const st = statusWord(packStatus(r, builtins));
        const shown = open === r.slug;
        return (
          <li key={r.slug} className="slds-item">
            <div className="flex flex-wrap items-center gap-3">
              <div className="min-w-[12rem] flex-1">
                <p className="font-semibold">{shipped?.manifest?.name || r.slug}</p>
                <p className="slds-text-body_small slds-text-color_weak">
                  {fill(t("studio.maintain.pack.using"), { v: r.version || "?", date: formatDate(r.installedAt) })}
                </p>
              </div>
              {st && <StatusText tone={st.tone}>{st.text}</StatusText>}
              {shipped && st?.tone === "warn" && (
                <Link className="slds-button slds-button_brand" href={studioHref(project, "bao-tri/nang-cap/:id", { id: r.slug })}
                  aria-label={`${t("studio.maintain.upgrade.open")}: ${r.slug}`}>
                  {t("studio.maintain.upgrade.open")}
                </Link>
              )}
              <Button variant="neutral" aria-expanded={fileFor === r.slug} onClick={() => setFileFor(fileFor === r.slug ? null : r.slug)}
                aria-label={`${t("studio.maintain.upgrade.fromFile")}: ${r.slug}`}>
                {t(fileFor === r.slug ? "studio.maintain.upgrade.fromFile.hide" : "studio.maintain.upgrade.fromFile")}
              </Button>
              <Button variant="neutral" aria-expanded={shown} onClick={() => setOpen(shown ? null : r.slug)}>
                {t(shown ? "studio.maintain.upgrade.hide" : "studio.maintain.upgrade.diff")}
              </Button>
            </div>
            {fileFor === r.slug && <PackFileUpgrade project={project} slug={r.slug} installedVersion={r.version || ""} />}
            {shown && (
              <div className="slds-m-top_small">
                <PackDiffPanel slug={r.slug} />
              </div>
            )}
          </li>
        );
      })}
    </ul>
  );
}
