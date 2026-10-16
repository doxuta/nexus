"use client";

// Vận hành › Dữ liệu: the table of tables (specs/078 lát u7-du-lieu-danh-sach-bang, FR-050..FR-053).
// A flat table, one row per business table; the name is the link to its records. No initial-letter grid, no counters
// the engine cannot answer yet (the "Bản ghi" figure comes from useRecordCounts, one usage read, never a call per table).
import type { ColumnDef } from "@tanstack/react-table";
import Link from "next/link";
import { useMemo, useState } from "react";
import { DataTable, fill } from "@/components/slds/DataTable";
import { Input } from "@/components/slds/Input";
import { QueryState } from "@/components/studio/QueryState";
import type { EntityMeta } from "@/lib/api";
import { t } from "@/lib/i18n";
import { mergeRecordCounts, useProjectEntities, useRecordCounts } from "@/lib/operateData";
import { studioHref } from "@/lib/studioRoutes";

export function DataTablesPage({ slug }: { slug: string }) {
  const entities = useProjectEntities(slug);
  const usage = useRecordCounts(slug);
  const counts = useMemo(() => (usage.data ? mergeRecordCounts(entities.data ?? [], [...usage.data].map(([entity, count]) => ({ entity, label: "", count }))) : null), [usage.data, entities.data]);
  const total = counts ? [...counts.values()].reduce((a, b) => a + b, 0) : 0;
  const at = usage.computedAt ? new Date(usage.computedAt) : null;
  const time = at && !Number.isNaN(at.getTime()) ? at.toLocaleTimeString("vi-VN", { hour: "2-digit", minute: "2-digit", hour12: false }) : null;
  const [q, setQ] = useState("");
  const list = useMemo(() => entities.data ?? [], [entities.data]);
  const needle = q.trim().toLowerCase();
  const shown = needle ? list.filter((e) => e.label.toLowerCase().includes(needle) || e.name.toLowerCase().includes(needle)) : list;

  const columns = useMemo<ColumnDef<EntityMeta>[]>(
    () => [
      {
        id: "name",
        header: t("studio.operate.data.col.name"),
        accessorFn: (e) => e.label,
        cell: ({ row: { original: e } }) => (
          <Link href={studioHref(slug, "van-hanh/du-lieu/:bang", { bang: e.name })}>
            <strong>{e.label}</strong>
          </Link>
        ),
      },
      {
        id: "records",
        header: t("studio.operate.data.col.records"),
        accessorFn: (e) => counts?.get(e.name) ?? -1,
        meta: { numeric: true },
        cell: ({ row: { original: e } }) => {
          const n = counts?.get(e.name);
          return n === undefined ? null : n.toLocaleString("vi-VN");
        },
      },
      {
        id: "actions",
        header: () => <span className="slds-assistive-text">{t("studio.operate.data.col.actions")}</span>,
        enableSorting: false,
        cell: ({ row: { original: e } }) => (
          <span className="slds-grid slds-grid_align-end">
            <Link
              href={studioHref(slug, "van-hanh/du-lieu/:bang/nhap", { bang: e.name })}
              aria-label={fill(t("studio.operate.data.importOf"), { name: e.label })}
            >
              {t("studio.operate.data.import")}
            </Link>
            <Link
              className="slds-m-left_small"
              href={studioHref(slug, "van-hanh/du-lieu/:bang/thung-rac", { bang: e.name })}
              aria-label={fill(t("studio.operate.data.trashOf"), { name: e.label })}
            >
              {t("studio.operate.data.trash")}
            </Link>
          </span>
        ),
      },
    ],
    [slug, counts],
  );

  return (
    <div className="slds-p-around_medium">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="nx-lede">{entities.data && (
            <>
              <b className="tabular-nums">{fill(t("studio.operate.data.lede"), { n: list.length })}</b>
              {counts && time && <span className="tabular-nums">{fill(t("studio.operate.data.ledeRecords"), { total: total.toLocaleString("vi-VN"), time })}</span>}
              {usage.isError && (
                <>
                  {" · "}
                  <span className="slds-text-color_weak">{t("studio.operate.data.recordsError")}</span>
                </>
              )}
            </>
          )}
        </p>
        <div className="flex items-center gap-3">
          {list.length > 0 && (
            <Input type="search" label={t("studio.operate.data.search")} labelHidden placeholder={t("studio.operate.data.search")} value={q} onChange={(e) => setQ(e.target.value)} />
          )}
          <Link href={studioHref(slug, "dung/du-lieu")}>{t("studio.operate.data.more")}</Link>
        </div>
      </div>
      <div className="slds-m-top_medium">
        <QueryState
          query={entities}
          empty={
            <p className="slds-p-around_medium slds-text-color_weak">
              {t("studio.operate.data.empty")} <Link href={studioHref(slug, "dung/du-lieu")}>{t("studio.operate.data.emptyLink")}</Link>
            </p>
          }
        >
          {shown.length === 0 ? (
            <p className="slds-p-around_medium slds-text-color_weak">{fill(t("studio.operate.data.noMatch"), { q: q.trim() })}</p>
          ) : (
            <DataTable label={t("studio.operate.data.table")} columns={columns} data={shown} getRowId={(e) => e.name} stacked />
          )}
        </QueryState>
      </div>
    </div>
  );
}
