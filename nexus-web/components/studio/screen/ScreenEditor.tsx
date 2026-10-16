"use client";

// ScreenEditor (specs/075 US1 + US4): edit the FORM (man=form) or the RECORD PAGE (man=ho-so) of one table on the real screen.
// ScreenBar on top, the stage (FormCanvas) in the middle, the properties column on the right (below on a narrow work
// surface). Edits go through lib/screenEditorModel on the lib/layoutDraft draft; Lưu = POST /admin/layouts the first
// time, PUT /admin/layouts/:id after; "Đưa về màn tự sinh" = DELETE /admin/layouts/:id. The server never answers 409
// for a layout; a PUT 404 means the screen was reset elsewhere (offer to save it again as a new one).
import { useEffect, useMemo, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { DndContext, useSensor, useSensors, type DragEndEvent, type DragOverEvent, type DragStartEvent, type UniqueIdentifier } from "@dnd-kit/core";
import { Card } from "@/components/slds/Card";
import { EmptyState } from "@/components/slds/EmptyState";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Button } from "@/components/slds/Button";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { draftStateLabel } from "@/components/studio/StatusText";
import { useDirectApplyGate } from "@/components/studio/useDirectApplyGate";
import { usePublishState } from "@/components/studio/usePublishState";
import { QueryState } from "@/components/studio/QueryState";
import { toast } from "@/components/ui/Toaster";
import { deleteLayout, getTenantBrandingDoc, type EntityMeta, type FieldMeta } from "@/lib/api";
import { classOf } from "@/lib/draftClass";
import { undoDraftDelete } from "@/lib/draftUndo";
import { strictErrorMessage } from "@/lib/errorMessages";
import {
  addSection, draftEquals, draftFromSections, findCell, missingRequired, removeCell, unplacedFields, updateCell,
  type DraftFieldCell, type DraftSection,
} from "@/lib/layoutDraft";
import { clearDependents, dependentsOf } from "@/lib/visibleWhenRules";
import {
  addSpacerCell, moveCellStep, moveCellToSection, moveSectionStep, placeField, removeFieldGuarded, removeSectionGuarded, setCellLabel,
  applyBulkMoveToSection, applyBulkRemove, applyBulkRequired, setRequiredEdit, setSectionColumns, setSectionTitle, withRequiredEdits,
} from "@/lib/screenEditorModel";
import { saveScreen, savedRequiredSummary, type RequiredEdits } from "@/lib/screenSave";
import { dndAccessibility } from "@/lib/dndKeyboard";
import { FORM_SENSORS, UNPLACED_ZONE, applyDrop, dropTarget, formCollision } from "./dnd";
import { SectionProperties, SectionsPanel } from "./SectionProperties";
import { useSetBlocked } from "@/lib/navigation-blocker";
import { t, tf } from "@/lib/i18n";
import { studioHref } from "@/lib/studioRoutes";
import { BulkBar, bulkMessage } from "./BulkBar";
import { BulkFieldTable, NO_SECTION, sectionTitle } from "./BulkFieldTable";
import { FieldProperties } from "./FieldProperties";
import { FormCanvas } from "./FormCanvas";
import { RecordPageCanvas } from "./RecordPageCanvas";
import { RelatedListPane } from "./RelatedListPane";
import { useProposedChange } from "@/components/studio/assistant/ProposalOverlay";
import { GhostLive } from "@/components/studio/assistant/ProposedGhost";
import { screenOverlayFrom } from "@/lib/screenProposalOverlay";
import { ListScreenEditor } from "./ListScreenEditor";
import { ScreenBar, screenLabel, type ScreenMan } from "./ScreenBar";
import { UnplacedFieldList } from "./UnplacedFieldList";
import { editViewOf, useScreenEditorData, type ScreenData } from "./useScreenEditorData";
import { useSurfaceWidth } from "./useSurfaceWidth";

