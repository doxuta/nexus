"use client";

// listColumns — the SHARED metadata→grid column pipeline for the dynamic record list.
//
// Extracted from LayoutList so BOTH the live list and the recycle bin render identical columns
// (layout order + declared widths + FieldDisplay cells) without forking the cell logic. Cell values
// go through FieldDisplay — the same read-mode renderer the detail view uses — so currency / date /
// relation / picklist all format correctly. No per-entity hard-coding: columns come from the
// layout's list view, field type/label from sys_fields metadata.
//
// Two consumers' shapes: buildListGridColumns returns the legacy DataGrid descriptor (the recycle
// bin and Studio's quick view still render components/ui/DataGrid), and toDataTableColumns adapts
// that same list to @tanstack ColumnDefs for the SLDS DataTable (components/slds/DataTable) that the
// live list renders since specs/038 T022. One pipeline, two adapters — never two cell renderers.

import { useQuery } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import type { MouseEvent } from "react";
import { listUserDirectory, type ApiRecord, type FieldMeta, type LayoutListColumn } from "@/lib/api";
import { t } from "@/lib/i18n";
import { recordLabel } from "@/lib/recordLabel";
import { FieldDisplay } from "@/components/fields/display";
import { metaToSpec } from "@/lib/fieldSpec";
import type { DataGridColumn } from "@/components/ui/DataGrid";

// OWNER-DIMENSION — column id of the optional "Chủ sở hữu" column. Deliberately the SAME spelling
// as the API's row key (`ApiRecord.ownerId`) and as the engine's reserved sort/filter key
// (?sort=ownerId, ?ownerId[eq]=…), so the id a caller sees on the grid is the id it can send back
// to the server without translation.
export const OWNER_COLUMN_ID = "ownerId";

/** Row-header link of the live list: the record name opens the record (SLDS data-table pattern). */
export interface RecordLink {
  href: (record: ApiRecord) => string;
  /** Plain left click → client navigation (guarded router); modified clicks keep the browser's. */
  onOpen?: (id: string) => void;
}

// Build DataGrid column descriptors from the layout's list columns + field metadata. The FIRST
// column renders the record label (falls back to recordLabel when its own value is blank); numeric
// types right-align. Identical output to LayoutList's former inline `gridColumns` memo.
//
// opts.showOwner appends the "Chủ sở hữu" column, OFF by default so every existing list stays
// byte-identical. It is an argument rather than a layout column on purpose: a list layout can
// never carry this column — the backend's validateListColumns rejects any column whose field is
// not a live sys_field (buildFieldAllowlist admits live non-system fields plus "id"), and
// pruneListLayout silently drops such a column at serve time. `{field:"ownerId"}` in a saved
// layout would 422 on save and vanish on read, so routing the owner through `columns` would be
// code nothing can ever reach.
// opts.link turns the first column into the record link (live list only; the recycle bin's rows
// are deleted and open nothing).
export function buildListGridColumns(
  columns: LayoutListColumn[],
  fieldsByName: Record<string, FieldMeta>,
  fields: FieldMeta[],
  opts?: { showOwner?: boolean; link?: RecordLink },
): DataGridColumn<ApiRecord>[] {
  const cols = columns.map((c, i) => {
    const meta = fieldsByName[c.field];
    return {
      id: c.field,
      header: c.labelOverride ?? meta?.label ?? c.field,
      width: c.width ?? 140,
      sortable: c.sortable,
      numeric: meta?.fieldType === "currency" || meta?.fieldType === "number",
      cell: (r: ApiRecord) => (
        <ListCell column={c} field={meta} record={r} isFirst={i === 0} allFields={fields} link={opts?.link} />
      ),
    };
  });

  if (opts?.showOwner) {
    cols.push({
      id: OWNER_COLUMN_ID,
      header: t("page.list.ownerColumn.header"),
      width: 160,
      // sortable:false even though the engine now accepts ?sort=ownerId: LayoutList's applySort
      // only fires for a column found in the LAYOUT's columns[], which (see above) can never hold
      // this one. A clickable header here would be a header that does nothing — wiring the sort
      // belongs with whichever caller opts the column in.
      sortable: false,
      numeric: false,
      cell: (r: ApiRecord) => <OwnerCell ownerId={r.ownerId} />,
    });
  }
  return cols;
}

