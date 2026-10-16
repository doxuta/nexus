"use client";

// DashboardAuthoring — admin widget CRUD folded INTO the app-face viewer /dashboards/[id],
// killing the old Studio/App dashboard split-brain (one artifact, ONE editor: rename + widget
// add/edit/remove + delete live next to the grid the admin already drags/resizes).
// SLDS (specs/038 T029): a card (blueprint `cards`) with the widget list (`slds-has-dividers_bottom-space`),
// icon buttons per row, and the widget form built from the kit's input / radio-button-group / select.
//
// Contract: this panel only computes the NEXT widget array and hands it up — the page owns
// persistence (full-replace PUT). The base array comes from getBaseWidgets() at SAVE time so an
// in-flight (debounced, un-persisted) layout drag is never dropped, and drafts carry ONLY
// form-modeled fields (lib/widgetAuthoring.WidgetDraft) so a stale draft can never clobber a
// fresher grid rect. Stored keys the form does not model (x/y/w/h) round-trip untouched; the fixed
// filters are edited in the form (specs/070 #266 — the list's condition builder).

import { entityOptionText, useEntityOptions } from "@/lib/entityOptions";
import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Pencil, Plus, Trash2, X } from "lucide-react";
import { hasDraftChange, listEntities, listFields, type DashboardWidgetDef, type DraftState } from "@/lib/api";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { qk } from "@/lib/query/keys";
import {
  WIDGET_TYPES, WIDGET_METRICS, blankWidget, upsertWidgetDraft, groupByChoices, isIndexFixable,
  valueFieldChoices, type WidgetDraft,
} from "@/lib/widgetAuthoring";
import { IndexFixNotice } from "./IndexFixNotice";
import { BUCKET_CHOICES, isBucketableGroupBy } from "@/lib/timeBuckets";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Card } from "@/components/slds/Card";
import { Input } from "@/components/slds/Input";
import { RadioButtonGroup } from "@/components/slds/RadioGroup";
import { ExpressionBuilder } from "@/components/slds/ExpressionBuilder";
import { filterRowIssues, filterRowsFromSaved } from "@/lib/savedFilters";
import { withOwnerFilter } from "@/lib/filterModel";
import { Select } from "@/components/slds/Select";