export interface ScreenEditorProps {
  slug: string;
  app?: string | null;
  /** Tables the picker offers (the caller applies the app filter). */
  entities: EntityMeta[];
  bang: string;
  /** `?truong=` already validated by the route. */
  truong: string | null;
  onTruong: (field: string | null) => void;
  /** "form" (default), "ho-so" or "danh-sach" (the list: ListScreenEditor). */
  man?: ScreenMan;
  /** `?lien-quan=` — the related list whose display settings are open (record page only). */
  relId?: string | null;
  onRel?: (id: string | null) => void;
  /** `?che-do=danh-sach` — the bulk list mode instead of the stage. */
  listMode?: boolean;
  onMode?: (list: boolean) => void;
  /** Move to another screen (the editor has already asked about unsaved changes). */
  onPick: (bang: string, man: ScreenMan) => void;
}

export function ScreenEditor(props: ScreenEditorProps) {
  const delivered = usePublishState(props.slug).data?.delivered === true;
  const [live, setLive] = useState(false);
  const q = useScreenEditorData(props.bang, delivered, live && delivered);
  const entity = props.entities.find((e) => e.name === props.bang);
  const empty = (
    <EmptyState
      plain
      title={t("studio.build.screens.empty.noFields.title")}
      description={t("studio.build.screens.empty.noFields.desc")}
      action={<a className="slds-button slds-button_brand" href={studioHref(props.slug, "dung/du-lieu/:bang", { bang: props.bang })}>{t("studio.build.screens.empty.action")}</a>}
    />
  );
  return (
    <QueryState query={{ ...q, data: q.data?.fields }} empty={empty}>
      {q.data && (
        // a fresh server read (after Lưu / reset) re-seeds the draft and the baseline
        props.man === "danh-sach" ? (
          <ListScreenEditor key={`${q.dataUpdatedAt}:${delivered}:${live}`} slug={props.slug} entities={props.entities} bang={props.bang} truong={props.truong} onTruong={props.onTruong}
            onPick={props.onPick} data={q.data} tableLabel={entity?.label || props.bang} refetch={() => q.refetch()} />
        ) : (
          <Editor key={`${q.dataUpdatedAt}:${delivered}:${live}`} {...props} live={live && delivered} onLive={delivered ? setLive : undefined} data={q.data} tableLabel={entity?.label || props.bang} refetch={() => q.refetch()} />
        )
      )}
    </QueryState>
  );
}

