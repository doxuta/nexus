"use client";

// Section ("khung") management of the form editor (specs/075 US1, L1b): the list of sections and the properties of the
// picked one — title, 1-4 columns, up/down, "Thêm ô trống", "Xoá khung" (a section that holds required fields hands
// them to its neighbour; refused when it has none — screenEditorModel.removeSectionGuarded).
import clsx from "clsx";
import { Button } from "@/components/slds/Button";
import { Input } from "@/components/slds/Input";
import { Select } from "@/components/slds/Select";
import type { DraftSection, SectionColumns } from "@/lib/layoutDraft";
import { t, tf } from "@/lib/i18n";

export interface SectionPropertiesProps {
  section: DraftSection;
  canUp: boolean;
  canDown: boolean;
  /** The last "Xoá khung" was refused (only section holding a required field). */
  removeBlocked: boolean;
  onTitle: (title: string) => void;
  onColumns: (n: SectionColumns) => void;
  onUp: () => void;
  onDown: () => void;
  onAddSpacer: () => void;
  onRemove: () => void;
}

export function SectionProperties(p: SectionPropertiesProps) {
  return (
    <div>
      <Input label={t("studio.build.screens.section.title")} value={p.section.title} onChange={(e) => p.onTitle(e.target.value)} />
      <Select
        label={t("studio.build.screens.section.columns")}
        placeholder={false}
        value={String(p.section.columns)}
        onChange={(e) => p.onColumns(Number(e.target.value) as SectionColumns)}
      >
        {[1, 2, 3, 4].map((n) => (
          <option key={n} value={n}>{n}</option>
        ))}
      </Select>
      <div className="slds-button-group slds-m-vertical_small" role="group">
        <Button variant="neutral" disabled={!p.canUp} onClick={p.onUp}>{t("studio.build.screens.section.up")}</Button>
        <Button variant="neutral" disabled={!p.canDown} onClick={p.onDown}>{t("studio.build.screens.section.down")}</Button>
      </div>
      <div>
        <Button variant="neutral" onClick={p.onAddSpacer}>{t("studio.build.screens.section.addSpacer")}</Button>
      </div>
      <div className="slds-m-top_small">
        <Button variant="text-destructive" onClick={p.onRemove}>{t("studio.build.screens.section.remove")}</Button>
      </div>
      {p.removeBlocked && <p role="alert" className="slds-text-color_error slds-m-top_x-small">{t("studio.build.screens.section.requiredStay")}</p>}
    </div>
  );
}

export interface SectionsPanelProps {
  sections: DraftSection[];
  selectedId: string | null;
  onPick: (id: string) => void;
}

export function SectionsPanel({ sections, selectedId, onPick }: SectionsPanelProps) {
  return (
    <section aria-labelledby="nx-sections-title">
      <h3 id="nx-sections-title" className="slds-text-title_caps slds-m-bottom_x-small">
        {t("studio.build.screens.section.listTitle")} <span className="slds-text-color_weak">{sections.length}</span>
      </h3>
      <ul className="slds-has-dividers_bottom">
        {sections.map((s) => (
          <li key={s.id} className={clsx("slds-item", s.id === selectedId && "slds-theme_shade")}>
            <button type="button" className="slds-button slds-button_reset slds-text-align_left slds-p-vertical_xx-small" style={{ width: "100%" }} aria-pressed={s.id === selectedId} onClick={() => onPick(s.id)}>
              <span className="slds-truncate slds-show">{s.title.trim() || t("studio.build.screens.stage.sectionUntitled")}</span>
              <span className="slds-text-body_small slds-text-color_weak slds-show">
                {tf("studio.build.screens.section.summary", { cols: s.columns, n: s.cells.length })}
              </span>
            </button>
          </li>
        ))}
      </ul>
    </section>
  );
}