export function DashboardAuthoringPanel({
  widgets, saving, draftState, getBaseWidgets, onSave, onDeleteDashboard,
}: {
  widgets: DashboardWidgetDef[];
  /** Set when the builder reads a delivered workspace over the draft overlay (specs/063 S7-2). */
  draftState?: DraftState;
  saving: boolean;
  /** Layout-aware widget base (live rects + funnel toggles overlaid) — called at SAVE time. */
  getBaseWidgets: () => DashboardWidgetDef[];
  /** Persist the next widget array (full-replace PUT). Resolves true on success. */
  onSave: (next: DashboardWidgetDef[]) => Promise<boolean>;
  onDeleteDashboard: () => void;
}) {
  const [draft, setDraft] = useState<WidgetDraft | null>(null);
  const [removeId, setRemoveId] = useState<string | null>(null);

  const { data: entities = [], isLoading: entitiesLoading } = useQuery({
    queryKey: qk.entities(),
    queryFn: listEntities,
    staleTime: 5 * 60_000,
  });
  const draftEntity = draft?.entity ?? "";
  // Shares the qk.fields cache with the viewer's funnel-order lookup.
  const { data: fields = [] } = useQuery({
    queryKey: qk.fields(draftEntity),
    queryFn: () => listFields(draftEntity),
    staleTime: 5 * 60_000,
    enabled: !!draftEntity,
  });

  const patch = (p: Partial<WidgetDraft>) => setDraft((d) => (d ? { ...d, ...p } : d));

  function changeEntity(name: string) {
    setDraft((d) => {
      if (!d) return d;
      // Entity-specific picks no longer exist on the new entity; system timestamps survive.
      // The fixed filters name fields of the old entity — they go too.
      const keepSys = d.groupBy === "created_at" || d.groupBy === "updated_at";
      return { ...d, entity: name, field: undefined, filters: [], ...(keepSys ? {} : { groupBy: undefined, bucket: undefined }) };
    });
  }

  async function saveDraft() {
    if (!draft || !draft.entity) return;
    const ok = await onSave(upsertWidgetDraft(getBaseWidgets(), draft, fields));
    if (ok) setDraft(null);
  }

  async function removeWidget(id: string) {
    const ok = await onSave(getBaseWidgets().filter((w) => w.id !== id));
    if (ok) {
      setRemoveId(null);
      if (draft?.id === id) setDraft(null);
    }
  }

  const editingStored = draft ? widgets.find((w) => w.id === draft.id) : undefined;
  const gbChoices = groupByChoices(fields, draft?.groupBy);
  // specs/070 #265: an unindexed group-by gets "Bật index ngay" in place; saving waits for it
  // (the widget would 422 AGGREGATE_REQUIRES_INDEXED on every run).
  const groupByField = draft?.groupBy ? fields.find((f) => f.name === draft.groupBy) : undefined;
  const groupByNeedsIndex = !!groupByField && isIndexFixable(groupByField);
  const vfChoices = valueFieldChoices(fields, draft?.field);
  const entityOptions = draftEntity && !entities.some((e) => e.name === draftEntity)
    ? [...entities, { name: draftEntity, label: draftEntity, tableName: "" }]
    : entities;
  const entityChoices = useEntityOptions(entityOptions).map((o) => ({ value: o.value, label: entityOptionText(o) }));
  // Business words for the engine tokens a row / select would otherwise show raw (S13).
  const metricLabel = (m: string) => t(`page.reports.config.metric.${m}` as I18nKey);
  const typeLabel = (k: string) => (WIDGET_TYPES.find((x) => x.key === k)?.label ?? k).replace(/\s*\(.*\)$/, "");
  const draftFilters = draft?.filters ?? [];
  const filterIssues = filterRowIssues(draftFilters);
  const saveBlockedReason = groupByNeedsIndex
    ? t("page.reports.config.saveNeedsIndex")
    : Object.keys(filterIssues).length ? t("page.reports.config.saveFilterIncomplete") : null;

  return (
    <Card
      title={t("page.dashboards.authoring.title")}
    >
        <div className="mb-3 flex flex-wrap items-center gap-2">
          <Button
            variant="brand"
            iconLeft={Plus}
            onClick={() => setDraft(blankWidget(entities[0]?.name ?? ""))}
            disabled={entities.length === 0 || saving}
            title={
              entities.length === 0
                ? t(entitiesLoading ? "page.dashboards.authoring.entitiesLoading" : "page.dashboards.authoring.noEntities")
                : undefined
            }
          >
            {t("page.dashboards.authoring.add")}
          </Button>
          <Button variant="text-destructive" iconLeft={Trash2} onClick={onDeleteDashboard}>
            {t("page.dashboards.authoring.deleteDashboard")}
          </Button>
        </div>
      {hasDraftChange(draftState) && <ScopedNotification className="mb-3">{t("draft.saved")}</ScopedNotification>}
      {/* Widget rows */}
      <ul className="slds-has-dividers_bottom-space">
        {widgets.length === 0 && (
          <li className="slds-item slds-text-color_weak">{t("page.dashboards.authoring.empty")}</li>
        )}
        {widgets.map((w) => (
          <li key={w.id} className={draft?.id === w.id ? "slds-item slds-theme_shade" : "slds-item"}>
            {/* basis-40: in the 300px rail the "Xoá ô số liệu này?" confirm drops to its own line instead of squeezing the title. */}
            <div className="flex flex-wrap items-center gap-2">
              <div className="min-w-0 flex-1 basis-40">
                <p className="slds-truncate" title={w.title}>{w.title}</p>
                <p className="slds-text-body_small slds-text-color_weak slds-truncate">
                  {tf("page.dashboards.authoring.rowSub", {
                    type: typeLabel(w.type),
                    entity: entityChoices.find((c) => c.value === w.entity)?.label ?? w.entity,
                    metric: metricLabel(w.metric),
                  })}
                  {w.groupBy ? tf("page.dashboards.authoring.rowSubBy", { groupBy: w.groupBy }) : ""}
                </p>
              </div>
              {removeId === w.id ? (
                <div className="flex shrink-0 items-center gap-2">
                  <span className="slds-text-color_error">{t("page.dashboards.authoring.removeAsk")}</span>
                  <Button variant="destructive" onClick={() => removeWidget(w.id)} disabled={saving}>
                    {t("page.dashboards.authoring.removeConfirm")}
                  </Button>
                  <Button variant="base" onClick={() => setRemoveId(null)}>
                    {t("action.cancel")}
                  </Button>
                </div>
              ) : (
                <div className="flex shrink-0 items-center gap-1">
                  <ButtonIcon
                    icon={Pencil}
                    variant="border-filled"
                    label={tf("page.dashboards.authoring.edit", { title: w.title })}
                    onClick={() => setDraft({
                      id: w.id, title: w.title, type: w.type, entity: w.entity, metric: w.metric,
                      field: w.field, groupBy: w.groupBy, bucket: w.bucket, filters: filterRowsFromSaved(w.filters),
                    })}
                  />
                  <ButtonIcon
                    icon={X}
                    variant="border-filled"
                    label={tf("page.dashboards.authoring.remove", { title: w.title })}
                    onClick={() => setRemoveId(w.id)}
                  />
                </div>
              )}
            </div>
          </li>
        ))}
      </ul>

      {/* Widget form (add / edit) */}
      {draft && (
        <form
          className="slds-form @container mt-4"
          onSubmit={(e) => {
            e.preventDefault();
            void saveDraft();
          }}
        >
          <h3 className="slds-text-heading_small">
            {t(editingStored ? "page.dashboards.authoring.editTitle" : "page.dashboards.authoring.newTitle")}
          </h3>
          <div className="mt-2 grid gap-3 @md:grid-cols-2 @3xl:grid-cols-3">
            <Input
              label={t("page.dashboards.authoring.titleLabel")}
              value={draft.title}
              onChange={(e) => patch({ title: e.target.value })}
            />
            <RadioButtonGroup
              label={t("page.dashboards.authoring.type")}
              className="@3xl:col-span-2"
              value={draft.type}
              onValueChange={(type) => patch({ type })}
              options={WIDGET_TYPES.map(({ key, label }) => ({ value: key, label: label.replace(/\s*\(.*\)$/, "") }))}
            />
            <Select
              label={t("page.dashboards.authoring.entity")}
              placeholder={false}
              value={draft.entity}
              onChange={(e) => changeEntity(e.target.value)}
              options={entityChoices}
            />
            <Select
              label={t("page.dashboards.authoring.metric")}
              placeholder={false}
              value={draft.metric}
              onChange={(e) => patch({ metric: e.target.value })}
              options={WIDGET_METRICS.map((m) => ({ value: m, label: metricLabel(m) }))}
            />
            <Select
              label={t("page.dashboards.authoring.field")}
              placeholder={t("page.dashboards.authoring.fieldAll")}
              value={draft.field ?? ""}
              onChange={(e) => patch({ field: e.target.value || undefined })}
              options={vfChoices.map((f) => ({ value: f.value, label: f.label }))}
            />
            <Select
              label={t("page.dashboards.authoring.groupBy")}
              placeholder={t("page.dashboards.authoring.groupNone")}
              help={t("page.dashboards.authoring.groupHelp")}
              value={draft.groupBy ?? ""}
              onChange={(e) => {
                const gb = e.target.value || undefined;
                // Re-pointing groupBy at a non-date column invalidates the bucket — clear it.
                patch({ groupBy: gb, ...(isBucketableGroupBy(gb, fields) ? {} : { bucket: undefined }) });
              }}
              // Unindexed but indexable → pickable, fixed in place below; not indexable → dead end.
              options={gbChoices.map((c) => ({
                value: c.value,
                label: c.label + (c.indexed ? "" : t("page.dashboards.authoring.notIndexed")),
                disabled: !c.indexed && !c.fixable,
              }))}
            />
            {/* Bucket — only when groupBy is date-kind; the BE ties bucket to a date groupBy
                (WIDGET_BUCKET_REQUIRES_GROUPBY / AGGREGATE_BUCKET_FIELD_NOT_DATE). */}
            {isBucketableGroupBy(draft.groupBy, fields) && (
              <Select
                label={t("page.dashboards.authoring.bucket")}
                placeholder={BUCKET_CHOICES.find((b) => b.value === "")?.label}
                help={t("page.dashboards.authoring.bucketHelp")}
                value={draft.bucket ?? ""}
                onChange={(e) => patch({ bucket: e.target.value || undefined })}
                options={BUCKET_CHOICES.filter((b) => b.value !== "").map((b) => ({ value: b.value, label: b.label }))}
              />
            )}
          </div>
          <IndexFixNotice entity={draft.entity} field={groupByField} />
          {/* specs/070 #266: the widget's fixed filters ("Phiếu đang mở", "Đơn Thắng tháng này"),
              the same condition builder as the report panel; the run ANDs them with the overlay. */}
          <div className="mt-3">
            <ExpressionBuilder
              title={t("page.reports.config.filters")}
              fields={withOwnerFilter(fields)}
              value={draftFilters}
              onChange={(rows) => patch({ filters: rows })}
              errors={filterIssues}
            />
            <p className="slds-text-body_small slds-text-color_weak pt-1">{t("page.reports.config.filtersHelp")}</p>
          </div>
          <div className="mt-3 flex items-center gap-2">
            <Button
              type="submit"
              variant="brand"
              disabled={saving || !draft.entity || !!saveBlockedReason}
              title={saveBlockedReason ?? undefined}
            >
              {t("page.dashboards.authoring.save")}
            </Button>
            <Button onClick={() => setDraft(null)} disabled={saving}>
              {t("action.cancel")}
            </Button>
          </div>
        </form>
      )}
    </Card>
  );
}
