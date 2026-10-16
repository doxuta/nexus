"use client";

// /reports/[id] — report viewer (W4 v2) on SLDS (specs/038 T030, RESEARCH §5 /reports/[id]).
// • Authoring folded in (kills the /studio/reports split-brain): admin "Cấu hình" opens the
//   ReportConfigPanel docked right (panels/base) — name/type/columns/groupBy/bucket/metric/join;
//   a save invalidates the run query so the result re-renders live (?config=1 opens it ready).
// • Filter bar SHARED with the dashboard viewer (RunFilterBar) → ?dateFrom/?dateTo/?ownerId of
//   GET /reports/:id/run — the viewer re-runs the report without touching its definition (§3.11).
// • Result by type: tabular → data-tables/base (relation/user cells read as names, row header
//   links to the record); summary grouped → chart card (Cột ↔ Tròn) + group table whose keys
//   drill to the filtered list; config.bucket → time series (Đường ↔ Cột), zero-filled, bucket
//   axis labels; no groups → scalar card. Charts are Nx charts (recharts) — no SLDS blueprint,
//   framed in cards. A Matrix with a column axis draws as a cross-tab with row/column totals
//   (ReportPivotTable, specs/049 F1). A result with `levels` (nested groups, or the one-level
//   threshold path) draws the first level as the chart and ALL levels as a TreeGrid with a subtotal
//   per group and a closing Tổng cộng (F2); a matrix report with no column axis is flagged to the
//   admin only.
// • "Xuất CSV" = GET /reports/:id/export.csv with EXACTLY the filters on screen (BE parity with
//   /run, api-conventions §4.4). Server failure → client CSV of what is on screen, said out loud
//   in a toast (the tabular run caps at 500 rows — never silent) — except for a grid/tree result,
//   where a key,value,count file would contradict the screen: toast only. The grid/tree file itself
//   is the server's (S4 writeGridCSV): the same rows/columns/cells as the cross-tab on screen.
// • KPI delta: ReportConfig has no period-comparison concept → skipped honestly, no fake trend.
// Error model: direct HTTP error (422/404/403) — NOT partial success; a 422 config error is a real
// state. The sentence comes from lib/reportErrorHint per code (FR-024): staff never see the machine
// code or the server's raw sentence; the admin keeps both for diagnosis.
// Design read: record-home style viewer for the customer face (+ admin panel); blueprints
// page-headers/record-home · cards · data-tables/base · button-groups/button-icons (stateful) ·
// panels/base · scoped-notifications · spinners. VARIANCE 3 · MOTION 2 · DENSITY 7.
import Link from "next/link";
import { useState, type ReactNode } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import type { CellContext, ColumnDef } from "@tanstack/react-table";
import { BarChart2, ChartPie, CloudDownload, FileBarChart2, LineChart, Settings2, Sigma, Table2 } from "lucide-react";
import {
  ApiError, downloadReportCSV, listFields, listReports, listUserDirectory, runReport,
  type ReportRunData, type ReportRunRow,
} from "@/lib/api";
import { qk } from "@/lib/query/keys";
import { csvGuard, reportCellText } from "@/lib/reportCell";
import { reportResultShape, reportShapeNote } from "@/lib/reportConfig";
import { buildPivotGrid, buildReportTree, type TreeNode } from "@/lib/reportGrid";
import { reportErrorHint } from "@/lib/reportErrorHint";
import { useAuth } from "@/lib/auth/AuthProvider";
import { useEntities } from "@/lib/metadata/hooks";
import { aggGroupsToChartData, formatWidgetValue, relabelChartData } from "@/lib/analytics";
import { filtersToQueryKey, type RunFilters } from "@/lib/runFilters";
import {
  BUCKET_CHOICES, bucketAxisLabel, bucketSeriesToChartData, isBucketId, zeroFillGroups, type BucketId,
} from "@/lib/timeBuckets";
import { t, tf } from "@/lib/i18n";
import { filterRowsFromSaved } from "@/lib/savedFilters";
import { withOwnerFilter } from "@/lib/filterModel";
import { conditionText, fieldLabel as filterFieldLabel } from "@/components/slds/ExpressionBuilder";
import { useRunFilters } from "@/components/analytics/useRunFilters";
import { RunFilterBar } from "@/components/analytics/RunFilterBar";
import { ReportConfigPanel } from "@/components/analytics/ReportConfigPanel";
import { ReportPivotTable } from "@/components/analytics/ReportPivotTable";
import { DraftViewButton, DraftViewNotice, useDraftView } from "@/components/analytics/useDraftView";
import { NxBar } from "@/components/charts/NxBar";
import { NxLine } from "@/components/charts/NxLine";
import { NxPie } from "@/components/charts/NxPie";
import { toast } from "@/components/ui/Toaster";
import { PageHeader } from "@/components/slds/PageHeader";
import { Button } from "@/components/slds/Button";
import { ButtonGroup } from "@/components/slds/ButtonGroup";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Card } from "@/components/slds/Card";
import { DataTable } from "@/components/slds/DataTable";
import { Panel } from "@/components/slds/Panel";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { TreeGrid } from "@/components/slds/TreeGrid";
import { reportTypeLabel } from "./ReportsScreen";

