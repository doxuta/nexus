"use client";

// BulkFieldTable (specs/075 FR-070/073, T125): list-mode stage. One row a field: tick + name · Kiểu · Khung trên form
// (Select, "Không đặt" takes it off the form) · Bắt buộc (switch). The Select and the switch edit the row at once; the
// ticks feed BulkBar. Below 768px the DataTable stacks each row (slds-max-medium-table_stacked). All edits go to the
// editor's one local draft — this component owns no state but the shape of the rows.
import { useMemo } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { DataTable } from "@/components/slds/DataTable";
import { Select } from "@/components/slds/Select";
import { Toggle } from "@/components/slds/Toggle";
import type { FieldMeta } from "@/lib/api";
import type { DraftSection } from "@/lib/layoutDraft";
import { t, tf } from "@/lib/i18n";
import { typeLabelOf } from "./FormCanvas";

interface Row {
  name: string;
  label: string;
  type: string;
  sectionId: string;
  required: boolean;
  sections: Array<{ id: string; title: string }>;
  // the handlers travel with the row: the columns below close over nothing, so they are built once (see the note there)
  disabled?: boolean;
  onSection: BulkFieldTableProps["onSection"];
  onRequired: BulkFieldTableProps["onRequired"];
}

export const NO_SECTION = "";

export interface BulkFieldTableProps {
  fields: FieldMeta[];
  draft: DraftSection[];
  selected: string[];
  /** Record page: no required switch. */
  detail?: boolean;
  disabled?: boolean;
  onSelect: (names: string[]) => void;
  /** `sectionId` NO_SECTION = take it off the form. */
  onSection: (name: string, sectionId: string) => void;
  onRequired: (name: string, on: boolean) => void;
}

export const sectionTitle = (s: DraftSection): string => s.title.trim() || t("studio.build.screens.stage.sectionUntitled");

export function BulkFieldTable({ fields, draft, selected, detail, disabled, onSelect, onSection, onRequired }: BulkFieldTableProps) {
  const rows = useMemo<Row[]>(() => {
    const sections = draft.map((s) => ({ id: s.id, title: sectionTitle(s) }));
    const where = new Map<string, string>();
    for (const s of draft) for (const c of s.cells) if (c.kind === "field") where.set(c.field, s.id);
    return fields.map((f) => ({
      name: f.name,
      label: f.label || f.name,
      type: typeLabelOf(f),
      sectionId: where.get(f.name) ?? NO_SECTION,
      required: !!f.isRequired,
      sections,
      disabled,
      onSection,
      onRequired,
    }));
  }, [fields, draft, disabled, onSection, onRequired]);

  // A column's `cell` is rendered as a component: a new function each render would remount the Select and the switch
  // (focus lost after every edit). So the columns depend on `detail` only and read everything else from the row.
  const columns = useMemo<ColumnDef<Row>[]>(() => {
    const cols: ColumnDef<Row>[] = [
      { id: "label", accessorKey: "label", header: t("studio.build.screens.bulk.colField"), enableSorting: false },
      { id: "type", accessorKey: "type", header: t("studio.build.screens.bulk.colType"), enableSorting: false },
      {
        id: "section",
        header: t("studio.build.screens.bulk.colSection"),
        enableSorting: false,
        cell: ({ row }) => (
          <Select
            label={tf("studio.build.screens.bulk.sectionOf", { name: row.original.label })}
            labelHidden
            placeholder={false}
            disabled={row.original.disabled}
            value={row.original.sectionId}
            onChange={(e) => row.original.onSection(row.original.name, e.target.value)}
          >
            <option value={NO_SECTION}>{t("studio.build.screens.bulk.none")}</option>
            {row.original.sections.map((s) => <option key={s.id} value={s.id}>{s.title}</option>)}
          </Select>
        ),
      },
    ];
    if (!detail) {
      cols.push({
        id: "required",
        header: t("studio.build.screens.bulk.colRequired"),
        enableSorting: false,
        cell: ({ row }) => (
          <Toggle
            label={<span className="slds-assistive-text">{tf("studio.build.screens.bulk.requiredOf", { name: row.original.label })}</span>}
            disabled={row.original.disabled}
            checked={row.original.required}
            onChange={(e) => row.original.onRequired(row.original.name, e.target.checked)}
          />
        ),
      });
    }
    return cols;
  }, [detail]);

  return (
    <DataTable
      label={t("studio.build.screens.bulk.table")}
      columns={columns}
      data={rows}
      getRowId={(r) => r.name}
      rowHeader="label"
      selectable
      stacked
      maxHeight="70vh"
      rowSelection={Object.fromEntries(selected.map((n) => [n, true]))}
      onRowSelectionChange={(sel) => onSelect(Object.keys(sel).filter((k) => sel[k]))}
      empty={<p className="slds-p-around_medium slds-text-color_weak">{t("studio.build.screens.bulk.empty")}</p>}
    />
  );
}
