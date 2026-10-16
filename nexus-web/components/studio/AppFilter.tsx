"use client";

// AppFilter (specs/074 FR-003/FR-010): the app filter of the Studio build pages. The app is a `?app=` value in the URL
// (never the shared "active app" lens), so a filtered page can be linked, reloaded and shared. Shown only when the
// project has more than one app — with one app there is nothing to choose. The line "n <noun> của app khác đang ẩn ·
// Xem tất cả" is a text link: the filter is soft, the other apps' rows are one click away.
import Link from "next/link";
import type { AppSummary } from "@/lib/api";
import { useGuardedRouter } from "@/lib/navigation-blocker";
import { t, tf } from "@/lib/i18n";
import { studioHref, type RouteId } from "@/lib/studioRoutes";
import { Select } from "@/components/slds/Select";

export function AppFilter({
  slug, route, apps, value, hiddenCount, hiddenNoun, query,
}: {
  slug: string;
  /** The page this filter belongs to; `?app=` is written onto its own href. */
  route: RouteId;
  apps: AppSummary[];
  /** App code from `?app=`, null = all apps. */
  value: string | null;
  hiddenCount: number;
  /** "bảng" / "luật" / "báo cáo": the thing that is hidden. */
  hiddenNoun: string;
  /** Other query keys of the page that must survive a change of app (e.g. `q`). */
  query?: Record<string, string | undefined>;
}) {
  const router = useGuardedRouter();
  if (apps.length <= 1) return null;
  const href = (app: string | undefined) => studioHref(slug, route, {}, { ...query, app });
  return (
    <div className="slds-grid slds-grid_vertical-align-center slds-wrap">
      <div className="slds-m-right_small">
        <Select
          label={t("studio.build.data.filterLabel")}
          labelHidden
          placeholder={false}
          value={value ?? ""}
          onChange={(e) => router.replace(href(e.target.value || undefined))}
          options={[{ value: "", label: t("studio.build.data.filterAll") }, ...apps.map((a) => ({ value: a.name, label: a.label }))]}
        />
      </div>
      {value && hiddenCount > 0 && (
        <p className="slds-text-body_small slds-text-color_weak">
          {tf("studio.build.data.hidden", { n: hiddenCount, noun: hiddenNoun })}
          {" · "}
          <Link href={href(undefined)}>{t("studio.build.data.showAll")}</Link>
        </p>
      )}
    </div>
  );
}
