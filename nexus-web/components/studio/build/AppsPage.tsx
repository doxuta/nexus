"use client";

// App và menu (specs/074 FR-052, lát f2-apps-menu): `dung/man-hinh/app`. App list on the left (with "+ App mới" as an
// in-page strip and delete confirmed in the page), the NavEditor of the chosen app on the right. The chosen app is the
// URL's `?app=`, never the shared "active app" lens, so picking one here cannot move another tab's app.
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { adminApps } from "@/lib/api";
import { qk } from "@/lib/query/keys";
import { t } from "@/lib/i18n";
import { useGuardedRouter } from "@/lib/navigation-blocker";
import { studioHref } from "@/lib/studioRoutes";
import { Button } from "@/components/slds/Button";
import { EmptyState } from "@/components/slds/EmptyState";
import { Spinner } from "@/components/slds/Spinner";
import { AppsManager, NavEditor } from "@/components/studio/AppsManager";

export function AppsPage({ slug, app }: { slug: string; app?: string }) {
  const router = useGuardedRouter();
  const qc = useQueryClient();
  const appsQ = useQuery({ queryKey: qk.adminApps(), queryFn: adminApps });

  if (appsQ.isLoading) return <Spinner />;
  if (appsQ.isError) {
    return (
      <EmptyState plain title={t("studio.build.apps.loadFailed")} action={<Button onClick={() => void appsQ.refetch()}>{t("studio.build.apps.retry")}</Button>} />
    );
  }
  const apps = appsQ.data ?? [];
  const chosen = apps.find((a) => a.name === app) ?? null;
  const select = (name: string | null) => router.replace(studioHref(slug, "dung/man-hinh/app", {}, name ? { app: name } : {}));
  const refresh = async () => {
    await qc.invalidateQueries({ queryKey: qk.apps() });
    await qc.invalidateQueries({ queryKey: qk.adminApps() });
  };
  return (
    <div className="slds-grid slds-wrap slds-gutters slds-p-horizontal_medium">
      <div className="slds-col slds-size_1-of-1 slds-large-size_1-of-3">
        <AppsManager app={chosen?.name ?? null} onSelectApp={select} />
      </div>
      <div className="slds-col slds-size_1-of-1 slds-large-size_2-of-3">
        {chosen ? <NavEditor key={chosen.id} app={chosen} onSaved={refresh} /> : apps.length > 0 && <p className="slds-text-color_weak">{t("studio.build.apps.pick")}</p>}
      </div>
    </div>
  );
}
