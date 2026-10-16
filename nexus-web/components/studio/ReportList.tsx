"use client";

// ReportList (specs/074 FR-006, lát f2-report-list-view): the left column of `dung/bao-cao` — reports and dashboards
// in two groups, as real links (blueprint vertical-navigation). The filters are soft and explicit: `entity` (?bang=)
// keeps one table's reports; `entityNames` (?app=) keeps what reads a table of the app. A report with no table, or a
// dashboard with no widget naming one, is never hidden (it cannot be attributed).
import { FileBarChart2, LayoutDashboard } from "lucide-react";
import type { DashboardSummary, ReportSummary } from "@/lib/api";
import { dashboardInScope, splitByScope } from "@/lib/appScope";
import { t } from "@/lib/i18n";
import { useProposedChanges } from "@/components/studio/assistant/ProposalOverlay";
import { EmptyState } from "@/components/slds/EmptyState";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { VerticalNavigation } from "@/components/slds/VerticalNavigation";
import { draftStateLabel, StatusText } from "./StatusText";

// Draft label of a row. An undelivered project's rows carry no draftState, so they get no label.
function draftStatus(state: ReportSummary["draftState"]) {
  const d = draftStateLabel(state);
  return d ? <StatusText tone={d.tone}>{d.label}</StatusText> : undefined;
}

export type ReportListStatus = "loading" | "error" | "ready";

/** The rows the list will draw; ReportsPage uses the same call to pick the default selection and count hidden rows. */
export function filterReportRows(
  reports: ReportSummary[],
  dashboards: DashboardSummary[],
  entityNames: Set<string> | null,
  entity?: string,
) {
  const byTable = entity ? reports.filter((r) => r.entityName === entity) : reports;
  const { visible: rs } = splitByScope(byTable, entityNames, (r) => r.entityName);
  const ds = entity ? [] : dashboards.filter((d) => dashboardInScope(d.widgets.map((w) => w.entity), entityNames));
  return { reports: rs, dashboards: ds };
}

export function ReportList({
  reports, dashboards, entityNames, entity, selectedId, status, reportHref, dashboardHref,
}: {
  reports: ReportSummary[];
  dashboards: DashboardSummary[];
  entityNames: Set<string> | null;
  entity?: string;
  selectedId: string | null;
  status: ReportListStatus;
  reportHref: (id: string) => string;
  dashboardHref: (id: string) => string;
}) {
  const ghosts = useProposedChanges("report");
  if (status === "loading") return <Spinner size="small" label={t("studio.build.reports.listLabel")} />;
  if (status === "error") return <ScopedNotification theme="error">{t("studio.build.reports.loadError")}</ScopedNotification>;
  const rows = filterReportRows(reports, dashboards, entityNames, entity);
  const sections = [];
  const mark = (id: string) => {
    const g = ghosts.find((c) => c.docKey === id);
    if (!g) return {};
    return { ghostKey: g.proposalKey, ...(g.op === "delete" ? { status: <StatusText tone="proposed">{t("studio.build.ai.report.willDelete")}</StatusText> } : { status: <StatusText tone="proposed">{t("studio.status.proposed")}</StatusText> }) };
  };
  // A new report has no row yet: draw it as a ghost row (no link, nothing to open).
  const created = ghosts.filter((c) => c.op === "create").map((c) => ({ key: `ghost-${c.proposalKey}`, label: c.label, href: "", icon: FileBarChart2, ghostOnly: true, ghostKey: c.proposalKey, status: <StatusText tone="proposed">{t("studio.status.proposed")}</StatusText> }));
  if (rows.reports.length > 0 || rows.dashboards.length > 0 || created.length > 0) {
    sections.push({
      id: "reports",
      title: t("studio.build.reports.groupReports"),
      note: rows.reports.length === 0 && created.length === 0 ? t(entity ? "studio.build.reports.emptyTable" : "studio.build.reports.empty") : undefined,
      items: [
        ...rows.reports.map((r) => ({
          key: r.id, label: r.name, href: reportHref(r.id), icon: FileBarChart2, active: r.id === selectedId, status: draftStatus(r.draftState), ...mark(r.id),
        })),
        ...created,
      ],
    });
  }
  if (rows.dashboards.length > 0) {
    sections.push({
      id: "dashboards",
      title: t("studio.build.reports.groupDashboards"),
      items: rows.dashboards.map((d) => ({ key: d.id, label: d.name, href: dashboardHref(d.id), icon: LayoutDashboard, external: true, status: draftStatus(d.draftState) })),
    });
  }
  if (sections.length === 0) {
    return (
      <nav aria-label={t("studio.build.reports.listLabel")}>
        <EmptyState
          title={t(entity ? "studio.build.reports.emptyTable" : "studio.build.reports.empty")}
          description={t("studio.build.reports.emptyHint")}
        />
      </nav>
    );
  }
  return <VerticalNavigation label={t("studio.build.reports.listLabel")} sections={sections} compact />;
}
