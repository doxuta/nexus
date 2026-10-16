"use client";

// One quiet line per installed pack that has a newer shipped version, on the health-check page (specs/079 FR-021a).
// Same query keys as NangCapPage, so the two pages share one read. No second button: the only action is the link in
// the sentence, and nothing is drawn when no pack is behind (or the reads are not done / failed).
import { Info } from "lucide-react";
import Link from "next/link";
import { Icon } from "@/components/slds/Icon";
import { fill } from "@/components/slds/DataTable";
import { listBuiltinVerticals, listPackInstalls } from "@/lib/api";
import { t } from "@/lib/i18n";
import { latestInstallBySlug, packStatus } from "@/lib/packStatus";
import { isMounted, studioHref } from "@/lib/studioRoutes";
import { useProjectQuery } from "../useProjectQuery";

export function PackUpgradeNotice({ slug }: { slug: string }) {
  const installs = useProjectQuery(slug, ["installs"], listPackInstalls);
  const builtins = useProjectQuery(slug, ["builtins"], listBuiltinVerticals);
  if (!installs.data || !builtins.data) return null;
  const behind = latestInstallBySlug(installs.data).flatMap((r) => {
    const s = packStatus(r, builtins.data);
    return s.kind === "newer" ? [{ name: builtins.data.find((b) => b.slug === r.slug)?.manifest?.name || r.slug, from: s.installed, to: s.available }] : [];
  });
  if (behind.length === 0) return null;
  return (
    <ul className="slds-m-top_medium">
      {behind.map((b) => (
        <li key={b.name} className="flex items-start gap-2">
          <Icon icon={Info} size="x-small" />
          <span>
            {fill(t("studio.maintain.kham.packRow"), { name: b.name, to: b.to, from: b.from })}{" "}
            {isMounted("bao-tri/nang-cap") && <Link href={studioHref(slug, "bao-tri/nang-cap")}>{t("studio.maintain.upgrade.open")}</Link>}
          </span>
        </li>
      ))}
    </ul>
  );
}
