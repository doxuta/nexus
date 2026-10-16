"use client";

// Màn hình (specs/074 FR-051, lát f2-screens-header; specs/075 L1c): `dung/man-hinh` has no list of screens. The head row is
// the app filter and the text button "App và menu"; below it the ScreenBar picks the screen. The table is `…/man-hinh/<bang>`
// or `?bang=`, else the first one in scope; `?man=form|ho-so|danh-sach` picks the view (default: the form). All three are
// the ScreenEditor (the list is its ListScreenEditor); no LayoutBuilder is mounted from here any more.
import { useMemo } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useQuery } from "@tanstack/react-query";
import { appsCatalog, listEntities } from "@/lib/api";
import { splitByScope, useAppEntityNames } from "@/lib/appScope";
import { qk } from "@/lib/query/keys";
import { t } from "@/lib/i18n";
import { studioHref, viewOfMan } from "@/lib/studioRoutes";
import { EmptyState } from "@/components/slds/EmptyState";
import { Spinner } from "@/components/slds/Spinner";
import { AppFilter } from "@/components/studio/AppFilter";
import type { ScreenMan } from "@/components/studio/screen/ScreenBar";
import { ScreenEditor } from "@/components/studio/screen/ScreenEditor";

export function ScreensPage({ slug, entity, man, app, truong, lienQuan, cheDo }: { slug: string; entity?: string; man?: string; app?: string; truong?: string; lienQuan?: string; cheDo?: string }) {
  const appCode = app ?? null;
  const router = useRouter();
  const entitiesQ = useQuery({ queryKey: ["studio", "count", "entities"], queryFn: listEntities, staleTime: 60_000 });
  const appsQ = useQuery({ queryKey: qk.apps(), queryFn: appsCatalog, staleTime: 60_000 });
  const scope = useAppEntityNames(appCode);
  const all = useMemo(() => entitiesQ.data ?? [], [entitiesQ.data]);
  const { visible, hidden } = useMemo(() => splitByScope(all, scope.entityNames, (e) => e.name), [all, scope.entityNames]);

  if (entitiesQ.isLoading) return <Spinner />;
  // A table named in the URL stays reachable even when the app filter hides it (soft filter).
  const bang = entity && all.some((e) => e.name === entity) ? entity : visible[0]?.name ?? all[0]?.name;
  if (!bang) {
    return (
      <EmptyState
        plain
        title={t("studio.build.screens.empty.title")}
        description={t("studio.build.screens.empty.desc")}
        action={<Link className="slds-button slds-button_brand" href={studioHref(slug, "dung/du-lieu")}>{t("studio.build.screens.empty.action")}</Link>}
      />
    );
  }
  const view = viewOfMan(man) ?? "Form";
  const current: ScreenMan = view === "Detail" ? "ho-so" : view === "List" ? "danh-sach" : "form";
  const pickable = visible.some((e) => e.name === bang) ? visible : [...visible, ...all.filter((e) => e.name === bang)];
  const listMode = cheDo === "danh-sach" && view !== "List";
  const go = (b: string, m: string, tr?: string, rel?: string, mode: boolean = listMode) =>
    router.replace(studioHref(slug, "dung/man-hinh/:bang", { bang: b }, { app: appCode ?? undefined, man: m, truong: tr, "lien-quan": rel, "che-do": mode ? "danh-sach" : undefined }), { scroll: false });
  return (
    <div>
      <div className="slds-grid slds-grid_vertical-align-center slds-wrap slds-m-bottom_small">
        <div className="slds-m-right_small">
          <AppFilter slug={slug} route="dung/man-hinh" apps={appsQ.data ?? []} value={appCode} hiddenCount={hidden} hiddenNoun={t("studio.build.screens.hiddenNoun")}
            query={{ bang, man }} />
        </div>
        <Link className="slds-text-link slds-col_bump-left" href={studioHref(slug, "dung/man-hinh/app")}>{t("studio.build.screens.apps")}</Link>
      </div>
      <ScreenEditor
        key={bang}
        slug={slug}
        app={appCode}
        entities={pickable}
        bang={bang}
        man={current}
        truong={truong ?? null}
        relId={current === "ho-so" ? (lienQuan ?? null) : null}
        onTruong={(f) => { if ((f ?? undefined) !== truong) go(bang, current, f ?? undefined); }}
        onRel={(id) => { if ((id ?? undefined) !== lienQuan) go(bang, "ho-so", undefined, id ?? undefined); }}
        onPick={(b, m) => go(b, m)}
        listMode={listMode}
        onMode={(list) => go(bang, current, undefined, undefined, list)}
      />
    </div>
  );
}
