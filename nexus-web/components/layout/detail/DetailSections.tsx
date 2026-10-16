"use client";

// Record detail body — the "Tổng quan" tab and the printed document (specs/038 T023).
// Each layout section is an SLDS expandable section (blueprint expandable-section/base) around a
// record-detail form in view mode (blueprint form-element/record-detail):
//   div.slds-section > h3.slds-section__title > button · div.slds-section__content
//     > div.slds-form[role=list] > div.slds-form__row > div.slds-form__item[role=listitem]
//       > div.slds-form-element.slds-form-element_readonly.slds-form-element_stacked (FormElement)
// Rows are packed from the layout's own `columns` (1–4; the blueprint's two-column form is the
// columns=2 case). A fullWidth cell takes a row of its own (slds-form-element_1-col), a colspan
// widens its item by flex-grow, a spacer keeps its slot as the blueprint's empty listitem, and a
// short last row is padded with empty listitems so the columns stay aligned. `section.collapsed`
// (set in the layout builder) starts that section closed. Nothing is hard-coded per entity: cells,
// labels and help text come from the layout + sys_fields (mergeFieldSpec), values via FieldDisplay.
// A cell's per-view helpText is the blueprint's field-level help: an info button in
// `slds-form-element__icon` with the text in a tooltip (tooltips/base), not a help line under the
// value. The printed document (collapsible={false}) goes to customers and carries no help at all.
import clsx from "clsx";
import { Info } from "lucide-react";
import type { ReactNode } from "react";
import { cellField, isSpacerCell, type ApiRecord, type FieldMeta, type LayoutSection } from "@/lib/api";
import { mergeFieldSpec, metaToSpec } from "@/lib/fieldSpec";
import { t } from "@/lib/i18n";
import { FieldDisplay } from "@/components/fields/display";
import { Button } from "@/components/slds/Button";
import { ExpandableSection } from "@/components/slds/ExpandableSection";
import { FormElement } from "@/components/slds/FormElement";
import { Tooltip } from "@/components/slds/Tooltip";

// Blueprint form-element/record-detail "help" icon button, wrapped by tooltips/base.
function FieldHelp({ label, text }: { label: string; text: string }) {
  const name = `${t("page.record.detail.help")}: ${label}`;
  return (
    <Tooltip
      trigger={
        <button type="button" className="slds-button slds-button_icon">
          <Info aria-hidden="true" className="slds-button__icon nx-lucide" />
          <span className="slds-assistive-text">{name}</span>
        </button>
      }
    >
      {text}
    </Tooltip>
  );
}

// A cell spanning n columns grows n shares (.slds-form__item is `flex: 1 1 0%`). Static map:
// Tailwind only emits class names it can read literally.
const GROW: Record<number, string> = { 2: "grow-2", 3: "grow-3", 4: "grow-4" };

interface Item {
  key: string;
  span: number;
  node: ReactNode;
}

function packRows(items: Item[], cols: number): Item[][] {
  const rows: Item[][] = [];
  let row: Item[] = [];
  let used = 0;
  const flush = () => {
    if (row.length === 0) return;
    for (let i = used; i < cols; i++) row.push({ key: `pad-${rows.length}-${i}`, span: 1, node: null });
    rows.push(row);
    row = [];
    used = 0;
  };
  for (const item of items) {
    if (used + item.span > cols) flush();
    row.push(item);
    used += item.span;
    if (used >= cols) flush();
  }
  flush();
  return rows;
}

function RecordDetailForm({ items, cols }: { items: Item[]; cols: number }) {
  return (
    <div className="slds-form" role="list">
      {packRows(items, cols).map((row, ri) => (
        <div key={ri} className="slds-form__row">
          {row.map((item) => (
            <div key={item.key} className={clsx("slds-form__item", GROW[item.span])} role="listitem">
              {item.node}
            </div>
          ))}
        </div>
      ))}
    </div>
  );
}

function sectionItems(
  section: LayoutSection,
  si: number,
  cols: number,
  fieldsByName: Record<string, FieldMeta>,
  values: Record<string, unknown>,
  withHelp: boolean,
): Item[] {
  return section.fields.map((cell, ci) => {
    const fc = isSpacerCell(cell) ? null : cellField(cell);
    if (!fc) return { key: `spacer-${si}-${ci}`, span: 1, node: null };
    const span = fc.fullWidth ? cols : Math.min(Math.max(fc.colspan ?? 1, 1), cols);
    const oneColumn = span === cols && cols > 1;
    const base = fieldsByName[fc.field];
    if (!base) {
      // Referenced by the layout but missing from metadata — say so instead of crashing (forward-compat).
      return {
        key: fc.field,
        span,
        node: (
          <FormElement label={fc.field} layout="stacked" readOnly oneColumn={oneColumn}>
            <span className="slds-text-color_weak">{t("page.record.detail.missingField")}</span>
          </FormElement>
        ),
      };
    }
    const spec = mergeFieldSpec(base, fc);
    return {
      key: fc.field,
      span,
      node: (
        <FormElement
          label={spec.label}
          layout="stacked"
          readOnly
          oneColumn={oneColumn}
          fieldLevelHelp={withHelp && spec.helpText ? <FieldHelp label={spec.label} text={spec.helpText} /> : undefined}
        >
          <FieldDisplay field={spec} value={values[fc.field]} />
        </FormElement>
      ),
    };
  });
}

