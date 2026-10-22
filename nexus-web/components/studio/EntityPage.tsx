"use client";

// EntityPage — the ONE table page of a Studio project (`dung/du-lieu/<bang>`, specs/073 FR-038, T169). It replaces
// EntityHub on this route: no `slds-tabs_default` strip (a third navigation level), no `?tab=`, no second copy of the
// layout / diagram builders. Header = "← Tất cả bảng" + the table as the frame's h2 + Sửa / Xoá; body = the field editor,
// then a flat "Bản ghi" section; the right column (300px, inside the same surface, from 1024px) is RelatedLists (specs/074 lát 4):
// Màn hình, Liên kết, Báo cáo, Dữ liệu of THIS table, hidden while a field inspector is open (`?truong=`).
// Sửa / Xoá are the old hub's dialogs with the same calls (EntityHub stays untouched until F2 deletes it, so the
// small option helpers below are a copy on purpose). "Trường tên" (options.labelField, #272) lives in the Sửa dialog.
import Link from "next/link";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { MoreHorizontal, Plus, Table2 } from "lucide-react";
import { updateEntity, deleteEntity, listEntities, listFields, type EntityMeta, type FieldMeta } from "@/lib/api";
import { undoDraftDelete } from "@/lib/draftUndo";
import { emitDraftsChanged } from "@/lib/draftEvents";
import { toast } from "@/components/ui/Toaster";
import { qk } from "@/lib/query/keys";
import { t, tf } from "@/lib/i18n";
import { RelatedLists } from "@/components/studio/RelatedLists";
import { useRelatedSections } from "@/components/studio/useRelatedSections";
import { studioHref } from "@/lib/studioRoutes";
import { PageHeader } from "@/components/slds/PageHeader";
import { Menu, MenuItem } from "@/components/slds/Menu";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { Button } from "@/components/slds/Button";
import { Input } from "@/components/slds/Input";
import { Select } from "@/components/slds/Select";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { fill } from "@/components/slds/DataTable";
import { EntityFieldEditor } from "@/components/studio/EntityFieldEditor";
import { useAskAssistant } from "@/components/studio/assistant/AssistantProvider";
import { useRecordCounts } from "@/lib/operateData";
import { EntityRecordsQuickView } from "@/components/studio/EntityRecordsQuickView";
import { useProposedChange } from "@/components/studio/assistant/ProposalOverlay";
import { GhostLive, ProposedGhost } from "@/components/studio/assistant/ProposedGhost";

// Field types that can name a row — mirrors metadata.ResolveLabelField on the server.
const NAME_FIELD_TYPES = new Set(["text", "email", "auto_number"]);

// sys_entities.options travels as a JSON string on GET /metadata/entities (object on PUT replies).
function parseEntityOptions(raw: EntityMeta["options"]): Record<string, unknown> {
  if (!raw) return {};
  if (typeof raw === "object") return raw;
  try {
    const o = JSON.parse(raw);
    return o && typeof o === "object" ? (o as Record<string, unknown>) : {};
  } catch {
    return {};
  }
}

