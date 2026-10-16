"use client";

// /reports — report catalog. Endpoint: GET /reports (non-admin catalog, auth-only).
// Authoring merged in (kills /studio/reports): admin "Tạo báo cáo" = minimal dialog
// (name + entity, type defaults to tabular) → POST /admin/reports → viewer /reports/[id]?config=1
// where the full ReportConfigPanel takes over.
//
// SLDS (specs/038 T030, RESEARCH §5 /reports): the ObjectHomePage template — page-headers/object-home
// in a card, report type as the list-view switcher next to the title (was a pill toolbar), the
// catalog as data-tables/advanced with a row action menu ("Chạy"), modals/base for create,
// illustration for the empty state. Entity shows its label, not the table name.
import Link from "next/link";
import { entityOptionText, useEntityOptions } from "@/lib/entityOptions";
import { useState } from "react";
import { useRouter } from "next/navigation";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { ChevronDown, FileBarChart2 } from "lucide-react";
import { createReport, listFields, listReports, type ReportSummary } from "@/lib/api";
import { qk } from "@/lib/query/keys";
import { buildReportConfig } from "@/lib/reportConfig";
import { useAuth } from "@/lib/auth/AuthProvider";
import { useEntities } from "@/lib/metadata/hooks";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { ObjectHomePage } from "@/components/slds/templates/ObjectHomePage";
import { Menu, MenuCheckboxItem, MenuItem } from "@/components/slds/Menu";
import { DataTable } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Modal } from "@/components/slds/Modal";
import { Input } from "@/components/slds/Input";
import { Select } from "@/components/slds/Select";
import { Button } from "@/components/slds/Button";
import { Badge } from "@/components/slds/Badge";
import { DraftViewButton, useDraftView } from "@/components/analytics/useDraftView";
import { hasDraftChange } from "@/lib/api";
import { useWorkshopScope } from "@/lib/workshopScope";
import { inEntityScope } from "@/lib/workshopScope";
import { scopeEntityOptions } from "@/lib/appScope";
import { ScopeHiddenNote } from "@/components/studio/ScopeHiddenNote";

const REPORT_TYPES = ["tabular", "summary", "matrix"] as const;
type ReportTypeFilter = "all" | typeof REPORT_TYPES[number];

/** Display label of a report type; unknown types show as-is. */
export function reportTypeLabel(type: string | undefined): string {
  return (REPORT_TYPES as readonly string[]).includes(type ?? "")
    ? t(`page.reports.type.${type}` as I18nKey)
    : (type ?? "—");
}

