"use client";

// ReportsPage (specs/074 FR-006, lát f2-report-list-view): `dung/bao-cao` — the report list on the left, the real
// report drawn on the right (the same ReportViewer the customer's admin has, embedded), and "+ Báo cáo mới" as a form
// in place. The selection lives in the URL (`?bc=`); with nothing there the first report of the CURRENT filter is shown,
// so the page never opens on a report the filter has hidden. Dashboards are listed too and open on their own page.

import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { appsCatalog, listAdminDashboards, listAdminReports, type ReportSummary } from "@/lib/api";
import { useAppEntityNames } from "@/lib/appScope";
import { t, tf } from "@/lib/i18n";
import { useGuardedRouter } from "@/lib/navigation-blocker";
import { qk } from "@/lib/query/keys";
import { studioHref } from "@/lib/studioRoutes";
import { ReportViewer } from "@/components/screens/ReportViewer";
import { Button } from "@/components/slds/Button";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { AppFilter } from "@/components/studio/AppFilter";
import { ReportCreateForm } from "@/components/studio/ReportCreateForm";
import { ReportList, filterReportRows } from "@/components/studio/ReportList";

export function ReportsPage({ slug, bc, bang, app }: { slug: string; bc?: string; bang?: string; app?: string }) {
  const router = useGuardedRouter();
  const reportsQ = useQuery({ queryKey: qk.adminReports(), queryFn: listAdminReports, staleTime: 30_000 });
  const dashQ = useQuery({ queryKey: qk.adminDashboards(), queryFn: listAdminDashboards, staleTime: 30_000 });
  const appsQ = useQuery({ queryKey: qk.apps(), queryFn: appsCatalog, staleTime: 60_000 });
  const scope = useAppEntityNames(app ?? null);
  const [creating, setCreating] = useState(false);
  const [createdId, setCreatedId] = useState<string | null>(null);

  const reports = useMemo(() => reportsQ.data ?? [], [reportsQ.data]);
  const dashboards = useMemo(() => dashQ.data ?? [], [dashQ.data]);
  const rows = useMemo(() => filterReportRows(reports, dashboards, scope.entityNames, bang), [reports, dashboards, scope.entityNames, bang]);
  const selected: ReportSummary | undefined = rows.reports.find((r) => r.id === bc) ?? rows.reports[0];
  const hidden = (bang ? reports.filter((r) => r.entityName === bang) : reports).length - rows.reports.length;

  const status = reportsQ.isError || dashQ.isError ? "error" : reportsQ.isPending || dashQ.isPending || scope.isLoading ? "loading" : "ready";
  const hrefFor = (id?: string) => studioHref(slug, "dung/bao-cao", {}, { bc: id, bang, app });
  const select = (r: ReportSummary) => {
    setCreating(false);
    setCreatedId(r.id);
    router.replace(hrefFor(r.id));
  };

  if (scope.unknown) return <p className="slds-p-around_medium">{tf("studio.build.data.unknownApp", { app: app ?? "" })}</p>;

  return (
    <div className="slds-p-around_medium">
      <div className="slds-grid slds-grid_vertical-align-center slds-wrap slds-m-bottom_small">
        <p className="slds-text-body_small slds-text-color_weak slds-m-right_medium">
          {status === "ready" ? tf("studio.build.reports.step", { n: rows.reports.length, m: rows.dashboards.length }) : "…"}
        </p>
        <AppFilter
          slug={slug}
          route="dung/bao-cao"
          apps={appsQ.data ?? []}
          value={app ?? null}
          hiddenCount={hidden}
          hiddenNoun={t("studio.build.reports.hiddenNoun")}
          query={{ bang }}
        />
        <div className="slds-col_bump-left">
          <Button onClick={() => setCreating((v) => !v)} aria-expanded={creating}>{t("studio.build.reports.new")}</Button>
        </div>
      </div>
      {creating && (
        <ReportCreateForm entity={bang} entityNames={scope.entityNames} onCancel={() => setCreating(false)} onCreated={select} />
      )}
      <div className="grid gap-4 md:grid-cols-[18rem_minmax(0,1fr)]">
        <ReportList
          reports={reports}
          dashboards={dashboards}
          entityNames={scope.entityNames}
          entity={bang}
          selectedId={selected?.id ?? null}
          status={status}
          reportHref={hrefFor}
          dashboardHref={(id) => studioHref(slug, "dung/bao-cao/tong-quan/:id", { id })}
        />
        <div className="min-w-0">
          {status === "loading" ? null : selected ? (
            // The key carries the flag: a new report can be the first of the list (and so shown) a beat before select() runs,
            // and a viewer already mounted would never read openConfig again.
            <ReportViewer key={`${selected.id}:${createdId === selected.id}`} id={selected.id} embedded openConfig={createdId === selected.id} />
          ) : status === "ready" ? (
            <ScopedNotification>{t("studio.build.reports.pickOne")}</ScopedNotification>
          ) : null}
        </div>
      </div>
    </div>
  );
}