export function DetailSections({
  sections,
  fieldsByName,
  values,
  collapsible = true,
}: {
  sections: LayoutSection[];
  fieldsByName: Record<string, FieldMeta>;
  values: Record<string, unknown>;
  /** false on the printed document: shaded non-collapsible titles, always open, no field help. */
  collapsible?: boolean;
}) {
  if (sections.length === 0) return <p className="slds-text-color_weak">{t("page.record.detail.noSections")}</p>;
  return (
    <>
      {sections.map((section, si) => {
        const cols = Math.min(Math.max(section.columns ?? 1, 1), 4);
        const form = <RecordDetailForm items={sectionItems(section, si, cols, fieldsByName, values, collapsible)} cols={cols} />;
        const key = section.title ?? `section-${si}`;
        return section.title ? (
          <ExpandableSection key={key} title={section.title} collapsible={collapsible} defaultOpen={!section.collapsed}>
            {form}
          </ExpandableSection>
        ) : (
          <div key={key} className="slds-section slds-is-open">
            <div className="slds-section__content">{form}</div>
          </div>
        );
      })}
    </>
  );
}

// ── Tổng quan tab: layout sections + "Thông tin hệ thống" ────────────────────────────────────
const TIMESTAMP_FIELDS = new Set(["created_at", "updated_at", "deleted_at"]);
const SYSTEM_LABELS = {
  created_at: "page.record.system.createdAt",
  updated_at: "page.record.system.updatedAt",
  deleted_at: "page.record.system.deletedAt",
  created_by: "page.record.system.createdBy",
  modified_by: "page.record.system.modifiedBy",
  updated_by: "page.record.system.modifiedBy",
} as const;

function systemValue(record: ApiRecord, name: string): unknown {
  if (name === "created_at") return record.createdAt ?? record.data["created_at"];
  if (name === "updated_at") return record.updatedAt ?? record.data["updated_at"];
  return record.data[name];
}

export function OverviewTab({
  sections,
  fieldsByName,
  record,
  systemFields,
  names = {},
  tech,
}: {
  sections: LayoutSection[];
  fieldsByName: Record<string, FieldMeta>;
  record: ApiRecord;
  systemFields: string[];
  /** user id → display name (tenant directory); actor columns fall back to the raw id. */
  names?: Record<string, string>;
  /** DP-1.6: builder tech line (object slug · record id · version) — admins only. */
  tech?: { entity: string; recordId: string };
}) {
  const items: Item[] = systemFields
    .map((name) => ({ name, value: systemValue(record, name) }))
    .filter((r) => r.value != null && r.value !== "")
    .map(({ name, value }) => ({
      key: name,
      span: 1,
      node: (
        <FormElement label={name in SYSTEM_LABELS ? t(SYSTEM_LABELS[name as keyof typeof SYSTEM_LABELS]) : name} layout="stacked" readOnly>
          {TIMESTAMP_FIELDS.has(name) ? (
            <FieldDisplay field={metaToSpec({ name, fieldType: "datetime" })} value={value} />
          ) : (
            names[String(value)] ?? String(value)
          )}
        </FormElement>
      ),
    }));
  if (tech) {
    items.push(
      { key: "tech-entity", span: 1, node: <FormElement label={t("page.record.system.entity")} layout="stacked" readOnly>{tech.entity}</FormElement> },
      {
        key: "tech-id",
        span: 1,
        node: (
          <FormElement label={t("page.record.system.recordId")} layout="stacked" readOnly>
            <Button variant="base" title={t("page.record.system.copyId")} onClick={() => void navigator.clipboard?.writeText(tech.recordId)}>
              {tech.recordId}
            </Button>
          </FormElement>
        ),
      },
      { key: "tech-version", span: 1, node: <FormElement label={t("page.record.system.version")} layout="stacked" readOnly>{`v${record.version}`}</FormElement> },
    );
  }
  return (
    <>
      <DetailSections sections={sections} fieldsByName={fieldsByName} values={record.data} />
      {items.length > 0 && (
        <ExpandableSection title={t("page.record.system.title")}>
          <RecordDetailForm items={items} cols={2} />
        </ExpandableSection>
      )}
    </>
  );
}
