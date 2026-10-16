"use client";

// SectionGrid — render layout.sections[] (doc §4.1 + §2.2) on SLDS 2.264.1: each titled section is
// an `expandable-section`, its body the record-detail form grid
//   div.slds-form[role=list] > div.slds-form__row > div.slds-form__item[role=listitem] × n
// (blueprint form-element/record-detail). Columns = section.columns (1–4, mobile always 1): the row
// becomes a CSS grid (layout utility) so cell.colspan / cell.fullWidth can span columns, which
// SLDS's equal-width flex row cannot. Every cell goes through CellRenderer; a spacer keeps its slot.
//
// SectionFields is one section's body — RecordFormPage (the /new and /edit form) draws its own
// expandable sections around it; SectionGrid is the whole stack (record detail, layout preview).
//
// Tailwind 4 scans STATIC classes ⇒ never interpolate `col-span-${n}`; fixed class maps below.

import type { ReactElement } from "react";
import { clsx } from "clsx";
import type { LayoutCell, LayoutSection, FieldMeta } from "@/lib/api";
import { cellField, isSpacerCell } from "@/lib/api";
import { clearedDependents } from "@/lib/dependentPicklist";
import { ExpandableSection } from "@/components/slds/ExpandableSection";
import { CellRenderer, type CellMode } from "./CellRenderer";

export interface SectionGridProps {
  sections: LayoutSection[];
  mode: CellMode;
  fieldsByName: Record<string, FieldMeta>;
  // Current values keyed by field name (form state in edit, record.data in detail).
  values: Record<string, unknown>;
  // edit-only: per-field change + error lookups.
  onChange?: (field: string, v: unknown) => void;
  errors?: Record<string, string | undefined>;
  // Studio screen editor: wraps each rendered cell INSIDE its grid item (same row, colspan, column), so the editor
  // never draws a second grid. Default = the cell as-is.
  renderCell?: (cell: LayoutCell, index: number, node: ReactElement) => ReactElement;
}

export type SectionFieldsProps = Omit<SectionGridProps, "sections"> & { section: LayoutSection; sectionIndex?: number };

// Section column count → grid-cols class (md+; mobile stays single column).
// W5: columns widened {1,2,3} → {1,2,3,4} (contract §2.3).
const COLS_CLASS: Record<1 | 2 | 3 | 4, string> = {
  1: "md:grid-cols-1",
  2: "md:grid-cols-2",
  3: "md:grid-cols-3",
  4: "md:grid-cols-4",
};

// Cell colspan → col-span class (capped at the section width by the caller).
const SPAN_CLASS: Record<number, string> = {
  1: "md:col-span-1",
  2: "md:col-span-2",
  3: "md:col-span-3",
  4: "md:col-span-4",
};

/** Field names a section renders (spacers skipped) — e.g. to hold a section open while one errors. */
export function sectionFieldNames(section: LayoutSection): string[] {
  return section.fields.flatMap((cell) => {
    if (isSpacerCell(cell)) return [];
    const fc = cellField(cell);
    return fc ? [fc.field] : [];
  });
}

export function SectionFields({
  section,
  sectionIndex = 0,
  mode,
  fieldsByName,
  values,
  onChange,
  errors,
  renderCell,
}: SectionFieldsProps): ReactElement {
  const cols = (section.columns ?? 1) as 1 | 2 | 3 | 4;
  // specs/052: a new value in a controlling field blanks the dependent picklists it no longer
  // allows — wherever they sit on the form (another section included); an edit then sends them null.
  const change = onChange && ((field: string, v: unknown) => {
    onChange(field, v);
    for (const [child, blank] of Object.entries(clearedDependents(fieldsByName, { ...values, [field]: v }, field))) {
      onChange(child, blank);
    }
  });
  return (
    <div className="slds-form" role="list">
      {/* min-w-0 on items: .slds-form__item has min-width:280px, which would overflow 3–4 grid columns. */}
      <div className={clsx("slds-form__row grid grid-cols-1", COLS_CLASS[cols])}>
        {section.fields.map((cell, ci) => {
          // Span: fullWidth → whole row; colspan → n columns (clamped to section width).
          let spanClass = "";
          const fc = isSpacerCell(cell) ? null : cellField(cell);
          if (fc && typeof cell === "object") {
            if ("fullWidth" in cell && cell.fullWidth) {
              spanClass = "md:col-span-full";
            } else if ("colspan" in cell && typeof cell.colspan === "number") {
              spanClass = SPAN_CLASS[Math.min(Math.max(cell.colspan, 1), cols)] ?? "";
            }
          }
          const fieldName = fc?.field;
          const node = (
            <CellRenderer
              cell={cell}
              mode={mode}
              fieldsByName={fieldsByName}
              value={fieldName ? values[fieldName] : undefined}
              values={values}
              onChange={fieldName && change ? (v) => change(fieldName, v) : undefined}
              error={fieldName ? errors?.[fieldName] : undefined}
            />
          );
          return (
            <div key={fieldName ?? `cell-${sectionIndex}-${ci}`} className={clsx("slds-form__item min-w-0", spanClass)} role="listitem">
              {renderCell ? renderCell(cell, ci, node) : node}
            </div>
          );
        })}
      </div>
    </div>
  );
}

export function SectionGrid({ sections, ...rest }: SectionGridProps): ReactElement {
  return (
    <div>
      {sections.map((section, si) => {
        const body = <SectionFields section={section} sectionIndex={si} {...rest} />;
        const key = section.title ?? `section-${si}`;
        return section.title ? (
          <ExpandableSection key={key} title={section.title}>
            {body}
          </ExpandableSection>
        ) : (
          <div key={key} className="slds-section slds-is-open">
            {body}
          </div>
        );
      })}
    </div>
  );
}