// ponytail: the row-header column needs a width floor — with every other column at its declared
// px width a fixed-layout table would squeeze an unsized first column to nothing on a laptop.
const FIRST_COLUMN_MIN = 200;

// Adapt the descriptors to @tanstack ColumnDefs for components/slds/DataTable. Sorting is the
// SERVER's (DataTable manualSorting): `enableSorting` mirrors the served `sortable` flag and the
// accessor exists only because tanstack refuses to sort a column without one — it returns
// undefined, so the table never reads (or titles cells with) a raw value FieldDisplay would format.
export function toDataTableColumns(cols: DataGridColumn<ApiRecord>[]): ColumnDef<ApiRecord>[] {
  return cols.map((c, i) => ({
    id: c.id,
    header: typeof c.header === "string" ? c.header : c.id,
    size: i === 0 ? Math.max(c.width ?? 0, FIRST_COLUMN_MIN) : c.width,
    enableSorting: !!c.sortable,
    accessorFn: () => undefined,
    meta: { numeric: c.numeric },
    cell: ({ row }) => c.cell(row.original),
  }));
}

// OwnerCell renders the record's CURRENT owner — the value the ACL row filter actually enforces
// on, returned on every row as `ownerId` and, until now, shown on no list at all ("ai phụ trách"
// was invisible outside the detail screen).
//
// The id is resolved to a person through GET /users/directory — the same directory (and the same
// react-query key + staleTime) the detail view's "Chủ sở hữu" line uses, so: non-admins can
// resolve names too (/admin/users is admin-only), and one cached request serves every row of
// every list plus the detail screen instead of one request per cell.
export function OwnerCell({ ownerId }: { ownerId?: string }) {
  const { data: directory } = useQuery({
    queryKey: ["user-directory"],
    queryFn: () => listUserDirectory(),
    staleTime: 5 * 60 * 1000,
    retry: false,
    enabled: !!ownerId,
  });

  if (!ownerId) return <span className="slds-text-color_weak">—</span>;
  const u = directory?.find((x) => x.id === ownerId);
  // Never fabricate a name. A directory still loading, or an owner who is no longer an active
  // user, falls back to the SHORT id (the same #xxxxxxxx shorthand recordLabel uses) — honest
  // about having someone we cannot name, and readable in a 160px cell, unlike a raw uuid.
  return u ? (
    <div className="slds-truncate">{u.fullName?.trim() || u.email}</div>
  ) : (
    <div className="slds-truncate slds-text-color_weak">{`#${ownerId.slice(0, 8)}`}</div>
  );
}

// Plain primary click → the caller's client navigation; cmd/ctrl/shift/middle click keep the
// native link behaviour (new tab / window).
function openHandler(id: string, onOpen?: (id: string) => void) {
  return (e: MouseEvent<HTMLAnchorElement>) => {
    if (!onOpen || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    onOpen(id);
  };
}

// ── ListCell ─────────────────────────────────────────────────────────────────────
export function ListCell({
  column, field, record, isFirst, allFields, link,
}: {
  column: LayoutListColumn;
  field: FieldMeta | undefined;
  record: ApiRecord;
  isFirst: boolean;
  allFields: FieldMeta[];
  link?: RecordLink;
}) {
  const raw = record.data[column.field];
  const numeric = field?.fieldType === "currency" || field?.fieldType === "number";

  if (isFirst) {
    const own = raw != null && raw !== "" ? null : recordLabel(record, allFields);
    const label = own ?? <FieldDisplay field={metaToSpec(field ?? { name: column.field, fieldType: "text" })} value={raw} />;
    if (!link) return <div className="slds-truncate">{label}</div>;
    // A plain text/number name is the link text itself, so it takes the SLDS link colour; FieldDisplay
    // (which paints its own text colour) only for typed names (picklist, date…).
    const text = own ?? (typeof raw === "string" || typeof raw === "number" ? String(raw) : null);
    return (
      <a href={link.href(record)} title={text ?? undefined} onClick={openHandler(record.id, link.onOpen)}>
        {text ?? label}
      </a>
    );
  }

  return (
    <div className={numeric ? "slds-truncate slds-text-align_right" : "slds-truncate"}>
      {field ? (
        <FieldDisplay field={metaToSpec(field)} value={raw} />
      ) : (
        <span className="slds-text-color_weak">{raw == null ? "—" : String(raw)}</span>
      )}
    </div>
  );
}
