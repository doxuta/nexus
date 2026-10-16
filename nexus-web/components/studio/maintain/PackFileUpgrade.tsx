"use client";

// "Nâng cấp bằng tệp" (specs/079 f7-nang-cap-giu-tep): the builder picks a pack file; it is parsed and checked in the
// browser (same pack code, not an older version) BEFORE any request, then the same upgrade page runs on it. The file
// stays in component state: never in a URL, never in a query cache key.
import { useState } from "react";
import { FileSelector } from "@/components/slds/FileSelector";
import type { PackSource } from "@/lib/api";
import { t } from "@/lib/i18n";
import { checkUpgradeFile } from "@/lib/packUpgrade";
import { UpgradePage } from "./UpgradePage";

export function PackFileUpgrade({ project, slug, installedVersion }: { project: string; slug: string; installedVersion: string }) {
  const [error, setError] = useState<string | null>(null);
  const [source, setSource] = useState<PackSource | null>(null);

  function onFile(file: File) {
    setError(null);
    setSource(null);
    const reader = new FileReader();
    reader.onload = () => {
      const r = checkUpgradeFile(String(reader.result ?? ""), slug, installedVersion);
      if (r.ok) setSource({ pack: r.pack });
      else setError(r.error);
    };
    reader.onerror = () => setError(t("studio.maintain.upgrade.fromFile.readFailed"));
    reader.readAsText(file);
  }

  return (
    <div className="slds-m-top_small">
      <FileSelector label={t("studio.maintain.upgrade.fromFile.label")} accept="application/json,.json" error={error ?? undefined}
        onFiles={(files) => { if (files[0]) onFile(files[0]); }} />
      <p className="slds-text-body_small slds-text-color_weak">{t("studio.maintain.upgrade.fromFile.hint")}</p>
      {source && <UpgradePage project={project} pack={slug} source={source} />}
    </div>
  );
}
