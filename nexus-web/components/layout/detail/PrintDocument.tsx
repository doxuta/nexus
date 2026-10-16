"use client";

// Print-only document — MOT-XUONG W3 #6 (in / xuất chứng từ).
// `hidden print:block`: not laid out on screen at all, the ONLY visible thing under @media print
// once the record page above goes print:hidden. The business fields reuse DetailSections in its
// non-collapsible form (shaded section titles, always open); system fields are left out on purpose —
// created_by/modified_by are internal user ids and never belong on a document handed to a customer.
// Child tables are plain SLDS bordered tables (blueprint data-tables/base, no row actions).
import { formatDateTime } from "@/lib/format";
import { metaToSpec } from "@/lib/fieldSpec";
import { t } from "@/lib/i18n";
import type { AdminRelationship, ApiRecord, FieldMeta, LayoutSection } from "@/lib/api";
import { FieldDisplay } from "@/components/fields/display";
import { DetailSections } from "./DetailSections";

// One resolved 1-N child table ready to print (e.g. a quote's crm_quote_lines).
export interface PrintChildGroup {
  rel: AdminRelationship;
  label: string;
  fields: FieldMeta[];
  records: ApiRecord[];
}

// Bulky/non-scalar types that do not read well inline (shared with the header highlights).
export const HIGHLIGHT_SKIP_TYPES = new Set(["rich_text", "file", "image", "composite"]);

export function PrintDocument({
  entityLabel,
  title,
  orgName,
  printedAt,
  sections,
  fieldsByName,
  record,
  childGroups,
}: {
  entityLabel: string;
  title: string;
  orgName?: string;
  printedAt: Date;
  sections: LayoutSection[];
  fieldsByName: Record<string, FieldMeta>;
  record: ApiRecord;
  childGroups: PrintChildGroup[];
}) {
  return (
    <div className="hidden print:block">
      <header className="slds-p-bottom_medium slds-m-bottom_medium slds-border_bottom">
        {orgName && <p className="slds-text-heading_small">{orgName}</p>}
        <h1 className="slds-text-heading_large">{entityLabel}</h1>
        <p className="slds-text-heading_small">{title}</p>
        <p className="slds-text-body_small slds-m-top_xx-small">
          {t("page.record.print.printedAt").replace("{date}", formatDateTime(printedAt))}
        </p>
      </header>

      <DetailSections sections={sections} fieldsByName={fieldsByName} values={record.data} collapsible={false} />

      {childGroups.map((group) => (
        <PrintChildTable key={group.rel.id} group={group} />
      ))}
    </div>
  );
}

// One child relation's rows. Column choice: an admin-configured viewConfig.columns wins (ENG-RELVIEW,
// same source of truth as the "Quan hệ" tab); absent that (true for every builtin pack relation
// today), every child field EXCEPT the FK back to this parent and bulky types — a static document
// has no "click through", so it needs the actual values (quantity/unit price/…), not just a name.
function PrintChildTable({ group }: { group: PrintChildGroup }) {
  const { rel, label, fields, records } = group;
  const fkField = (rel.childFkColumn ?? "").replace(/_id$/, "");
  const configuredCols = rel.viewConfig?.columns;
  const cols = configuredCols && configuredCols.length > 0
    ? configuredCols
    : fields.filter((f) => f.name !== fkField && !HIGHLIGHT_SKIP_TYPES.has(f.fieldType)).map((f) => f.name);
  const fieldsByName = Object.fromEntries(fields.map((f) => [f.name, f]));

  if (cols.length === 0) return null;

  return (
    <section className="slds-m-top_large break-inside-avoid">
      <h2 className="slds-text-heading_small slds-m-bottom_x-small">{label}</h2>
      <table className="slds-table slds-table_bordered slds-table_cell-buffer slds-no-row-hover">
        <thead>
          <tr className="slds-line-height_reset">
            {cols.map((c) => (
              <th key={c} scope="col">
                {fieldsByName[c]?.label ?? c}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {records.map((r) => (
            <tr key={r.id}>
              {cols.map((c) => {
                const meta = fieldsByName[c];
                const value = r.data?.[c];
                return (
                  <td key={c}>
                    {meta ? <FieldDisplay field={metaToSpec(meta)} value={value} /> : (value == null ? "—" : String(value))}
                  </td>
                );
              })}
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}
