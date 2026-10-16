"use client";

// The stage of the list screen (specs/075 US5, T100): the REAL customer list page (ObjectHomePage + DataTable) in the
// customer frame, with the draft's columns built by the same pipeline the live list uses (buildListGridColumns +
// toDataTableColumns) and up to five real records. A header click selects the column (it does not sort). Kanban and
// Calendar are not mounted: when declared, the stage says in one line each what the customer will get.
import { useMemo } from "react";
import { Table } from "lucide-react";
import type { ColumnDef } from "@tanstack/react-table";
import { DataTable } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { ObjectHomePage } from "@/components/slds/templates/ObjectHomePage";
import { CustomerPreviewFrame } from "@/components/studio/CustomerPreviewFrame";
import { buildListGridColumns, toDataTableColumns } from "@/components/layout/listColumns";
import type { ApiRecord, FieldMeta } from "@/lib/api";
import { columnsForField, pickKanbanField } from "@/lib/kanbanModel";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import type { ListDraft } from "@/lib/screenEditorModel";

export interface ListCanvasProps {
  tableLabel: string;
  fields: FieldMeta[];
  draft: ListDraft;
  records: ApiRecord[];
  appName: string;
  accentColor?: string;
  logoUrl?: string;
  selected: string | null;
  onSelect: (column: string) => void;
}

export function ListCanvas(p: ListCanvasProps) {
  const { draft } = p;
  const byName = useMemo(() => Object.fromEntries(p.fields.map((f) => [f.name, f])), [p.fields]);
  const columns = useMemo(() => {
    const layoutCols = draft.cols.filter((c) => byName[c]).map((field) => ({ field, labelOverride: draft.labels[field] }));
    return toDataTableColumns(buildListGridColumns(layoutCols, byName, p.fields)).map((c) => {
      const title = typeof c.header === "string" ? c.header : String(c.id);
      return {
        ...c,
        enableSorting: false,
        header: () => (
          <button
            type="button"
            className={p.selected === c.id ? "slds-button slds-text-link" : "slds-button"}
            aria-pressed={p.selected === c.id}
            aria-label={tf("studio.build.screens.list.pickColumn", { name: title })}
            onClick={() => p.onSelect(String(c.id))}
          >
            {title}
          </button>
        ),
      } as ColumnDef<ApiRecord>;
    });
  }, [draft.cols, draft.labels, byName, p]);
  const kanban = draft.kanban ? byName[draft.kanban] : undefined;
  const kanbanField = kanban && kanban.fieldType === "picklist" ? pickKanbanField(p.fields, draft.kanban) : undefined;
  const lanes = kanbanField ? columnsForField(kanbanField).filter((c) => !c.isEmpty).map((c) => c.label) : [];
  const calendar = draft.calendar ? byName[draft.calendar] : undefined;
  const sortName = t(`studio.build.screens.list.sort.${draft.sortField}` as I18nKey);

  return (
    <CustomerPreviewFrame appName={p.appName} accentColor={p.accentColor} logoUrl={p.logoUrl} caption={t(p.records.length ? "studio.build.screens.stage.captionRecord" : "studio.build.screens.stage.captionEmpty")}>
      <ObjectHomePage
        icon={{ glyph: Table, name: "list" }}
        label={p.tableLabel}
        title={tf("studio.build.screens.list.stageTitle", { bang: p.tableLabel })}
        meta={tf("studio.build.screens.list.sortNote", { field: sortName.toLowerCase(), order: t(`studio.build.screens.list.sort.${draft.sortOrder}` as I18nKey).toLowerCase() })}
        empty={
          columns.length === 0 ? (
            <EmptyState plain title={t("studio.build.screens.list.noCols")} description={t("studio.build.screens.list.noColsDesc")} />
          ) : undefined
        }
      >
        <DataTable<ApiRecord>
          label={tf("studio.build.screens.list.stageLabel", { bang: p.tableLabel })}
          columns={columns}
          data={p.records}
          getRowId={(r) => r.id}
          empty={<div className="slds-p-around_medium slds-text-align_center slds-text-color_weak">{t("studio.build.screens.list.noRows")}</div>}
        />
      </ObjectHomePage>
      {(kanbanField || calendar) && (
        <ul className="slds-m-top_small slds-text-body_small slds-text-color_weak">
          {kanbanField && (
            <li>
              <span>{tf("studio.build.screens.list.strip.kanban", { field: kanbanField.label || kanbanField.name })}</span>
              {lanes.length > 0 && <span>{`. ${tf("studio.build.screens.list.strip.lanes", { lanes: lanes.join(", ") })}`}</span>}
            </li>
          )}
          {calendar && <li>{tf("studio.build.screens.list.strip.calendar", { field: calendar.label || calendar.name })}</li>}
        </ul>
      )}
    </CustomerPreviewFrame>
  );
}
