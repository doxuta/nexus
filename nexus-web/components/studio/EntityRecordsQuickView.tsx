"use client";

// EntityRecordsQuickView — "Dữ liệu" tab of the Entity Hub (NEXUS-MOT-XUONG-2026-07-29.md §2b).
// A quick READ-ONLY glance at the table's real rows without leaving the workshop — the exact gap
// the spec names: "người dùng thấy NGAY bảng có dữ liệu hay không mà không phải sang mặt khách".
// Reuses the SAME metadata → columns pipeline the real App-face list uses (buildListGridColumns,
// fed by listRecordsPage) so what a maker sees here matches what the customer sees.
// No write surface on purpose (no row-open, no bulk bar, no filters) — that richer experience
// lives one click away via "Mở đầy đủ ở App khách"; this tab is a glance, not a second list.
// SLDS 2.264.1: a `cards` related-list card holding a read-only `data-tables/advanced` grid;
// empty = `illustration`, load error = `scoped-notifications`.

import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { Inbox } from "lucide-react";
import Link from "next/link";
import { getLayout, listFields, listRecordsPage, type ApiRecord, type LayoutListColumn } from "@/lib/api";
import { qk } from "@/lib/query/keys";
import { t } from "@/lib/i18n";
import { buildListGridColumns } from "@/components/layout/listColumns";
import { Card } from "@/components/slds/Card";
import { DataTable, fill } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { buttonClassName } from "@/components/slds/Button";

const PREVIEW_LIMIT = 20;
// Mirrors LayoutBuilder's seedListState default (which fields make a reasonable list when none
// is declared) — a safety net only: the engine always serves a generated default list layout for
// a live entity, so `layout.views.list.columns` normally already carries this exact choice.
const LIST_EXCLUDED_TYPES = new Set(["relation", "polymorphic_relation", "rich_text", "multi_select"]);

export function EntityRecordsQuickView({ entityName, onAskAssistant }: { entityName: string; onAskAssistant?: (recordId: string) => void }) {
  const fieldsQ = useQuery({ queryKey: qk.fields(entityName), queryFn: () => listFields(entityName), staleTime: 60_000 });
  const layoutQ = useQuery({ queryKey: qk.layout(entityName), queryFn: () => getLayout(entityName), staleTime: 60_000 });
  const recordsQ = useQuery({
    queryKey: qk.records(entityName, `limit=${PREVIEW_LIMIT}`),
    queryFn: () => listRecordsPage(entityName, `limit=${PREVIEW_LIMIT}`),
    staleTime: 15_000,
  });

  const fields = useMemo(() => fieldsQ.data ?? [], [fieldsQ.data]);
  const listColumns: LayoutListColumn[] = useMemo(() => {
    const declared = layoutQ.data?.views.list?.columns;
    if (declared && declared.length > 0) return declared;
    return fields.filter((f) => !LIST_EXCLUDED_TYPES.has(f.fieldType)).slice(0, 5).map((f) => ({ field: f.name }));
  }, [layoutQ.data, fields]);
  // The shared list pipeline's descriptors (header, width, numeric, FieldDisplay cell) as
  // data-table columns; sorting stays off — a 20-row preview sorted client-side would lie about order.
  const columns = useMemo<ColumnDef<ApiRecord>[]>(() => {
    const fieldsByName = Object.fromEntries(fields.map((f) => [f.name, f]));
    const cols: ColumnDef<ApiRecord>[] = buildListGridColumns(listColumns, fieldsByName, fields).map((c) => ({
      id: c.id,
      header: typeof c.header === "string" ? c.header : c.id,
      size: c.width,
      enableSorting: false,
      meta: { numeric: c.numeric },
      accessorFn: (r: ApiRecord) => r.data[c.id],
      cell: ({ row }) => c.cell(row.original),
    }));
    // Last column: ask the docked assistant about this row (sends the row ID only, never its values).
    if (onAskAssistant) {
      cols.push({
        id: "__ask",
        header: () => <span className="slds-assistive-text">{t("studio.assistant.record.ask")}</span>,
        enableSorting: false,
        cell: ({ row }) => (
          <button
            type="button"
            className="slds-button"
            aria-label={t("studio.assistant.record.askLabel")}
            onClick={() => onAskAssistant(row.original.id)}
          >
            {t("studio.assistant.record.ask")}
          </button>
        ),
      });
    }
    return cols;
  }, [listColumns, fields, onAskAssistant]);

  const loading = fieldsQ.isLoading || layoutQ.isLoading || recordsQ.isLoading;
  const error = fieldsQ.error ?? layoutQ.error ?? recordsQ.error;
  const records = recordsQ.data?.records ?? [];
  const total = recordsQ.data?.pagination.total ?? 0;

  const meta = loading
    ? t("page.entityRecords.loading")
    : fill(t(total > PREVIEW_LIMIT ? "page.entityRecords.countPreview" : "page.entityRecords.count"), {
      n: total.toLocaleString("vi-VN"),
      limit: PREVIEW_LIMIT,
    });

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto p-4">
      <Card
        title={t("page.entityRecords.title")}
        padded={false}
        actions={
          <Link href={`/${entityName}`} className={buttonClassName("neutral")}>
            {t("page.entityRecords.openFull")}
          </Link>
        }
        footer={<span className="slds-text-body_small slds-text-color_weak">{meta}</span>}
      >
        {error ? (
          <div className="p-3">
            <ScopedNotification theme="error">{error.message}</ScopedNotification>
          </div>
        ) : (
          // Layout columns carry declared widths; wider than the card, the grid scrolls sideways.
          <div className="slds-scrollable_x">
            <DataTable
              label={fill(t("page.entityRecords.grid"), { entity: entityName })}
              columns={columns}
              data={records}
              loading={loading}
              empty={
                <EmptyState
                  className="p-6"
                  icon={Inbox}
                  title={t("page.entityRecords.emptyTitle")}
                  description={t("page.entityRecords.emptyDesc")}
                />
              }
            />
          </div>
        )}
      </Card>
    </div>
  );
}
