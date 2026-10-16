"use client";

// bao-tri/nang-cap (specs/079 f7-nang-cap-hien-co): the honest platform line, the installed packs against the version
// shipped with Nexus, and the folded install history. There is no per-project platform upgrade (Nexus updates every
// project at once); the per-pack upgrade is bao-tri/nang-cap/:id (UpgradePage).
import { Info } from "lucide-react";
import { Icon } from "@/components/slds/Icon";
import { listBuiltinVerticals, listPackInstalls } from "@/lib/api";
import { t } from "@/lib/i18n";
import { QueryState } from "../QueryState";
import { useProjectQuery } from "../useProjectQuery";
import { PackExportForm } from "./PackExportForm";
import { PackInstallHistory } from "./PackInstallHistory";
import { PackStatusList } from "./PackStatusList";

export function NangCapPage({ slug: project }: { slug: string }) {
  const installs = useProjectQuery(project, ["installs"], listPackInstalls);
  // A pack can be compared only when Nexus ships a version of it.
  const builtins = useProjectQuery(project, ["builtins"], listBuiltinVerticals);
  return (
    <div className="slds-p-around_medium">
      <p className="nx-lede flex items-start gap-2">
        <Icon icon={Info} size="x-small" />
        <span>{t("studio.maintain.upgrade.platform")}</span>
      </p>
      <h2 className="slds-text-heading_medium slds-m-top_large">{t("studio.maintain.upgrade.packs")}</h2>
      <QueryState query={installs} empty={<p className="slds-text-color_weak slds-m-top_x-small">{t("studio.maintain.upgrade.none")}</p>}>
        <PackStatusList project={project} installs={installs.data ?? []} builtins={builtins.data ?? []} />
        <PackInstallHistory installs={installs.data ?? []} />
      </QueryState>
      <PackExportForm project={project} />
    </div>
  );
}