function Editor({
  slug, entities, bang, truong, onTruong, onPick, man = "form", relId = null, onRel, listMode = false, onMode, data, tableLabel, refetch, live, onLive,
}: ScreenEditorProps & { live: boolean; onLive?: (v: boolean) => void; data: ScreenData; tableLabel: string; refetch: () => Promise<unknown> }) {
  const { fields: storedFields, record } = data;
  const isRecord = man === "ho-so";
  const { view, isDefault, layoutId: initialLayoutId } = useMemo(() => editViewOf(data.layout, isRecord ? "detail" : "edit"), [data.layout, isRecord]);
  const initial = useMemo(() => draftFromSections(view?.sections), [view]);
  const [draft, setDraft] = useState<DraftSection[]>(initial);
  const [baseline, setBaseline] = useState<DraftSection[]>(initial); // re-seeded by the remount after every server read
  const [layoutId, setLayoutId] = useState(initialLayoutId);
  // "Bắt buộc nhập" edits waiting for Lưu, and the ones a partly failed Lưu already wrote (so the stored flag is not stale)
  const [edits, setEdits] = useState<RequiredEdits>({});
  const [applied, setApplied] = useState<RequiredEdits>({});
  const savedFields = useMemo(() => withRequiredEdits(storedFields, applied), [storedFields, applied]);
  const fields = useMemo(() => withRequiredEdits(savedFields, edits), [savedFields, edits]);
  const [saving, setSaving] = useState(false);
  const [saveErr, setSaveErr] = useState<string | null>(null);
  const [whenErr, setWhenErr] = useState<string | null>(null);
  const [gone, setGone] = useState(false);
  const [lockedMsg, setLockedMsg] = useState<string | null>(null);
  const [removeBlocked, setRemoveBlocked] = useState(false);
  const [pending, setPending] = useState<{ bang: string; man: ScreenMan } | null>(null);
  const [resetAsk, setResetAsk] = useState(false);
  const [sectionSel, setSectionSel] = useState<string | null>(null);
  const [sectionBlocked, setSectionBlocked] = useState(false);
  const [activeId, setActiveId] = useState<UniqueIdentifier | null>(null);
  const [overId, setOverId] = useState<UniqueIdentifier | null>(null);
  const [dropMsg, setDropMsg] = useState("");
  const pointerSensor = useSensor(FORM_SENSORS[0].sensor, FORM_SENSORS[0].options);
  const keyboardSensor = useSensor(FORM_SENSORS[1].sensor, FORM_SENSORS[1].options);
  const sensors = useSensors(pointerSensor, keyboardSensor);
  // The assistant's proposal for this table's form / record page (specs/075 US9); one at a time, null without a provider.
  const layoutProposal = useProposedChange({ docType: "layout", entity: bang });
  const fieldProposal = useProposedChange({ docType: "field", entity: bang });
  const proposal = layoutProposal ?? fieldProposal;
  const overlay = useMemo(
    () => (proposal ? screenOverlayFrom(proposal, isRecord ? "detail" : "edit", (n) => fields.find((f) => f.name === n)?.label || n) : null),
    [proposal, isRecord, fields],
  );
  const labelRef = useRef<HTMLInputElement>(null);
  const qc = useQueryClient();
  // layout is a drafted type: once delivered a Lưu is a draft ("Nháp"), and the read says so through view.draftState.
  // A type the project classes as direct/untraced would apply at once: ask in place first (class from classOf, no table here).
  const publish = usePublishState(slug).data;
  const gate = useDirectApplyGate(slug, "layout", {
    message: tf("studio.build.screens.draft.applyNow", { what: t("studio.build.screens.draft.thisScreen") }),
    confirm: t("studio.build.screens.draft.applyNowGo"),
  });
  const draftLabel = classOf("layout", publish) === "draft" && view?.draftState ? (draftStateLabel("edited")?.label ?? null) : null;
  const setBlocked = useSetBlocked();
  const { ref: surfaceRef, stacked } = useSurfaceWidth();
  const brand = useQuery({ queryKey: ["tenant-branding"], queryFn: getTenantBrandingDoc, staleTime: 60_000 });

  const dirty = !draftEquals(draft, baseline) || Object.keys(edits).length > 0;
  useEffect(() => {
    setBlocked(dirty && !saving);
    return () => setBlocked(false);
  }, [dirty, saving, setBlocked]);

  const fieldByName = useMemo(() => new Map<string, FieldMeta>(fields.map((f) => [f.name, f])), [fields]);
  const unplaced = useMemo(() => unplacedFields(fields, draft), [fields, draft]);
  const missing = useMemo(() => (isRecord ? [] : missingRequired(fields, draft)), [fields, draft, isRecord]);

  // selection: by field name (what ?truong= carries); unknown name = nothing selected (the route dropped it)
  const selectedName = truong && fieldByName.has(truong) ? truong : null;
  const placedCell = useMemo(() => {
    for (const s of draft) {
      const c = s.cells.find((x) => x.kind === "field" && x.field === selectedName);
      if (c && c.kind === "field") return { cell: c as DraftFieldCell, section: s, index: s.cells.indexOf(c) };
    }
    return null;
  }, [draft, selectedName]);
  const unplacedSelected = selectedName && !placedCell ? (fieldByName.get(selectedName) ?? null) : null;
  const si = placedCell ? draft.findIndex((s) => s.id === placedCell.section.id) : -1;
  const canUp = !!placedCell && (placedCell.index > 0 || si > 0);
  const canDown = !!placedCell && (placedCell.index < placedCell.section.cells.length - 1 || si < draft.length - 1);

  const edit = (next: DraftSection[]) => { setDraft(next); setSaveErr(null); setWhenErr(null); setRemoveBlocked(false); setSectionBlocked(false); };
  const sectionPicked = draft.find((x) => x.id === sectionSel) ?? null;
  const sectionIdx = sectionPicked ? draft.indexOf(sectionPicked) : -1;

  // Bắt buộc nhập: a field that is not on the form joins it (last section) so the floor rule holds on the same Lưu
  const setRequired = (f: FieldMeta, on: boolean) => {
    const stored = savedFields.find((x) => x.name === f.name) ?? f;
    setEdits((e) => setRequiredEdit(e, stored, on));
    if (on && !placedCell && !draft.some((s) => s.cells.some((c) => c.kind === "field" && c.field === f.name))) {
      edit(placeField(draft, f.name, null, t("studio.build.screens.stage.newSection")).draft);
    }
  };

  // ── list mode (L3): ticks + the outcome sentence; every edit lands in the same draft/edits as the stage ──
  const [bulkSel, setBulkSel] = useState<string[]>([]);
  const [bulkMsg, setBulkMsg] = useState<string | null>(null);
  const labelOf = (n: string) => fieldByName.get(n)?.label || n;
  // `done` says what was done to `n` fields; with none done only the skipped part is told
  const bulkDone = (done: (n: number) => string, total: number, skipped: Parameters<typeof bulkMessage>[1], clear: boolean) => {
    const n = total - skipped.length;
    setBulkMsg(bulkMessage(n > 0 ? done(n) : null, skipped, labelOf) || null);
    if (clear) setBulkSel([]);
  };
  const bulkRequired = (names: string[], clear: boolean) => {
    const r = applyBulkRequired(draft, savedFields, edits, names, t("studio.build.screens.stage.newSection"));
    edit(r.draft);
    setEdits(r.edits);
    bulkDone((n) => tf("studio.build.screens.bulk.doneRequired", { n }), names.length, r.skipped, clear);
  };
  const rowRequired = (name: string, on: boolean) => {
    const stored = savedFields.find((x) => x.name === name);
    if (on) bulkRequired([name], false);
    else if (stored) { setEdits((e) => setRequiredEdit(e, stored, false)); setBulkMsg(null); }
  };
  const bulkRemove = (names: string[], clear: boolean) => {
    const r = applyBulkRemove(draft, fields, names, { ignoreRequired: isRecord });
    edit(r.draft);
    bulkDone((n) => tf("studio.build.screens.bulk.doneRemove", { n }), names.length, r.skipped, clear);
  };
  const bulkMove = (names: string[], sectionId: string, clear: boolean) => {
    const r = applyBulkMoveToSection(draft, names, sectionId);
    edit(r.draft);
    const sec = r.draft.find((x) => x.id === sectionId);
    bulkDone((n) => tf("studio.build.screens.bulk.doneMove", { n, section: sec ? sectionTitle(sec) : "" }), names.length, r.skipped, clear);
  };

  // ── drag and drop (L1b) ──
  const itemName = (id: UniqueIdentifier): string => {
    const k = String(id);
    if (k.startsWith("u:")) return fieldByName.get(k.slice(2))?.label || k.slice(2);
    const hit = findCell(draft, k);
    if (!hit) return k;
    if (hit.cell.kind === "spacer") return t("studio.build.screens.field.spacer");
    const f = fieldByName.get(hit.cell.field);
    return hit.cell.labelOverride || f?.label || hit.cell.field;
  };
  const targetName = (id: UniqueIdentifier): string => {
    const k = String(id);
    if (k === UNPLACED_ZONE) return t("studio.build.screens.dnd.unplacedZone");
    if (k.startsWith("sec:")) {
      const sec = draft.find((x) => x.id === k.slice(4));
      return tf("studio.build.screens.dnd.sectionTarget", { section: sec?.title.trim() || t("studio.build.screens.stage.sectionUntitled") });
    }
    return tf("studio.build.screens.dnd.cellTarget", { name: itemName(k) });
  };
  const accessibility = {
    ...dndAccessibility(itemName, targetName),
    screenReaderInstructions: { draggable: t("studio.build.screens.dnd.instructions") },
  };
  const onDragStart = (e: DragStartEvent) => { setActiveId(e.active.id); setDropMsg(""); };
  const onDragOver = (e: DragOverEvent) => setOverId(e.over?.id ?? null);
  const onDragEnd = (e: DragEndEvent) => {
    setActiveId(null);
    setOverId(null);
    const r = dropTarget(draft, fields, e.active.id, e.over?.id);
    if (r.kind === "blocked") { setDropMsg(t("studio.build.screens.dnd.blocked")); return; }
    if (r.kind === "none") return;
    const name = itemName(e.active.id);
    const next = applyDrop(draft, r);
    edit(next);
    if (r.kind === "remove") { setDropMsg(tf("studio.build.screens.dnd.removed", { item: name })); return; }
    const moved = r.kind === "move" ? findCell(next, r.cellId) : null;
    const sec = next.find((x) => x.id === r.sectionId);
    const idx = moved ? moved.index + 1 : r.index + 1;
    setDropMsg(tf("studio.build.screens.dnd.moved", { item: name, i: idx, section: sec?.title.trim() || t("studio.build.screens.stage.sectionUntitled") }));
    if (r.kind === "place") onTruong(r.field);
  };
  const onDragCancel = () => { setActiveId(null); setOverId(null); };

  async function save(asNew = false) {
    if (missing.length > 0) {
      setSaveErr(tf("studio.build.screens.error.required", { names: missing.map((f) => f.label || f.name).join(", ") }));
      return;
    }
    setSaving(true);
    setSaveErr(null);
    setWhenErr(null);
    const r = await saveScreen({ entity: bang, viewType: isRecord ? "detail" : "edit", layoutId, asNew, draft, fields: savedFields, edits });
    if (r.ok) {
      setGone(false);
      void qc.invalidateQueries({ queryKey: ["metadata", "layout", bang] });
      void qc.invalidateQueries({ queryKey: ["metadata", "fields", bang] });
      await refetch(); // new dataUpdatedAt remounts this editor on the saved state
      toast.success(t("studio.build.screens.status.ok"));
      return;
    }
    // keep what was not written; what was written is now the stored state
    const wrote = Object.keys(edits).filter((n) => !(n in r.remaining));
    setApplied((a0) => ({ ...a0, ...Object.fromEntries(wrote.map((n) => [n, edits[n]])) }));
    setEdits(r.remaining);
    if (r.layoutId !== layoutId) setLayoutId(r.layoutId);
    if (r.phase === 3) setBaseline(draft); // the layout is written; only some fields are not
    const err = r.error as { code?: string; status?: number; details?: { retryAfter?: unknown } } | null;
    if (r.layoutGone) setGone(true);
    else if (err?.code === "TENANT_MAINTENANCE") {
      const sec = err.details?.retryAfter;
      const at = typeof sec === "number" ? new Date(Date.now() + sec * 1000) : null;
      setLockedMsg(at ? tf("studio.build.screens.bar.locked", { time: at.toLocaleTimeString("vi-VN", { hour: "2-digit", minute: "2-digit" }) }) : t("studio.build.screens.bar.lockedNoTime"));
    } else if (r.phase === 3) setSaveErr(savedRequiredSummary(r, savedFields, strictErrorMessage));
    else {
      if (err?.code === "LAYOUT_VISIBLE_WHEN_INVALID") setWhenErr(strictErrorMessage(r.error));
      setSaveErr(strictErrorMessage(r.error));
    }
    setSaving(false);
  }

  async function reset() {
    if (!layoutId) return;
    setSaving(true);
    try {
      await deleteLayout(layoutId);
      setResetAsk(false);
      await refetch();
    } catch (e) {
      setSaveErr(strictErrorMessage(e));
      setSaving(false);
    }
  }

  const pick = (b: string, m: ScreenMan) => {
    if (b === bang && m === man) return;
    if (dirty) setPending({ bang: b, man: m });
    else onPick(b, m);
  };

  // mouse/touch: the property column takes focus once the selection has drawn it; keyboard: focus stays on the
  // cell (FR-020). The selection round-trips through the URL, so the focus waits for the cell's properties.
  const focusLabelFor = useRef<string | null>(null);
  const placedCellId = placedCell?.cell.id ?? null;
  useEffect(() => {
    if (focusLabelFor.current && selectedName === focusLabelFor.current && placedCellId) {
      labelRef.current?.focus();
      focusLabelFor.current = null;
    }
  }, [selectedName, placedCellId]);
  const select = (name: string, viaPointer: boolean) => {
    if (viaPointer) {
      if (name === selectedName) labelRef.current?.focus();
      else focusLabelFor.current = name;
    }
    if (relId) onRel?.(null);
    onTruong(name);
  };

  const pendingEntity = pending ? entities.find((e) => e.name === pending.bang) : undefined;

  return (
    <div ref={surfaceRef}>
      <ScreenBar
        entities={entities}
        bang={bang}
        man={man}
        missing={isRecord ? null : missing}
        dirty={dirty}
        saving={saving}
        locked={lockedMsg != null || live}
        canReset={layoutId != null}
        draftLabel={live ? null : draftLabel}
        live={live}
        onLive={dirty ? undefined : onLive}
        mode={listMode ? "list" : "canvas"}
        onMode={onMode ? (m) => onMode(m === "list") : undefined}
        onPick={pick}
        onSave={() => gate.ask(() => void save())}
        onDiscard={() => { setDraft(baseline); setEdits({}); setWhenErr(null); setSaveErr(null); setGone(false); setRemoveBlocked(false); }}
        onReset={() => setResetAsk(true)}
      />
      {pending && (
        <InlineConfirm
          tone="warning"
          message={tf("studio.build.screens.bar.leaveAsk", { screen: screenLabel(pendingEntity ?? { name: pending.bang }, pending.man) })}
          confirmLabel={t("studio.build.screens.bar.leaveGo")}
          cancelLabel={t("studio.build.screens.bar.keep")}
          onConfirm={() => { const p = pending; setPending(null); onPick(p.bang, p.man); }}
          onCancel={() => setPending(null)}
        />
      )}
      {resetAsk && (
        <InlineConfirm
          tone="error"
          message={tf("studio.build.screens.bar.resetAsk", { bang: tableLabel })}
          confirmLabel={t("studio.build.screens.bar.resetGo")}
          onConfirm={() => void reset()}
          onCancel={() => setResetAsk(false)}
          busy={saving}
        />
      )}
      {gate.confirm}
      {live && <ScopedNotification theme="info">{t("studio.build.screens.live.note")}</ScopedNotification>}
      {lockedMsg && <ScopedNotification theme="warning">{lockedMsg}</ScopedNotification>}
      {saveErr && <ScopedNotification theme="error">{saveErr}</ScopedNotification>}
      {gone && (
        <ScopedNotification theme="warning">
          <span>{t("studio.build.screens.error.gone")} </span>
          <Button variant="neutral" onClick={() => void save(true)}>{t("studio.build.screens.error.saveAsNew")}</Button>
        </ScopedNotification>
      )}
      {isDefault && !dirty && !live && <p className="slds-text-color_weak slds-m-bottom_small">{t("studio.build.screens.bar.autoNote")}</p>}
      <GhostLive change={overlay ? overlay.change : null} />
      <p className="slds-assistive-text" role="status" aria-live="polite">{dropMsg}</p>
      {dropMsg === t("studio.build.screens.dnd.blocked") && <ScopedNotification theme="warning">{dropMsg}</ScopedNotification>}
      {listMode ? (
        <div data-testid="screen-bulk">
          <BulkBar
            count={bulkSel.length}
            sections={draft.map((x) => ({ id: x.id, title: sectionTitle(x) }))}
            detail={isRecord}
            disabled={lockedMsg != null}
            message={bulkMsg}
            onRequired={() => bulkRequired(bulkSel, true)}
            onRemove={() => bulkRemove(bulkSel, true)}
            onMove={(id) => bulkMove(bulkSel, id, true)}
            onClear={() => { setBulkSel([]); setBulkMsg(null); }}
          />
          <BulkFieldTable
            fields={fields}
            draft={draft}
            selected={bulkSel}
            detail={isRecord}
            disabled={lockedMsg != null}
            onSelect={setBulkSel}
            onSection={(n, id) => { if (id === NO_SECTION) bulkRemove([n], false); else bulkMove([n], id, false); }}
            onRequired={rowRequired}
          />
        </div>
      ) : (
      <DndContext
        sensors={sensors}
        collisionDetection={formCollision}
        accessibility={accessibility}
        onDragStart={onDragStart}
        onDragOver={onDragOver}
        onDragEnd={onDragEnd}
        onDragCancel={onDragCancel}
      >
      <div className={stacked ? "grid grid-cols-1 gap-4" : "grid gap-4"} style={stacked ? undefined : { gridTemplateColumns: "minmax(0,1fr) 300px" }}>
        <div className="slds-theme_shade slds-p-around_medium" data-testid="screen-stage">
          {isRecord ? (
            <RecordPageCanvas
              slug={slug}
              entity={bang}
              tableLabel={tableLabel}
              fields={fields}
              draft={draft}
              record={record}
              appName={brand.data?.branding.appName || t("studio.build.screens.stage.appFallback")}
              accentColor={brand.data?.branding.accentColor}
              logoUrl={brand.data?.branding.logoUrl}
              selected={selectedName}
              relId={relId}
              onRel={(id) => onRel?.(id)}
              labelOf={(n) => entities.find((e) => e.name === n)?.label || n}
              onSelect={select}
              onDeselect={() => onTruong(null)}
              onAddSection={() => edit(addSection(draft, t("studio.build.screens.stage.newSection")))}
              overlay={overlay}
            />
          ) : (
            <FormCanvas
            tableLabel={tableLabel}
            fields={fields}
            draft={draft}
            record={record}
            appName={brand.data?.branding.appName || t("studio.build.screens.stage.appFallback")}
            accentColor={brand.data?.branding.accentColor}
            logoUrl={brand.data?.branding.logoUrl}
            selected={selectedName}
            onSelect={select}
            onDeselect={() => onTruong(null)}
            onAddSection={() => edit(addSection(draft, t("studio.build.screens.stage.newSection")))}
            dragging={activeId != null}
            overId={overId != null ? String(overId) : null}
            onRemoveSpacer={(id) => edit(removeCell(draft, id))}
            overlay={overlay}
          />
          )}
        </div>
        <Card title={t("studio.build.screens.field.title")} className="slds-m-bottom_none">
          {isRecord && relId ? (
            <RelatedListPane
              slug={slug}
              entity={bang}
              relId={relId}
              labelOf={(n) => entities.find((e) => e.name === n)?.label || n}
              tableLabel={tableLabel}
              onGone={() => onRel?.(null)}
            />
          ) : (
          <FieldProperties
            detail={isRecord}
            cell={placedCell?.cell ?? null}
            field={placedCell ? (fieldByName.get(placedCell.cell.field) ?? null) : null}
            unplaced={unplacedSelected}
            sections={draft}
            fields={fields}
            dependents={placedCell ? dependentsOf(draft, placedCell.cell.field) : []}
            whenError={whenErr}
            sectionId={placedCell?.section.id ?? null}
            canUp={canUp}
            canDown={canDown}
            removeBlocked={removeBlocked}
            locked={lockedMsg != null || live}
            requiredOn={!!(placedCell ? fieldByName.get(placedCell.cell.field) : unplacedSelected)?.isRequired}
            onRequired={(on) => { const f = placedCell ? fieldByName.get(placedCell.cell.field) : unplacedSelected; if (f) setRequired(f, on); }}
            tableHref={studioHref(slug, "dung/du-lieu/:bang", { bang }, { truong: selectedName ?? undefined })}
            labelRef={labelRef}
            onLabel={(v) => placedCell && edit(setCellLabel(draft, placedCell.cell.id, v))}
            onUp={() => placedCell && edit(moveCellStep(draft, placedCell.cell.id, -1))}
            onDown={() => placedCell && edit(moveCellStep(draft, placedCell.cell.id, 1))}
            onToSection={(id) => placedCell && edit(moveCellToSection(draft, placedCell.cell.id, id))}
            onWhen={(vw) => placedCell && edit(updateCell(draft, placedCell.cell.id, { visibleWhen: vw }))}
            onRemove={() => {
              if (!placedCell) return;
              if (isRecord) { edit(removeCell(draft, placedCell.cell.id)); return; } // required fields may leave a record page
              const r = removeFieldGuarded(draft, placedCell.cell.id, fields);
              if (r.blocked) setRemoveBlocked(true);
              else edit(clearDependents(r.draft, placedCell.cell.field)); // a condition reading a removed cell would 422 (FR-3)
            }}
            onPlace={() => {
              if (!unplacedSelected) return;
              const r = placeField(draft, unplacedSelected.name, null, t("studio.build.screens.stage.newSection"));
              edit(r.draft);
            }}
            onDone={() => onTruong(null)}
            onOption={(patch) => placedCell && edit(updateCell(draft, placedCell.cell.id, patch))}
            onUndoDelete={() => {
              const id = placedCell?.cell && fieldByName.get(placedCell.cell.field)?.id;
              const target = id ?? unplacedSelected?.id;
              if (!target) return;
              undoDraftDelete("field", target)
                .then(() => refetch())
                .catch((e) => toast.error(tf("studio.build.screens.draft.undoFailed", { msg: strictErrorMessage(e) })));
            }}
          />
          )}
          {!selectedName && draft.length > 0 && !(isRecord && relId) && (
            <div className="slds-m-top_large">
              <SectionsPanel sections={draft} selectedId={sectionPicked?.id ?? null} onPick={setSectionSel} />
              {sectionPicked && (
                <div className="slds-m-top_small">
                  <SectionProperties
                    section={sectionPicked}
                    canUp={sectionIdx > 0}
                    canDown={sectionIdx < draft.length - 1}
                    removeBlocked={sectionBlocked}
                    onTitle={(v) => edit(setSectionTitle(draft, sectionPicked.id, v))}
                    onColumns={(n) => edit(setSectionColumns(draft, sectionPicked.id, n))}
                    onUp={() => edit(moveSectionStep(draft, sectionPicked.id, -1))}
                    onDown={() => edit(moveSectionStep(draft, sectionPicked.id, 1))}
                    onAddSpacer={() => edit(addSpacerCell(draft, sectionPicked.id))}
                    onRemove={() => {
                      const r = removeSectionGuarded(draft, sectionPicked.id, fields);
                      if (r.blocked) setSectionBlocked(true);
                      else { edit(r.draft); setSectionSel(null); }
                    }}
                  />
                </div>
              )}
            </div>
          )}
          {!(isRecord && relId) && <div className="slds-m-top_large">
            <UnplacedFieldList
              fields={unplaced}
              detail={isRecord}
              highlight={unplacedSelected?.name}
              disabled={lockedMsg != null || live}
              dragging={activeId != null}
              onPlace={(name) => {
                const r = placeField(draft, name, placedCell?.section.id ?? null, t("studio.build.screens.stage.newSection"));
                edit(r.draft);
                onTruong(name);
              }}
            />
          </div>}
        </Card>
      </div>
      </DndContext>
      )}
    </div>
  );
}

