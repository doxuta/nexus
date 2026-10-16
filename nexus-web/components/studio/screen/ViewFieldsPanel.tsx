"use client";

// "Kanban và Lịch" (specs/075 US5, T102; carried from the old LayoutBuilder ListConfig, DAILY-QUEUE #5): which picklist
// the Kanban board groups by and which date field the Calendar lays out on. Only usable fields are offered (a text
// field for Kanban would earn a 422). A declaration that no longer fits is shown by name, never as a blank "Tự đoán".
import { Select } from "@/components/slds/Select";
import type { FieldMeta } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import { setListCalendar, setListKanban, type ListDraft } from "@/lib/screenEditorModel";

export interface ViewFieldsPanelProps {
  fields: FieldMeta[];
  draft: ListDraft;
  onChange: (next: ListDraft) => void;
}

export function ViewFieldsPanel({ fields, draft, onChange }: ViewFieldsPanelProps) {
  const picklists = fields.filter((f) => f.fieldType === "picklist");
  const dates = fields.filter((f) => f.fieldType === "date" || f.fieldType === "datetime");
  const stale = (declared: string, usable: FieldMeta[]) =>
    declared && !usable.some((f) => f.name === declared) ? <option value={declared}>{tf("studio.build.screens.list.view.stale", { name: declared })}</option> : null;
  return (
    <section aria-labelledby="nx-list-view">
      <h3 id="nx-list-view" className="slds-text-title_caps slds-m-bottom_x-small">{t("studio.build.screens.list.view.title")}</h3>
      <p className="slds-text-color_weak slds-m-bottom_small">{t("studio.build.screens.list.view.hint")}</p>
      <Select
        label={t("studio.build.screens.list.view.kanban")}
        placeholder={false}
        value={draft.kanban}
        disabled={picklists.length === 0 && !draft.kanban}
        onChange={(e) => onChange(setListKanban(draft, e.target.value))}
      >
        <option value="">{t("studio.build.screens.list.view.auto")}</option>
        {stale(draft.kanban, picklists)}
        {picklists.map((f) => <option key={f.name} value={f.name}>{f.label || f.name}</option>)}
      </Select>
      {picklists.length === 0 && <p className="slds-text-body_small slds-text-color_weak">{t("studio.build.screens.list.view.noKanban")}</p>}
      <Select
        label={t("studio.build.screens.list.view.calendar")}
        placeholder={false}
        value={draft.calendar}
        disabled={dates.length === 0 && !draft.calendar}
        onChange={(e) => onChange(setListCalendar(draft, e.target.value))}
      >
        <option value="">{t("studio.build.screens.list.view.auto")}</option>
        {stale(draft.calendar, dates)}
        {dates.map((f) => <option key={f.name} value={f.name}>{f.label || f.name}</option>)}
      </Select>
      {dates.length === 0 && <p className="slds-text-body_small slds-text-color_weak">{t("studio.build.screens.list.view.noCalendar")}</p>}
    </section>
  );
}