/** `entity` (specs/073 kh3, `?bang=`): list only the reports built on that table. `onOpen` (specs/074): the host opens a report itself instead of a route change. */
export function ReportsScreen({ entity, clearEntityHref, onOpen }: { entity?: string; clearEntityHref?: string; onOpen?: (id: string) => void } = {}) {
  const [q, setQ] = useState("");
  const [typeFilter, setTypeFilter] = useState<ReportTypeFilter>("all");
  const workshop = useWorkshopScope();
  const [showAll, setShowAll] = useState(false);
  const reportHref = (id: string) => (workshop ? workshop.panelHref("bao-cao", { report: id }) : `/reports/${id}`);
  const { user } = useAuth();
  const isAdmin = Boolean(user?.isAdmin);
  const router = useRouter();
  const qc = useQueryClient();

  // specs/063 S7-2: the builder can list the catalog over the unpublished draft (?draft=1).
  const draftView = useDraftView();
  const { data, isLoading, isError, error } = useQuery({
    queryKey: draftView.on ? [...qk.reports(), "draft"] : qk.reports(),
    queryFn: listReports,
    staleTime: 30_000,
  });

  // ── Admin create — minimal dialog; the full config panel lives on the viewer page ──
  const { data: allEntities = [] } = useEntities();
  // Inside an app workshop a new report can only be born on one of the app's tables.
  const entities = workshop?.entityNames ? scopeEntityOptions(allEntities, workshop.entityNames) : allEntities;
  const entityChoices = useEntityOptions(entities).map((o) => ({ value: o.value, label: entityOptionText(o) }));
  const entityLabel = (name?: string) => allEntities.find((e) => e.name === name)?.label ?? name ?? "—";
  const [createOpen, setCreateOpen] = useState(false);
  const [cName, setCName] = useState("");
  const [cEntity, setCEntity] = useState("");
  const [cBusy, setCBusy] = useState(false);
  const [cErr, setCErr] = useState<string | null>(null);

  function openCreate() {
    setCName(""); setCErr(null);
    // Opened from a table's "Báo cáo" link: that table is the one the new report is about.
    setCEntity((prev) => entity || prev || entities[0]?.name || "");
    setCreateOpen(true);
  }

  const canCreate = Boolean(cName.trim() && cEntity && !cBusy);
  async function handleCreate() {
    if (!canCreate) return;
    setCBusy(true); setCErr(null);
    try {
      // Tabular needs ≥1 column (BE REPORT_CONFIG_INVALID) — same fallback rule as the panel:
      // first field of the entity, or "id".
      const flds = await listFields(cEntity);
      const created = await createReport({
        name: cName.trim(),
        entityName: cEntity,
        type: "tabular",
        config: buildReportConfig({
          type: "tabular", columns: [], metric: "count", fields: flds,
          fallbackColumn: flds[0]?.name ?? "id",
        }),
        isActive: true,
      });
      await qc.invalidateQueries({ queryKey: qk.reports() });
      // A delivered workspace creates the report in the DRAFT: open it on the draft or it is a 404 for the live read.
      if (onOpen) onOpen(created.id);
      else router.push(`${reportHref(created.id)}${workshop ? "&" : "?"}config=1${hasDraftChange(created.draftState) ? "&draft=1" : ""}`);
    } catch (e) {
      setCErr(e instanceof Error ? e.message : t("page.reports.createError"));
      setCBusy(false);
    }
  }

  const term = q.trim().toLowerCase();
  const byQuery: ReportSummary[] = (data ?? []).filter((r) => {
    const matchType = (typeFilter === "all" || r.type === typeFilter) && (!entity || r.entityName === entity);
    const matchQ = !term
      || r.name.toLowerCase().includes(term)
      || (r.entityName ?? "").toLowerCase().includes(term)
      || entityLabel(r.entityName).toLowerCase().includes(term);
    return matchType && matchQ;
  });
  const items = showAll ? byQuery : inEntityScope(byQuery, workshop?.entityNames ?? null, (r) => r.entityName);
  const hiddenCount = byQuery.length - items.length;

  const columns: ColumnDef<ReportSummary>[] = [
    {
      id: "name",
      header: t("page.reports.col.name"),
      accessorKey: "name",
      cell: ({ row }) => (
        <>
          <Link href={draftView.keep(reportHref(row.original.id))}>{row.original.name}</Link>
          {hasDraftChange(row.original.draftState) && (
            <Badge theme={row.original.draftState === "deleted" ? "warning" : undefined} className="slds-m-left_x-small">
              {t(`draft.state.${row.original.draftState}` as I18nKey)}
            </Badge>
          )}
        </>
      ),
    },
    { id: "entity", header: t("page.reports.col.entity"), accessorFn: (r) => entityLabel(r.entityName) },
    { id: "type", header: t("page.reports.col.type"), accessorFn: (r) => reportTypeLabel(r.type) },
    {
      id: "status",
      header: t("page.reports.col.status"),
      accessorFn: (r) => t(r.isActive !== false ? "page.reports.status.active" : "page.reports.status.off"),
    },
  ];

  const filtered = Boolean(term) || typeFilter !== "all";
  // Only the table filter is on: say "this table has none" and keep the create button.
  const tableOnly = Boolean(entity) && !filtered;
  let empty: React.ReactNode = null;
  if (isError) {
    empty = (
      <div className="slds-p-around_medium">
        <ScopedNotification theme="error">
          {error instanceof Error ? error.message : t("page.reports.loadError")}
        </ScopedNotification>
      </div>
    );
  } else if (!isLoading && items.length === 0) {
    empty = (
      <EmptyState
        className="slds-p-around_large"
        icon={FileBarChart2}
        title={t(filtered ? "page.reports.emptyFiltered" : tableOnly ? "page.reports.emptyTable" : "page.reports.empty")}
        description={t(
          filtered ? "page.reports.emptyFilteredDesc" : isAdmin ? "page.reports.emptyAdmin" : "page.reports.emptyUser",
        )}
        action={!filtered && isAdmin ? (
          <div className="slds-m-top_medium">
            <Button variant="brand" onClick={openCreate}>{t("page.reports.create")}</Button>
          </div>
        ) : undefined}
      />
    );
  }

  const switchLabel = t("page.reports.switchView");
  const viewTitle = typeFilter === "all" ? t("page.reports.view.all") : reportTypeLabel(typeFilter);

  return (
    <div className="flex min-h-0 flex-1 flex-col p-4">
      <ObjectHomePage
        icon={{ glyph: FileBarChart2, name: "report" }}
        label={t("page.reports.label")}
        title={viewTitle}
        titleMenu={
          <Menu
            trigger={
              <button type="button" className="slds-button slds-button_icon slds-button_icon-small" title={switchLabel}>
                <ChevronDown aria-hidden="true" className="slds-button__icon slds-icon_x-small nx-lucide" />
                <span className="slds-assistive-text">{switchLabel}</span>
              </button>
            }
          >
            {(["all", ...REPORT_TYPES] as ReportTypeFilter[]).map((v) => (
              <MenuCheckboxItem
                key={v}
                label={v === "all" ? t("page.reports.view.all") : reportTypeLabel(v)}
                checked={typeFilter === v}
                onCheckedChange={() => setTypeFilter(v)}
              />
            ))}
          </Menu>
        }
        actions={isAdmin ? [{ label: t("page.reports.create"), onSelect: openCreate }] : undefined}
        controls={[<DraftViewButton key="draft-view" view={draftView} />]}
        meta={isLoading ? "…" : tf("page.reports.count", { n: items.length })}
        search={{ value: q, onChange: setQ, placeholder: t("page.reports.search") }}
        empty={empty}
      >
        {entity && (
          <p className="slds-p-horizontal_medium slds-p-bottom_x-small slds-text-body_small">
            {tf("page.reports.tableOnly", { name: entityLabel(entity) })}
            {clearEntityHref && <> <Link href={clearEntityHref}>{t("page.reports.tableAll")}</Link></>}
          </p>
        )}
        {workshop?.entityNames && (
          <ScopeHiddenNote hidden={hiddenCount} noun="reports" showAll={showAll} onToggle={() => setShowAll((v) => !v)} />
        )}
        <DataTable
          label={t("page.reports.grid")}
          columns={columns}
          data={items}
          getRowId={(r) => r.id}
          sortable
          loading={isLoading}
          rowActions={(r) => (
            <MenuItem label={t("page.reports.run")} onSelect={() => (onOpen ? onOpen(r.id) : router.push(draftView.keep(reportHref(r.id))))} />
          )}
        />
      </ObjectHomePage>

      {/* Create dialog — name + entity; type defaults to tabular, the viewer's panel does the rest */}
      <Modal
        open={createOpen}
        onOpenChange={(o) => { if (!cBusy) { setCreateOpen(o); if (!o) setCErr(null); } }}
        title={t("page.reports.create")}
        tagline={t("page.reports.createTagline")}
        footer={
          <>
            <Button onClick={() => setCreateOpen(false)} disabled={cBusy}>{t("action.cancel")}</Button>
            <Button variant="brand" onClick={handleCreate} disabled={!canCreate}>
              {cBusy ? t("page.reports.creating") : t("page.reports.createSubmit")}
            </Button>
          </>
        }
      >
        <div className="slds-form flex flex-col gap-3">
          <Input
            autoFocus
            label={t("page.reports.createName")}
            value={cName}
            placeholder={t("page.reports.createNamePlaceholder")}
            onChange={(e) => setCName(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter") handleCreate(); }}
          />
          <Select
            label={t("page.reports.createEntity")}
            value={cEntity}
            onChange={(e) => setCEntity(e.target.value)}
            placeholder={entities.length === 0 ? t("page.reports.createNoEntity") : false}
            options={entityChoices}
          />
          {cErr && <ScopedNotification theme="error">{cErr}</ScopedNotification>}
        </div>
      </Modal>
    </div>
  );
}