export function EntityPage({
  slug, entity, field, onFieldChange, onDeleted, isNew = false,
}: {
  slug: string;
  entity: EntityMeta;
  /** `?truong=` already validated by parseStudioPath. */
  field: string | null;
  /** The inspector opened/closed on a field: the route writes `?truong=`. */
  onFieldChange: (name: string | null) => void;
  /** After a delete: this table no longer exists, the route goes back to the list. */
  onDeleted: () => void;
  /** `?moi=1`: the table was just created from the list; say what to do next. */
  isNew?: boolean;
}) {
  const qc = useQueryClient();
  const ask = useAskAssistant(); // null outside the project frame => no button on the quick view
  const label = entity.label || entity.name;
  const fieldProposed = useProposedChange({ docType: "field", entity: entity.name });
  const fieldGhost = fieldProposed && fieldProposed.op !== "delete" ? fieldProposed : null;
  const labelIn = (v: unknown): string | null => (v && typeof v === "object" && typeof (v as { label?: unknown }).label === "string" ? (v as { label: string }).label : null);
  const newLabel = labelIn(fieldGhost?.after);
  const oldLabel = labelIn(fieldGhost?.before);
  const entityProposed =useProposedChange({ docType: "entity", entity: entity.name });
  const tableGhost = entityProposed?.op === "delete" ? entityProposed : null;
  const related = useRelatedSections(slug, entity, label);
  // Same key as the field editor below: one request, and the number matches its "Trường (n)".
  const fieldCount = useQuery({ queryKey: qk.fields(entity.name), queryFn: () => listFields(entity.name) }).data?.length;

  // Same single usage read as the table list and Vận hành › Dữ liệu; nothing drawn when it has no figure for this table.
  const records = useRecordCounts(slug).data?.get(entity.name);

  // Each press asks the field editor to open its add-field form (it owns that form; inline version is slice f2-add-field-inline).
  const [addSignal, setAddSignal] = useState(0);

  // ── Sửa (in the page) ──
  const [editOpen, setEditOpen] = useState(false);
  const [eeLabel, setEeLabel] = useState("");
  const [eeDesc, setEeDesc] = useState("");
  const [eeLoading, setEeLoading] = useState(false);
  const [eeErr, setEeErr] = useState<string | null>(null);
  // "" = automatic. Only text/email/auto-number fields can name a row.
  const [eeNameField, setEeNameField] = useState("");
  const [eeNameFields, setEeNameFields] = useState<FieldMeta[]>([]);
  const storedOptions = parseEntityOptions(entity.options);
  const storedNameField = typeof storedOptions.labelField === "string" ? storedOptions.labelField : "";
  function startEdit() {
    setEeLabel(label);
    setEeDesc("");
    setEeErr(null);
    setEeNameField(storedNameField);
    setEeNameFields([]);
    listFields(entity.name)
      .then((fs) => {
        const eligible = fs.filter((f) => NAME_FIELD_TYPES.has(f.fieldType));
        setEeNameFields(eligible);
        // A stored choice that no longer names an eligible field (deleted/retyped) reads as Tự động.
        if (storedNameField && !eligible.some((f) => f.name === storedNameField)) setEeNameField("");
      })
      .catch(() => setEeNameFields([]));
    setDelOpen(false);
    setEditOpen(true);
  }
  async function handleUpdate() {
    if (!eeLabel || eeLoading) return;
    setEeLoading(true);
    setEeErr(null);
    try {
      await updateEntity(entity.name, {
        label: eeLabel,
        description: eeDesc || undefined,
        // options replace wholesale server-side: send them (merged) only when the choice changed.
        ...(eeNameField !== storedNameField ? { options: { ...storedOptions, labelField: eeNameField } } : {}),
      });
      // No emitDraftsChanged: PUT /admin/entities/:name applies live even on a delivered project (a table is only ever
      // drafted as a delete), so the pending count has not moved (#46).
      await qc.invalidateQueries({ queryKey: qk.entities() });
      await qc.invalidateQueries({ queryKey: qk.fields(entity.name) }); // isLabel moved with the choice
      await qc.invalidateQueries({ queryKey: ["studio", "count", "entities"] }); // the project's own list/route cache
      setEditOpen(false);
    } catch (e) {
      setEeErr(e instanceof Error ? e.message : t("page.entityHub.editFailed"));
    } finally {
      setEeLoading(false);
    }
  }

  // ── Xoá (in the page, type the table code) ──
  const [delOpen, setDelOpen] = useState(false);
  const [delTyped, setDelTyped] = useState("");
  const [delLoading, setDelLoading] = useState(false);
  const [delErr, setDelErr] = useState<string | null>(null);
  async function handleDelete() {
    if (delTyped !== entity.name || delLoading) return;
    setDelLoading(true);
    setDelErr(null);
    try {
      await deleteEntity(entity.name);
      setDelOpen(false);
      // specs/063 S7-9: on a delivered workspace the table is still there, marked "Sẽ xoá khi xuất bản". The delete
      // itself is done: a failed re-read must not turn it into an error, it just means "not deferred".
      const deferred = await listEntities().then((es) => es.some((e) => e.name === entity.name && e.draftState === "deleted"), () => false);
      if (deferred) {
        toast.info(tf("page.entityHub.deleteDeferred", { name: label }));
        emitDraftsChanged("entity-delete"); // the header's pending count and the publish popover re-read
        await qc.invalidateQueries({ queryKey: qk.entities() });
        await qc.invalidateQueries({ queryKey: ["studio", "count", "entities"] });
        return;
      }
      // Navigate first: the project list is EntityRoute's live query, so refreshing it before leaving would
      // unmount this page into a false "no such table" frame.
      onDeleted();
      await qc.invalidateQueries({ queryKey: qk.entities() });
      await qc.invalidateQueries({ queryKey: ["studio", "count", "entities"] });
    } catch (e) {
      setDelErr(e instanceof Error ? e.message : t("page.entityHub.deleteFailed"));
    } finally {
      setDelLoading(false);
    }
  }

  // Hoàn tác of the pending delete (it IS a pending change; undoing it = discarding that change).
  const [undoBusy, setUndoBusy] = useState(false);
  async function handleUndoDelete() {
    if (!entity.id || undoBusy) return;
    setUndoBusy(true);
    try {
      await undoDraftDelete("entity", entity.id);
      await qc.invalidateQueries({ queryKey: qk.entities() });
      await qc.invalidateQueries({ queryKey: ["studio", "count", "entities"] });
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t("page.entityHub.undoFailed"));
    } finally {
      setUndoBusy(false);
    }
  }

  return (
    <div>
      <GhostLive change={fieldGhost ?? tableGhost} />
      {/* px-4 = the field editor's own inset, so the header, the table and "Bản ghi" share one left edge. */}
      <div className="px-4">
      <p className="slds-m-bottom_small">
        <Link href={studioHref(slug, "dung/du-lieu")}>{`← ${t("studio.entity.back")}`}</Link>
      </p>
      <PageHeader
        variant="record-home"
        icon={{ glyph: Table2, name: "entity", label: t("page.entityHub.objectLabel") }}
        label={t("page.entityHub.objectLabel")}
        title={label}
        actions={[
          <Button key="add" variant="brand" iconLeft={Plus} onClick={() => setAddSignal((n) => n + 1)}>
            {t("studio.build.data.table.addField")}
          </Button>,
          <Menu
            key="more"
            align="right"
            trigger={<ButtonIcon icon={MoreHorizontal} label={t("studio.build.data.table.moreActions")} variant="border-filled" more />}
          >
            <MenuItem label={t("studio.build.data.table.edit")} onSelect={startEdit} />
            <MenuItem
              label={t("studio.build.data.table.delete")}
              onSelect={() => {
                setDelTyped("");
                setDelErr(null);
                setEditOpen(false);
                setDelOpen(true);
              }}
            />
          </Menu>,
        ]}
        details={[
          { label: t("page.entityHub.detailName"), value: entity.name },
          { label: t("page.entityHub.detailTable"), value: entity.tableName },
          ...(fieldCount === undefined ? [] : [{ label: t("studio.build.data.table.fieldCount"), value: String(fieldCount) }]),
          ...(records === undefined ? [] : [{ label: t("studio.build.data.records"), value: records.toLocaleString("vi-VN") }]),
        ]}
      />
      {tableGhost && (
        <p className="slds-m-top_small"><ProposedGhost change={tableGhost} /></p>
      )}
      {entity.draftState === "deleted" && (
        <div className="slds-m-top_small">
          <ScopedNotification theme="warning">
            {t("page.entityHub.deleteOnPublish")}{" "}
            <Button variant="base" disabled={undoBusy} onClick={() => void handleUndoDelete()}>{t("page.entityHub.undoDelete")}</Button>
          </ScopedNotification>
        </div>
      )}
      {isNew && (
        <div className="slds-m-top_small">
          <ScopedNotification theme="success">{fill(t("studio.build.data.table.justAdded"), { label })}</ScopedNotification>
        </div>
      )}
      {editOpen && (
        <form
          aria-label={t("studio.build.data.table.edit")}
          className="slds-m-top_small flex flex-col gap-3"
          onSubmit={(e) => { e.preventDefault(); void handleUpdate(); }}
        >
          <Input label={t("page.entityHub.editLabel")} required value={eeLabel} onChange={(e) => setEeLabel(e.target.value)} />
          <Input label={t("page.entityHub.editDesc")} value={eeDesc} onChange={(e) => setEeDesc(e.target.value)} />
          <Select
            label={t("studio.build.data.labelField.label")}
            help={t("studio.build.data.labelField.help")}
            value={eeNameField}
            placeholder={false}
            options={[
              { value: "", label: t("studio.build.data.labelField.auto") },
              ...eeNameFields.map((f) => ({ value: f.name, label: f.label || f.name })),
            ]}
            onChange={(e) => setEeNameField(e.target.value)}
          />
          {eeErr && <ScopedNotification theme="error">{eeErr}</ScopedNotification>}
          <div className="flex gap-2">
            <Button type="button" onClick={() => setEditOpen(false)}>{t("action.cancel")}</Button>
            <Button variant="brand" type="submit" disabled={eeLoading || !eeLabel}>{t("action.save")}</Button>
          </div>
        </form>
      )}
      {delOpen && (
        <div className="slds-m-top_small">
          <InlineConfirm
            tone="error"
            message={fill(t("page.entityHub.deleteTitle"), { label })}
            confirmLabel={t("page.entityHub.deleteConfirm")}
            busy={delLoading}
            confirmDisabled={delTyped !== entity.name}
            onCancel={() => setDelOpen(false)}
            onConfirm={() => { if (delTyped === entity.name) void handleDelete(); }}
          >
            <p>{fill(t("page.entityHub.deleteBody"), { table: entity.tableName })}</p>
            <Input
              label={fill(t("page.entityHub.deleteTypeName"), { name: entity.name })}
              value={delTyped}
              placeholder={entity.name}
              autoComplete="off"
              spellCheck={false}
              disabled={delLoading}
              onChange={(e) => setDelTyped(e.target.value)}
            />
            {delErr && <ScopedNotification theme="error">{delErr}</ScopedNotification>}
          </InlineConfirm>
        </div>
      )}
      </div>
      <div className="flex flex-col gap-4 lg:flex-row">
        <div className="min-w-0 flex-1">
          {fieldGhost && (
            <p className="px-4 slds-m-bottom_small">
              <ProposedGhost change={fieldGhost}>
                {fieldGhost.op === "update" && newLabel && (
                  <>
                    <strong className="slds-m-left_x-small">{newLabel}</strong>
                    {oldLabel && <span className="slds-m-left_x-small slds-text-color_weak">{tf("studio.build.ai.before", { text: oldLabel })}</span>}
                  </>
                )}
              </ProposedGhost>
            </p>
          )}
          <EntityFieldEditor entityName={entity.name} initialField={field} onSelectField={onFieldChange} addSignal={addSignal} />
          {/* The quick view's own flat section carries the "Bản ghi" heading (Card, embedded). */}
          <div className="slds-m-top_large">
            <EntityRecordsQuickView entityName={entity.name} onAskAssistant={ask ? (id) => ask(entity.name, id) : undefined} />
          </div>
        </div>
        {field === null && <RelatedLists label={t("studio.build.data.related.label")} sections={related} />}
      </div>
    </div>
  );
}
