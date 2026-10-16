"use client";

// "Cột hiển thị" (specs/075 US5, T101): the right column of the list screen. Default sort on top (only the three keys the
// server sorts a list layout by), then the ticked columns in their order with Lên/Xuống, then the fields not shown.
// Relation / long text / multi-select fields are not offered, and one line says why.
import { Button } from "@/components/slds/Button";
import { Checkbox } from "@/components/slds/Checkbox";
import { Select } from "@/components/slds/Select";
import type { FieldMeta } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import {
  LIST_SORT_FIELDS, listCandidateFields, moveListCol, setListSort, toggleListCol, type ListDraft, type ListSortField,
} from "@/lib/screenEditorModel";

export interface ListColumnsPanelProps {
  fields: FieldMeta[];
  draft: ListDraft;
  /** Column selected on the stage (a header click); its row is marked. */
  selected: string | null;
  onSelect: (name: string | null) => void;
  onChange: (next: ListDraft) => void;
}

export function ListColumnsPanel({ fields, draft, selected, onSelect, onChange }: ListColumnsPanelProps) {
  const cand = listCandidateFields(fields);
  const byName = new Map(cand.map((f) => [f.name, f]));
  const shown = draft.cols.map((c) => byName.get(c)).filter((f): f is FieldMeta => !!f);
  const rest = cand.filter((f) => !draft.cols.includes(f.name));
  const label = (f: FieldMeta) => draft.labels[f.name] || f.label || f.name;
  return (
    <div>
      <section aria-labelledby="nx-list-sort" className="slds-m-bottom_large">
        <h3 id="nx-list-sort" className="slds-text-title_caps slds-m-bottom_x-small">{t("studio.build.screens.list.sort.title")}</h3>
        <Select
          label={t("studio.build.screens.list.sort.field")}
          placeholder={false}
          value={draft.sortField}
          onChange={(e) => onChange(setListSort(draft, e.target.value as ListSortField, draft.sortOrder))}
        >
          {LIST_SORT_FIELDS.map((k) => <option key={k} value={k}>{t(`studio.build.screens.list.sort.${k}` as I18nKey)}</option>)}
        </Select>
        <Select
          label={t("studio.build.screens.list.sort.order")}
          placeholder={false}
          value={draft.sortOrder}
          onChange={(e) => onChange(setListSort(draft, draft.sortField, e.target.value as "asc" | "desc"))}
        >
          <option value="asc">{t("studio.build.screens.list.sort.asc")}</option>
          <option value="desc">{t("studio.build.screens.list.sort.desc")}</option>
        </Select>
      </section>

      <section aria-labelledby="nx-list-cols">
        <h3 id="nx-list-cols" className="slds-text-title_caps slds-m-bottom_x-small">
          {t("studio.build.screens.list.cols.title")} <span className="slds-text-color_weak">{tf("studio.build.screens.list.cols.count", { n: shown.length })}</span>
        </h3>
        {cand.length === 0 ? (
          <p className="slds-text-color_weak">{t("studio.build.screens.list.cols.none")}</p>
        ) : (
          <ul className="slds-has-dividers_bottom">
            {shown.map((f, i) => (
              <li key={f.name} className="slds-grid slds-grid_vertical-align-center slds-p-vertical_xx-small" aria-current={selected === f.name ? "true" : undefined}>
                <div className="slds-col">
                  <Checkbox
                    label={label(f)}
                    aria-label={tf("studio.build.screens.list.cols.show", { name: label(f) })}
                    checked
                    onChange={() => { onChange(toggleListCol(draft, f.name, false, fields)); if (selected === f.name) onSelect(null); }}
                  />
                </div>
                <Button variant="base" disabled={i === 0} aria-label={tf("studio.build.screens.list.cols.up", { name: label(f) })} onClick={() => onChange(moveListCol(draft, f.name, -1))}>
                  {t("studio.build.screens.field.up")}
                </Button>
                <Button variant="base" className="slds-m-left_x-small" disabled={i === shown.length - 1} aria-label={tf("studio.build.screens.list.cols.down", { name: label(f) })} onClick={() => onChange(moveListCol(draft, f.name, 1))}>
                  {t("studio.build.screens.field.down")}
                </Button>
              </li>
            ))}
            {rest.map((f) => (
              <li key={f.name} className="slds-p-vertical_xx-small">
                <Checkbox
                  label={label(f)}
                  aria-label={tf("studio.build.screens.list.cols.show", { name: label(f) })}
                  checked={false}
                  onChange={() => { onChange(toggleListCol(draft, f.name, true, fields)); onSelect(f.name); }}
                />
              </li>
            ))}
          </ul>
        )}
        {cand.length < fields.length && <p className="slds-text-body_small slds-text-color_weak slds-m-top_x-small">{t("studio.build.screens.list.cols.skipped")}</p>}
      </section>
    </div>
  );
}
