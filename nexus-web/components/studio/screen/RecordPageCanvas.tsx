"use client";

// The stage of the record-page editor (specs/075 US4, FR-040..FR-046): the REAL customer record home
// (RecordHomePage) in the customer frame, bound to the table's first record. Tab "Tổng quan" is the section stack
// (FormCanvas mode="detail": the same cells, drawn in view mode); tab "Quan hệ" is the list of related lists; the
// built-in tabs are one line each — nothing is drawn that the maker cannot change. The detail row under the title
// is the customer page's own `headerHighlights` of the first section (first two fields with a value).
import type { ScreenOverlay } from "@/lib/screenProposalOverlay";
import { useMemo, useState } from "react";
import { FileText } from "lucide-react";
import { RecordHomePage } from "@/components/slds/templates/RecordHomePage";
import { FieldDisplay } from "@/components/fields/display";
import { CustomerPreviewFrame } from "@/components/studio/CustomerPreviewFrame";
import { headerHighlights } from "@/components/layout/LayoutDetail";
import { type ApiRecord, type FieldMeta, type LayoutSection } from "@/lib/api";
import { metaToSpec } from "@/lib/fieldSpec";
import { sectionsToJson, type DraftSection } from "@/lib/layoutDraft";
import { recordLabel } from "@/lib/recordLabel";
import { t, tf } from "@/lib/i18n";
import { FormCanvas } from "./FormCanvas";
import { RelatedListsCanvas } from "./RelatedListsCanvas";

export interface RecordPageCanvasProps {
  slug: string;
  entity: string;
  tableLabel: string;
  fields: FieldMeta[];
  draft: DraftSection[];
  record: ApiRecord | null;
  appName: string;
  accentColor?: string;
  logoUrl?: string;
  selected: string | null;
  /** Selected related list (`?lien-quan=`). */
  relId: string | null;
  onRel: (id: string | null) => void;
  onSelect: (field: string, viaPointer: boolean) => void;
  onDeselect: () => void;
  onAddSection: () => void;
  overlay?: ScreenOverlay | null;
  /** Business label of another table (for the related lists); defaults to its name. */
  labelOf?: (entityName: string) => string;
}

const BUILTIN = ["notes", "files", "history", "approvals"] as const;

export function RecordPageCanvas(p: RecordPageCanvasProps) {
  const [tab, setTab] = useState<string>(p.relId ? "related" : "overview");
  const fieldsByName = useMemo(() => Object.fromEntries(p.fields.map((f) => [f.name, f])), [p.fields]);
  const sections = useMemo(() => sectionsToJson(p.draft).sections as unknown as LayoutSection[], [p.draft]);
  const title = p.record ? recordLabel(p.record, p.fields) : tf("studio.build.screens.record.homeTitle", { bang: p.tableLabel });
  const highlights = p.record ? headerHighlights(sections, fieldsByName, p.record.data, title) : [];
  const firstHasFields = (p.draft[0]?.cells ?? []).some((c) => c.kind === "field");

  const oneLine = (id: (typeof BUILTIN)[number]) => ({
    value: id,
    label: t(`page.record.tab.${id}`),
    content: tab === id ? <p className="slds-text-color_weak slds-p-around_small">{t("studio.build.screens.record.builtin")}</p> : null,
  });
  const tabs = [
    {
      value: "overview",
      label: t("page.record.tab.overview"),
      content: tab !== "overview" ? null : (
        <FormCanvas
          mode="detail"
          tableLabel={p.tableLabel}
          fields={p.fields}
          draft={p.draft}
          record={p.record}
          appName={p.appName}
          selected={p.selected}
          onSelect={p.onSelect}
          onDeselect={p.onDeselect}
          onAddSection={p.onAddSection}
          overlay={p.overlay}
        />
      ),
    },
    {
      value: "related",
      label: t("page.record.tab.related"),
      content:
        tab === "related" ? (
          <RelatedListsCanvas slug={p.slug} entity={p.entity} labelOf={p.labelOf ?? ((n) => n)} selectedId={p.relId} onSelect={p.onRel} />
        ) : null,
    },
    ...BUILTIN.map(oneLine),
  ];

  return (
    <div>
      {!p.record && <p className="slds-text-color_weak slds-m-bottom_small">{t("studio.build.screens.record.emptyNote")}</p>}
      {!firstHasFields && <p className="slds-text-color_weak slds-m-bottom_small">{t("studio.build.screens.record.highlightsHint")}</p>}
      <CustomerPreviewFrame
        appName={p.appName}
        accentColor={p.accentColor}
        logoUrl={p.logoUrl}
        caption={t(p.record ? "studio.build.screens.stage.captionRecord" : "studio.build.screens.stage.captionEmpty")}
      >
        <RecordHomePage
          icon={{ glyph: FileText, name: "record" }}
          objectLabel={p.tableLabel}
          title={title}
          details={highlights.map(({ field, value }) => ({
            label: field.label ?? field.name,
            value: <FieldDisplay field={metaToSpec(field)} value={value} />,
          }))}
          tabs={tabs}
          tab={tab}
          onTabChange={setTab}
          tabsLabel={t("page.record.tabsLabel")}
        />
      </CustomerPreviewFrame>
    </div>
  );
}
