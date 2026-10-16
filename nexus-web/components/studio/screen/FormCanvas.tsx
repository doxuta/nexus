"use client";

// The stage of the form screen editor (specs/075 US1, FR-011..FR-027): the REAL customer form, drawn with the customer's
// own template (RecordFormPage) and grid (SectionFields) inside CustomerPreviewFrame, bound to the table's first
// record. The editor only wraps each cell (SectionFields.renderCell) — it never draws a second grid.
// Keyboard (FR-027): the stage is ONE Tab stop (roving tabindex); arrows/Home/End walk the cells, Enter/Space select,
// Esc deselects. Selection is by field name (that is what `?truong=` carries).
import { useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import clsx from "clsx";
import { useDroppable } from "@dnd-kit/core";
import { RecordFormPage } from "@/components/slds/templates/RecordFormPage";
import { Button } from "@/components/slds/Button";
import { ExpandableSection } from "@/components/slds/ExpandableSection";
import { SectionFields } from "@/components/layout/SectionGrid";
import { CustomerPreviewFrame } from "@/components/studio/CustomerPreviewFrame";
import { cellField, type ApiRecord, type FieldMeta } from "@/lib/api";
import { sectionsToJson, type DraftSection } from "@/lib/layoutDraft";
import { seedValues } from "@/lib/recordPayload";
import { FIELD_TYPE_META, type FieldType } from "@/lib/metadata";
import { t, tf } from "@/lib/i18n";
import type { ScreenOverlay } from "@/lib/screenProposalOverlay";
import { CanvasCell } from "./CanvasCell";
import { ScreenGhost } from "./ScreenGhost";
import { sectionDropId } from "./dnd";

export interface FormCanvasProps {
  tableLabel: string;
  fields: FieldMeta[];
  draft: DraftSection[];
  record: ApiRecord | null;
  appName: string;
  accentColor?: string;
  logoUrl?: string;
  selected: string | null;
  onSelect: (field: string, viaPointer: boolean) => void;
  onDeselect: () => void;
  onAddSection: () => void;
  /** A drag is in progress (drop-zone look appears only then). */
  dragging?: boolean;
  /** Droppable currently under the dragged item (cell id), for the landing hint. */
  overId?: string | null;
  onRemoveSpacer?: (cellId: string) => void;
  /** "form" (default): the whole form page in the customer frame. "detail": only the section stack of the record page
   *  (its caller draws the frame, header and tabs); cells are drawn in view mode. */
  mode?: "form" | "detail";
  /** The assistant's proposal for this screen (specs/075 US9), drawn as ghosts; null/absent = none. */
  overlay?: ScreenOverlay | null;
}

function EmptySection({ id, dragging, children }: { id: string; dragging: boolean; children: ReactNode }) {
  const { setNodeRef } = useDroppable({ id: sectionDropId(id) });
  return <div ref={setNodeRef} className={clsx(dragging && "slds-drop-zone slds-drop-zone_drag")}>{children}</div>;
}

export const typeLabelOf = (f: FieldMeta): string => FIELD_TYPE_META[f.fieldType as FieldType]?.label ?? f.fieldType;

export function FormCanvas({ tableLabel, fields, draft, record, appName, accentColor, logoUrl, selected, onSelect, onDeselect, onAddSection, dragging = false, overId = null, onRemoveSpacer, mode = "form", overlay = null }: FormCanvasProps) {
  const root = useRef<HTMLDivElement>(null);
  const key = overlay?.change.proposalKey;
  // bring the first ghost into view when a preview turns on (nearest: never yank the page when it is already visible)
  useEffect(() => {
    if (key) root.current?.querySelector("[data-proposal-key]")?.scrollIntoView?.({ block: "nearest" });
  }, [key]);
  const fieldsByName = useMemo(() => Object.fromEntries(fields.map((f) => [f.name, f])), [fields]);
  const values = useMemo(() => seedValues(fields, record?.data), [fields, record]);
  // roving order = every cell (spacers included), by cell id; selection stays by field name
  const order = useMemo(() => draft.flatMap((s) => s.cells.map((c) => c.id)), [draft]);
  const selectedId = useMemo(() => draft.flatMap((s) => s.cells).find((c) => c.kind === "field" && c.field === selected)?.id ?? null, [draft, selected]);
  const refs = useRef(new Map<string, HTMLDivElement>());
  const [rover, setRover] = useState<string | null>(null);
  // the one Tab stop: the cell last focused/selected, else the first
  const stop = (rover && order.includes(rover) ? rover : null) ?? (selectedId && order.includes(selectedId) ? selectedId : null) ?? order[0] ?? null;
  const [announce, setAnnounce] = useState("");

  const focusCell = (name: string | undefined) => {
    if (!name) return;
    setRover(name);
    refs.current.get(name)?.focus();
  };
  const navKey = (name: string) => (e: KeyboardEvent<HTMLDivElement>) => {
    const i = order.indexOf(name);
    const go = (to: number) => { e.preventDefault(); focusCell(order[Math.min(Math.max(to, 0), order.length - 1)]); };
    if (e.key === "ArrowRight" || e.key === "ArrowDown") go(i + 1);
    else if (e.key === "ArrowLeft" || e.key === "ArrowUp") go(i - 1);
    else if (e.key === "Home") go(0);
    else if (e.key === "End") go(order.length - 1);
    else if (e.key === "Escape" && selected) {
      e.preventDefault();
      onDeselect();
      setAnnounce(t("studio.build.screens.stage.deselected"));
    }
  };

  const sections = draft.map((s) => {
    const section = sectionsToJson([s]).sections[0] as unknown as Parameters<typeof SectionFields>[0]["section"];
    const title = s.title.trim() || t("studio.build.screens.stage.sectionUntitled");
    const body =
      s.cells.length === 0 ? (
        <EmptySection id={s.id} dragging={dragging}>
          <p className="slds-text-color_weak slds-p-vertical_small">{t("studio.build.screens.stage.sectionEmpty")}</p>
        </EmptySection>
      ) : (
        <div className={clsx(dragging && "slds-drop-zone slds-drop-zone_drag")}>
        <SectionFields
          section={section}
          mode={mode === "detail" ? "detail" : "edit"}
          fieldsByName={fieldsByName}
          values={values}
          renderCell={(cell, ci, node) => {
            const name = cellField(cell)?.field;
            const f = name ? fieldsByName[name] : undefined;
            const dc = s.cells[ci];
            if (dc?.kind === "spacer") {
              return (
                <CanvasCell
                  cellId={dc.id}
                  ariaLabel={tf("studio.build.screens.stage.spacerLabel", { section: title, i: ci + 1, n: s.cells.length })}
                  dragName={t("studio.build.screens.field.spacer")}
                  selected={false}
                  tabbable={stop === dc.id}
                  dragOver={dragging && overId === dc.id}
                  cellRef={(el) => { if (el) refs.current.set(dc.id, el); else refs.current.delete(dc.id); }}
                  onNavKey={navKey(dc.id)}
                  onSelect={() => { setRover(dc.id); }}
                  onRemove={onRemoveSpacer ? () => onRemoveSpacer(dc.id) : undefined}
                >
                  {node}
                </CanvasCell>
              );
            }
            if (!name || !f || !dc) return node;
            const label = s.cells[ci] && s.cells[ci].kind === "field" && s.cells[ci].labelOverride ? s.cells[ci].labelOverride! : f.label || f.name;
            const aria = tf("studio.build.screens.stage.cellLabel", {
              label, type: typeLabelOf(f), req: f.isRequired ? t("studio.build.screens.stage.req") : "", section: title, i: ci + 1, n: s.cells.length,
            });
            const gone = overlay?.removes.includes(name);
            const re = overlay?.relabels.find((r) => r.field === name);
            const ghost = overlay && (gone || re) ? (
              <ScreenGhost change={overlay.change} label={gone ? label : re?.after ?? label} before={gone ? undefined : re?.before !== re?.after ? re?.before : undefined} willRemove={gone} />
            ) : undefined;
            return (
              <CanvasCell
                cellId={dc.id}
                ghost={ghost}
                ariaLabel={aria}
                dragName={label}
                selected={selected === name}
                tabbable={stop === dc.id}
                dragOver={dragging && overId === dc.id}
                cellRef={(el) => { if (el) refs.current.set(dc.id, el); else refs.current.delete(dc.id); }}
                onNavKey={navKey(dc.id)}
                onSelect={(viaPointer) => {
                  setRover(dc.id);
                  onSelect(name, viaPointer);
                  setAnnounce(tf("studio.build.screens.stage.selected", { label }));
                }}
              >
                {node}
              </CanvasCell>
            );
          }}
        />
        </div>
      );
    const si = draft.indexOf(s);
    const adds = (overlay?.adds ?? []).filter((a) => (a.section < 0 || a.section >= draft.length ? si === draft.length - 1 : a.section === si));
    const withAdds = adds.length === 0 || !overlay ? body : (
      <>
        {body}
        {adds.map((a, i) => <div key={i} className="slds-m-top_x-small"><ScreenGhost change={overlay.change} label={a.label} /></div>)}
      </>
    );
    return { id: s.id, title, body: withAdds };
  });

  if (mode === "detail") {
    return (
      <div ref={root}>
        <p className="slds-assistive-text" role="status" aria-live="polite">{announce}</p>
        {sections.map((s) => (
          <ExpandableSection key={s.id} title={s.title}>{s.body}</ExpandableSection>
        ))}
        <div className="slds-m-top_small">
          <Button variant="base" onClick={onAddSection}>{t("studio.build.screens.stage.addSection")}</Button>
        </div>
      </div>
    );
  }
  return (
    <div ref={root}>
      <p className="slds-assistive-text" role="status" aria-live="polite">{announce}</p>
      {!record && <p className="slds-text-color_weak slds-m-bottom_small">{t("studio.build.screens.stage.emptyNote")}</p>}
      <CustomerPreviewFrame
        appName={appName}
        accentColor={accentColor}
        logoUrl={logoUrl}
        caption={t(record ? "studio.build.screens.stage.captionRecord" : "studio.build.screens.stage.captionEmpty")}
      >
        <RecordFormPage
          title={tf("studio.build.screens.stage.formTitle", { bang: tableLabel })}
          sections={sections}
          onSubmit={(e) => e.preventDefault()}
          onCancel={() => {}}
          docked={false}
          inertFooter
        />
      </CustomerPreviewFrame>
      <div className="slds-m-top_small">
        <Button variant="base" onClick={onAddSection}>{t("studio.build.screens.stage.addSection")}</Button>
      </div>
    </div>
  );
}