const PANEL_ID = "nx-report-config";

// A run that returned nothing at all (no rows, no groups, no count): the Studio says "no data yet" instead of an empty card.
function isEmptyRun(d: ReportRunData): boolean {
  if (d.type === "tabular") return d.total === 0 && d.rows.length === 0;
  return !d.groups?.length && !d.rows?.length && !d.cells?.length && !d.levels?.length && Number(d.count ?? 0) === 0;
}

// ── Client CSV fallback (kept from P2 #12 — only when the server export fails) ─────
function downloadClientCSV(matrix: string[][], filename: string) {
  // csvGuard: the server export neutralizes formula-looking cells; this file must too.
  const csv = matrix.map((r) => r.map((c) => `"${csvGuard(String(c)).replace(/"/g, '""')}"`).join(",")).join("\n");
  const blob = new Blob(["﻿" + csv], { type: "text/csv;charset=utf-8;" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url; a.download = filename; a.click();
  URL.revokeObjectURL(url);
}

// Drill-through targets — same contract as the dashboard viewer.
function drillUrl(entity: string, groupByField: string | undefined, key: string): string {
  if (!groupByField || !entity) return `/${entity}`;
  return `/${entity}?${encodeURIComponent(groupByField)}%5Beq%5D=${encodeURIComponent(key)}`;
}

// Per-report grouped-chart preference (per-browser — report config has no viz field).
type ReportViz = "bar" | "pie" | "line";
const VIZ_KEY = (id: string) => `nexus.report.viz.v1.${id}`;
function loadReportViz(id: string): ReportViz | null {
  if (typeof window === "undefined") return null;
  const v = window.localStorage.getItem(VIZ_KEY(id));
  return v === "bar" || v === "pie" || v === "line" ? v : null;
}

const bucketLabel = (b: BucketId) => BUCKET_CHOICES.find((c) => c.value === b)?.label ?? b;

// Export toast text: staff read the hint for the code, never the server's raw sentence (FR-024).
function exportErrorText(e: unknown, isAdmin: boolean): string {
  if (!(e instanceof ApiError)) return e instanceof Error ? e.message : t("page.reports.viewer.exportFailed");
  const h = reportErrorHint(e.code, e.details, isAdmin);
  return h.showCode ? `${h.body} ${e.message} (${tf("page.reports.viewer.errorCode", { code: e.code })})` : h.body;
}

export function ReportViewer({
  id, embedded, onConfigure, openConfig,
}: {
  id: string;
  /** Inside the Studio work surface (specs/074): the frame owns the h1 and the padding. */
  embedded?: boolean;
  /** Called when the maker opens the config panel (embedded: nothing in the URL says so). */
  onConfigure?: () => void;
  /** Embedded: open the config panel at once (a report just created in place). */
  openConfig?: boolean;
}) {
  const router = useRouter();
  const [filters, setFilters] = useRunFilters();
  // Admins get the docked config panel; the create dialog deep-links here with ?config=1.
  const { user } = useAuth();
  const isAdmin = Boolean(user?.isAdmin);
  const searchParams = useSearchParams();
  const [cfgOpen, setCfgOpen] = useState(embedded ? Boolean(openConfig) : searchParams.get("config") === "1");

  // specs/063 S7-2: the builder can open this screen on the unpublished draft (?draft=1). The flag is in
  // the query keys, so the live and the draft results never share a cache entry.
  const draftView = useDraftView();
  const draftKey: Record<string, string> = draftView.on ? { draft: "1" } : {};

  // Report metadata (name, entity, config) for DISPLAY from the catalog; the edit form reads the report itself.
  const { data: reports } = useQuery({
    queryKey: draftView.on ? [...qk.reports(), "draft"] : qk.reports(), queryFn: listReports, staleTime: 60_000,
  });
  const reportMeta = reports?.find((r) => r.id === id);
  const drillEntity = reportMeta?.entityName;
  const drillGroupBy = reportMeta?.config?.groupBy as string | undefined;
  const { data: entities = [] } = useEntities();
  // #450: the report's saved fixed conditions, so staff know why the count differs from the list.
  const fixedFilterRows = filterRowsFromSaved(reportMeta?.config?.filters);
  // REPORT-LABELS (WAVE 6): the groupBy field's display labels ("Proposal" → "Báo giá"). Drill
  // keys STAY codes — the drilled list filters ?field[eq]=<code>.
  const { data: reportEntityFields } = useQuery({
    queryKey: qk.fields(drillEntity ?? ""),
    queryFn: () => listFields(drillEntity!),
    staleTime: 5 * 60_000,
    enabled: !!drillEntity && (!!drillGroupBy || fixedFilterRows.length > 0),
  });
  const groupLabels = drillGroupBy
    ? (reportEntityFields?.find((f) => f.name === drillGroupBy)?.options?.valueLabels as Record<string, string> | undefined)
    : undefined;
  const filterFields = withOwnerFilter(reportEntityFields ?? []);
  const needsUsers = fixedFilterRows.some((r) => filterFields.find((x) => x.name === r.field)?.fieldType === "user");
  // A person condition names the person, never the id (#449).
  const { data: directory } = useQuery({ queryKey: ["users", "directory", "viewer-note"], queryFn: () => listUserDirectory("", 100), staleTime: 5 * 60_000, enabled: needsUsers });
  const userNames = Object.fromEntries((directory ?? []).map((u) => [u.id, u.fullName?.trim() || u.email]));
  const fixedFilterText = fixedFilterRows.map((r) => {
    const base = filterFields.find((x) => x.name === r.field);
    const f = base?.fieldType === "user" ? { ...base, options: { ...base.options, valueLabels: userNames } } : base;
    return `${f ? filterFieldLabel(f) : r.field} ${conditionText(r, f)}`.trim();
  }).join(" · ");
  const cfgBucket = reportMeta?.config?.bucket;
  const bucket: BucketId | null = isBucketId(cfgBucket) ? cfgBucket : null;

  const { data, isLoading, isError, error, isFetching } = useQuery({
    queryKey: qk.reportRun(id, { ...filtersToQueryKey(filters), ...draftKey }),
    queryFn: () => runReport(id, filters),
    staleTime: 60_000,
    retry: false, // 422 config errors should not be retried
    placeholderData: keepPreviousData,
  });

  // W4: server-side CSV with the CURRENT overlay filters (parity with the run on screen).
  const [exporting, setExporting] = useState(false);
  const exportCSV = async () => {
    if (!data) return;
    const filename = `${(reportMeta?.name ?? "report").replace(/[\\/:*?"<>|]+/g, "-")}.csv`;
    setExporting(true);
    try {
      await downloadReportCSV(id, filename, filters);
    } catch (e) {
      // Honest fallback: the client CSV holds only what is ON SCREEN (tabular run caps at 500 rows).
      const msg = exportErrorText(e, isAdmin);
      if (reportResultShape(data) !== "flat") {
        // A grid/tree flattened to key,value,count would contradict the screen — no file.
        toast.error(msg);
      } else if (data.type === "tabular") {
        downloadClientCSV(
          [data.columns.map((c) => c.label || c.name),
            ...data.rows.map((row) => data.columns.map((c) => reportCellText(row[c.name], "")))],
          filename,
        );
        toast.warning(tf("page.reports.viewer.exportFallbackPage", { msg }));
      } else if (data.groups?.length) {
        downloadClientCSV(
          [[t("page.reports.viewer.col.group"), t("page.reports.viewer.col.value"), t("page.reports.viewer.col.count")],
            ...data.groups.map((g) => [g.key, g.value, String(g.count)])],
          filename,
        );
        toast.warning(tf("page.reports.viewer.exportFallback", { msg }));
      } else {
        toast.error(msg);
      }
    } finally {
      setExporting(false);
    }
  };

  const errMsg = isError
    ? (error && typeof error === "object" && "message" in error
        ? (error as { message: string }).message
        : t("page.reports.viewer.errorFallback"))
    : null;
  const errCode = isError && error && typeof error === "object" && "code" in error
    ? (error as { code: string }).code
    : null;
  // A staff member hitting FORBIDDEN was once told to go index a field (CEO walkthrough, WAVE 6):
  // one hint per code, the index advice only for the index error (lib/reportErrorHint).
  const errHint = isError
    ? reportErrorHint(errCode, error instanceof ApiError ? error.details : undefined, isAdmin)
    : null;
  // Say what the screen does not draw yet (grid → row totals, tree → first level) + the threshold.
  const fieldLabel = (name: string) => reportEntityFields?.find((f) => f.name === name)?.label;
  const notes = data && !isError ? reportShapeNote(data, reportMeta?.type, isAdmin, fieldLabel) : [];

  const pivot = data && !isError ? buildPivotGrid(data) : null;
  const exportable = Boolean(data && !isError && (data.type === "tabular" ? data.rows.length > 0 : true));
  const entityLabel = entities.find((e) => e.name === reportMeta?.entityName)?.label ?? reportMeta?.entityName;

  const actions: ReactNode[] = [];
  if (isAdmin && reportMeta) {
    actions.push(
      <Button
        iconLeft={Settings2}
        aria-expanded={cfgOpen}
        aria-controls={PANEL_ID}
        title={t("page.reports.viewer.configureTitle")}
        onClick={() => { if (!cfgOpen) onConfigure?.(); setCfgOpen((v) => !v); }}
      >
        {t("page.reports.viewer.configure")}
      </Button>,
    );
  }
  actions.push(<DraftViewButton key="draft-view" view={draftView} />);
  actions.push(
    <Button
      iconLeft={CloudDownload}
      onClick={exportCSV}
      disabled={!exportable || exporting}
      title={t("page.reports.viewer.exportTitle")}
    >
      {exporting ? t("page.reports.viewer.exporting") : t("page.reports.viewer.export")}
    </Button>,
  );

  return (
    <div className={embedded ? "flex min-h-0 flex-1" : "flex min-h-0 flex-1 overflow-hidden"}>
      <div className={embedded ? "flex min-w-0 flex-1 flex-col gap-4" : "flex min-w-0 flex-1 flex-col gap-4 overflow-y-auto p-4"}>
        {embedded ? (
          <div className="slds-grid slds-grid_vertical-align-center slds-wrap">
            <div className="slds-col slds-has-flexi-truncate">
              <h2 className="slds-text-heading_medium slds-truncate" title={reportMeta?.name}>{reportMeta?.name ?? t("page.reports.label")}</h2>
              {reportMeta && (
                <p className="slds-text-body_small slds-text-color_weak">
                  {entityLabel ?? "—"} · {reportTypeLabel(reportMeta.type)}{bucket ? ` · ${bucketLabel(bucket)}` : ""}
                </p>
              )}
            </div>
            <div className="slds-col slds-no-flex slds-grid slds-grid_vertical-align-center">
              {actions.map((a, i) => <div key={i} className="slds-m-left_x-small">{a}</div>)}
            </div>
          </div>
        ) : (
        <PageHeader
          variant="record-home"
          icon={{ glyph: FileBarChart2, name: "report" }}
          label={t("page.reports.label")}
          title={reportMeta?.name ?? t("page.reports.label")}
          actions={actions}
          details={reportMeta ? [
            { label: t("page.reports.col.entity"), value: entityLabel ?? "—" },
            { label: t("page.reports.col.type"), value: reportTypeLabel(reportMeta.type) },
            ...(bucket ? [{ label: t("page.reports.config.bucket"), value: bucketLabel(bucket) }] : []),
          ] : undefined}
        />
        )}

        <DraftViewNotice view={draftView} state={data?.draftState ?? reportMeta?.draftState} />

        {fixedFilterText && (
          <p className="slds-text-body_small slds-text-color_weak">
            <strong>{t("page.reports.viewer.fixedFilters")}:</strong> {fixedFilterText}
          </p>
        )}

        {/* W4: viewer-side controls — re-run with an overlay, never edit the definition */}
        <RunFilterBar filters={filters} onChange={setFilters} />

        {isLoading && <Card title={t("page.reports.viewer.running")} icon={{ glyph: Table2, name: "report" }} loading />}

        {/* Real backend error (422 config / 403 / 404) — the hint must match the error; staff
            read only the hint, the admin also gets the server's sentence + code (FR-024). */}
        {errHint && !isLoading && (
          <ScopedNotification theme="warning">
            <span className="slds-text-heading_small">{errHint.title}</span>
            <br />
            {errHint.body}
            {errHint.showCode && (
              <>
                <br />
                <span className="slds-text-body_small">
                  {errMsg}
                  {errCode && <> ({tf("page.reports.viewer.errorCode", { code: errCode })})</>}
                </span>
              </>
            )}
          </ScopedNotification>
        )}

        {notes.map((n) => (
          <ScopedNotification key={n}>{n}</ScopedNotification>
        ))}

        {data && !isError && embedded && isEmptyRun(data) && (
          <ScopedNotification>{t("studio.build.reports.noData")}</ScopedNotification>
        )}

        {data && !isError && !(embedded && isEmptyRun(data)) && (
          // Cards stack on SLDS `.slds-card+.slds-card{margin-top:1rem}` — no flex gap on top of it.
          <div className="relative">
            {data.type === "tabular" && (
              <TabularResult data={data} entity={drillEntity} />
            )}
            {pivot && data.type !== "tabular" && (
              <Card title={t("page.reports.viewer.pivot.card")} icon={{ glyph: Table2, name: "report" }} padded={false}>
                <ReportPivotTable
                  grid={pivot}
                  label={t("page.reports.viewer.pivot.card")}
                  rowTitle={(data.rowAxis && fieldLabel(data.rowAxis.field)) || t("page.reports.viewer.pivot.rows")}
                  columnTitle={(data.columnAxis && fieldLabel(data.columnAxis.field)) || t("page.reports.viewer.pivot.columns")}
                  format={(v) => formatWidgetValue(v, reportMeta?.config?.metric as string | undefined, data.currencyCode)}
                />
              </Card>
            )}
            {!pivot && (data.type === "summary" || data.type === "matrix") && (
              <SummaryResult
                reportId={id}
                data={data}
                metric={reportMeta?.config?.metric as string | undefined}
                bucket={bucket}
                filters={filters}
                groupLabels={groupLabels}
                drillHref={drillEntity ? (code) => drillUrl(drillEntity, drillGroupBy, code) : undefined}
                onDrill={drillEntity ? (code) => router.push(drillUrl(drillEntity, drillGroupBy, code)) : undefined}
              />
            )}
            {isFetching && <Spinner container size="medium" label={t("page.reports.viewer.running")} />}
          </div>
        )}
      </div>

      {/* Admin authoring — docked right; the run on the left re-renders live on save */}
      {isAdmin && reportMeta && (
        <Panel
          id={PANEL_ID}
          open={cfgOpen}
          position="right"
          size="large"
          title={t("page.reports.config.title")}
          onClose={() => setCfgOpen(false)}
        >
          {cfgOpen && <ReportConfigPanel key={id} reportId={id} onSavedToDraft={() => draftView.set(true)} />}
        </Panel>
      )}
    </div>
  );
}

// ── Tabular result — data-tables/base; the row header links to the record ──────
function TabularResult({ data, entity }: { data: Extract<ReportRunData, { type: "tabular" }>; entity?: string }) {
  // Composite ids: a saved config CAN carry a duplicated column name — a bare name would collide.
  // REPORT-RELATION-LABELS: a relation/user column arrives as {id,label} (resolved server-side) —
  // reportCellText shows the label, never "[object Object]" and never the UUID when a name was served.
  // (No `cell: undefined` — an explicit undefined replaces tanstack's default renderer with nothing.)
  const columns: ColumnDef<ReportRunRow>[] = data.columns.map((col, i) => ({
    id: `${col.name}:${i}`,
    header: col.label || col.name,
    accessorFn: (row: ReportRunRow) => reportCellText(row[col.name]),
    ...(i === 0 && entity && {
      cell: ({ row, getValue }: CellContext<ReportRunRow, unknown>) => (
        <Link href={`/${entity}/${row.original.id}`}>{getValue() as string}</Link>
      ),
    }),
  }));
  return (
    <Card
      title={tf("page.reports.viewer.records", { n: data.total.toLocaleString("vi-VN") })}
      icon={{ glyph: Table2, name: "report" }}
      padded={false}
    >
      <DataTable
        label={t("page.reports.viewer.resultGrid")}
        columns={columns}
        data={data.rows}
        getRowId={(r, i) => r.id || `r${i}`}
      />
    </Card>
  );
}

// ── Summary / matrix result — chart card + group table ───────────────────────────
interface GroupRow { id: string; label: string; code: string; value: string; count: number }

function SummaryResult({
  reportId, data, metric, bucket, filters, groupLabels, drillHref, onDrill,
}: {
  reportId: string;
  data: Extract<ReportRunData, { type: "summary" | "matrix" }>;
  metric?: string;
  bucket: BucketId | null;
  filters: RunFilters;
  /** Display label per group CODE (options.valueLabels) — drill still travels by code. */
  groupLabels?: Record<string, string>;
  drillHref?: (code: string) => string;
  onDrill?: (code: string) => void;
}) {
  // Viz preference — default: bucket → line, else bar. Read in the initializer: this component only
  // mounts once the run query resolved, which is client-side only, so there is no hydration to drift
  // (and the viewer remounts per report id).
  const [viz, setViz] = useState<ReportViz>(() => loadReportViz(reportId) ?? (bucket ? "line" : "bar"));
  const pickViz = (v: ReportViz) => {
    setViz(v);
    if (typeof window !== "undefined") window.localStorage.setItem(VIZ_KEY(reportId), v);
  };
  const fmt = (n: number | string) => formatWidgetValue(n, metric, data.currencyCode);

  // Levelled result (specs/049 F2): every level in one tree; the first level also feeds the chart.
  const tree = buildReportTree(data, groupLabels);
  if (tree?.empty) {
    return (
      <Card title={t("page.reports.viewer.tree.card")} icon={{ glyph: Table2, name: "report" }}>
        <p className="slds-text-align_center slds-text-color_weak slds-p-vertical_large">{t(data.having ? "page.reports.viewer.tree.empty" : "state.noData")}</p>
      </Card>
    );
  }

  // Scalar (no groups) — KPI delta skipped honestly: ReportConfig has no period comparison.
  if (!data.groups?.length) {
    return (
      <Card title={t("page.reports.viewer.scalar")} icon={{ glyph: Sigma, name: "report" }}>
        <p className="slds-text-heading_large tabular-nums">{fmt(data.value ?? "")}</p>
        <p className="slds-text-body_small slds-text-color_weak">
          {tf("page.reports.viewer.records", { n: (data.count ?? 0).toLocaleString("vi-VN") })}
        </p>
      </Card>
    );
  }

  const groups = data.groups;

  // Time series (config.bucket): zero-fill + chronological + bucket axis labels; no drill (a bucket
  // key is not a field value). The table keeps the RAW bucket key next to its label.
  if (bucket) {
    const range = { from: filters.dateFrom, to: filters.dateTo };
    const pts = bucketSeriesToChartData(groups, bucket, range);
    const rows: GroupRow[] = zeroFillGroups(groups, bucket, range).map((g) => ({
      id: g.key, code: g.key, label: `${g.key} — ${bucketAxisLabel(g.key, bucket, true)}`,
      value: fmt(g.value), count: g.count,
    }));
    const shown = viz === "bar" ? "bar" : "line";
    return (
      <>
        <Card
          title={t("page.reports.viewer.timeSeries")}
          icon={{ glyph: LineChart, name: "report" }}
          actions={
            <VizSwitch
              value={shown}
              onChange={pickViz}
              options={[{ id: "line", icon: LineChart }, { id: "bar", icon: BarChart2 }]}
            />
          }
        >
          <p className="slds-text-body_small slds-text-color_weak slds-m-bottom_small">
            {tf("page.reports.viewer.timeSeriesNote", { bucket: bucketLabel(bucket).toLowerCase() })}
          </p>
          {pts.length === 0 ? (
            <p className="slds-text-align_center slds-text-color_weak slds-p-vertical_large">{t("page.reports.viewer.noRange")}</p>
          ) : shown === "bar" ? (
            <NxBar data={pts} height={224} formatValue={fmt} />
          ) : (
            <NxLine data={pts} height={224} formatValue={fmt} />
          )}
        </Card>
        {tree
          ? <TreeResult nodes={tree.nodes} fmt={fmt} />
          : <GroupTable rows={rows} keyHeader={tf("page.reports.viewer.col.bucket", { bucket: bucketLabel(bucket) })} />}
      </>
    );
  }

  // Grouped categorical: Cột ↔ Tròn (§3.11 "summary → NxBar/NxPie switch").
  // Labels for humans, codes for drill (relabelChartData's whole contract).
  const { pts, codeOf } = relabelChartData(aggGroupsToChartData(groups), groupLabels);
  const drill = onDrill ? (display: string) => onDrill(codeOf(display)) : undefined;
  const rows: GroupRow[] = pts.map((p) => ({
    id: p.key, label: p.key, code: codeOf(p.key), value: fmt(p.value), count: p.count,
  }));
  const shown = viz === "pie" ? "pie" : "bar";

  return (
    <>
      <Card
        title={t("page.reports.viewer.grouped")}
        icon={{ glyph: shown === "pie" ? ChartPie : BarChart2, name: "report" }}
        actions={
          <VizSwitch
            value={shown}
            onChange={pickViz}
            options={[{ id: "bar", icon: BarChart2 }, { id: "pie", icon: ChartPie }]}
          />
        }
      >
        {shown === "pie" ? (
          <NxPie
            data={pts}
            formatValue={fmt}
            centerLabel={t(metric === "count" ? "page.reports.viewer.centerCount" : "page.reports.viewer.centerTotal")}
            onSelect={drill}
          />
        ) : (
          <NxBar data={pts.slice(0, 12)} height={200} formatValue={fmt} onSelect={drill} />
        )}
      </Card>
      {tree
        ? <TreeResult nodes={tree.nodes} fmt={fmt} drillHref={drillHref} />
        : <GroupTable rows={rows} keyHeader={t("page.reports.viewer.col.group")} drillHref={drillHref} />}
    </>
  );
}

// Chart type switch — button-groups of stateful button-icons (aria-pressed + slds-is-selected).
function VizSwitch<T extends ReportViz>({
  value, onChange, options,
}: {
  value: T;
  onChange: (v: T) => void;
  options: { id: T; icon: typeof BarChart2 }[];
}) {
  return (
    <ButtonGroup label={t("page.reports.viewer.viz")}>
      {options.map((o) => (
        <ButtonIcon
          key={o.id}
          icon={o.icon}
          variant="border-filled"
          label={t(`page.reports.viewer.viz.${o.id}`)}
          selected={value === o.id}
          onClick={() => onChange(o.id)}
        />
      ))}
    </ButtonGroup>
  );
}

// Levelled result (specs/049 F2) — blueprint tree-grid/base. Each group row is its own subtotal, the
// closing row is the grand total. Level 1 opens by default; only first-level keys drill (the drill
// filters by config.groupBy, which is the first level), and never the total row.
function TreeResult({ nodes, fmt, drillHref }: { nodes: TreeNode[]; fmt: (v: string) => string; drillHref?: (code: string) => string }) {
  const columns: ColumnDef<TreeNode>[] = [
    {
      id: "key",
      header: t("page.reports.viewer.col.group"),
      accessorKey: "label",
      cell: ({ row }: CellContext<TreeNode, unknown>) => {
        const n = row.original;
        return drillHref && n.depth === 0 && !n.total ? (
          <Link href={drillHref(n.code)} title={tf("page.reports.viewer.filterBy", { key: n.label })}>{n.label}</Link>
        ) : (
          n.label
        );
      },
    },
    { id: "value", header: t("page.reports.viewer.col.value"), accessorFn: (n) => (n.value == null ? "—" : fmt(n.value)), meta: { numeric: true } },
    { id: "count", header: t("page.reports.viewer.col.count"), accessorFn: (n) => n.count, meta: { numeric: true } },
  ];
  // A fresh result (other ids) re-mounts so level 1 opens again; the same ids keep the user's choice.
  const sig = nodes.map((n) => n.id).join("|");
  const open = Object.fromEntries(nodes.filter((n) => n.children?.length).map((n) => [n.id, true]));
  return (
    <Card title={t("page.reports.viewer.tree.card")} icon={{ glyph: Table2, name: "report" }} padded={false}>
      <TreeGrid
        key={sig}
        label={t("page.reports.viewer.tree.card")}
        columns={columns}
        data={nodes}
        getSubRows={(n) => n.children}
        getRowId={(n) => n.id}
        defaultExpanded={open}
      />
    </Card>
  );
}

// Group data table (data-tables/base) shared by both summary flavours; the key cell drills.
function GroupTable({ rows, keyHeader, drillHref }: { rows: GroupRow[]; keyHeader: string; drillHref?: (code: string) => string }) {
  const columns: ColumnDef<GroupRow>[] = [
    {
      id: "key",
      header: keyHeader,
      accessorKey: "label",
      ...(drillHref && {
        cell: ({ row }: CellContext<GroupRow, unknown>) => (
          <Link href={drillHref(row.original.code)} title={tf("page.reports.viewer.filterBy", { key: row.original.label })}>
            {row.original.label}
          </Link>
        ),
      }),
    },
    { id: "value", header: t("page.reports.viewer.col.value"), accessorKey: "value", meta: { numeric: true } },
    { id: "count", header: t("page.reports.viewer.col.count"), accessorKey: "count", meta: { numeric: true } },
  ];
  return (
    <Card title={t("page.reports.viewer.groupGrid")} icon={{ glyph: Table2, name: "report" }} padded={false}>
      <DataTable label={t("page.reports.viewer.groupGrid")} columns={columns} data={rows} getRowId={(r) => r.id} />
    </Card>
  );
}
