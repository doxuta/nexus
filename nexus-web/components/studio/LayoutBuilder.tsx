"use client";

import { useEffect, useMemo, useState } from "react";
import { clsx } from "clsx";
import {
  GripVertical, Lock, RotateCcw, Check, Plus, Eye, Loader2, AlertCircle, Trash2,
  Search as SearchIcon, ArrowUp, ArrowDown, SquareDashed, SlidersHorizontal, RefreshCw, X,
} from "lucide-react";
import {
  DndContext, closestCenter, PointerSensor, useSensor, useSensors, useDroppable, useDraggable,
  type DragEndEvent,
} from "@dnd-kit/core";
import { SortableContext, verticalListSortingStrategy, rectSortingStrategy, useSortable, arrayMove } from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import {
  ensureAuth, listEntities, listFields, getLayout, createLayout, updateLayout, deleteLayout,
  listRecords, ApiError,
  type EntityMeta, type FieldMeta, type EntityLayout, type LayoutSection, type LayoutViewKind,
  type ApiRecord,
} from "@/lib/api";
import {
  draftFromSections, sectionsToJson, addSection, removeSection, moveSection, updateSection,
  addFieldCell, addSpacer, removeCell, moveCell, updateCell, findCell, unplacedFields,
  missingRequired, HELP_TEXT_MAX,
  type DraftSection, type DraftFieldCell, type SectionColumns,
} from "@/lib/layoutDraft";
import { CellRenderer } from "@/components/layout/CellRenderer";
import { SectionGrid } from "@/components/layout/SectionGrid";
import { seedValues } from "@/lib/recordPayload";
import { useNavigationBlocker } from "@/lib/navigation-blocker";
import { Tooltip } from "@/components/ui/Tooltip";
import { FIELD_TYPE_META, type FieldType } from "@/lib/metadata";
import { t } from "@/lib/i18n";
import { scopeEntityOptions } from "@/lib/appScope";

// Layout Builder — W5 v2: WYSIWYG canvas cho Form(edit)/Detail + List config.
//   • palette (trái): field CHƯA đặt — kéo vào canvas (hoặc bấm + để append), spacer cell.
//   • canvas (giữa): section cards, cột 1..4, cell render bằng WIDGET THẬT (CellRenderer/L2)
//     bind bản ghi ĐẦU TIÊN của entity — kéo cell giữa section/vị trí, colspan, chọn để sửa.
//   • rail phải: inspector cell (labelOverride/helpText/readonly/required/colspan) ↔ preview
//     thuần L2 (đúng cái App face sẽ render), toggle được.
// Save = POST/PUT /admin/layouts (v1 grammar; cell demote string↔object transparent —
// lib/layoutDraft). Floor rule (edit): thiếu required field → cảnh báo trước, 422 hiện rõ.
//
// KEY BACKEND CONSTRAINTS (api-conventions §10.4, verified live 2026-07-03):
//   - viewType: "edit" | "detail" | "list" (NOT "form")
//   - layoutJson.version must be 1
//   - list defaultSort.field ∈ created_at|updated_at|id
//   - section.columns: 1..4 (W5 widened) · cell.helpText ≤200 rune (lowercase key)
//   - edit view: all required fields must appear in sections (422 LAYOUT_INVALID)

const TABS = [
  { key: "List" as const, viewType: "list" as const },
  { key: "Detail" as const, viewType: "detail" as const },
  { key: "Form" as const, viewType: "edit" as const },
];
type TabKey = typeof TABS[number]["key"];

const SORT_FIELDS = ["created_at", "updated_at", "id"] as const;
type SortField = typeof SORT_FIELDS[number];

// Tailwind 4 quét class TĨNH — không nội suy col-span-${n}.
const GRID_COLS: Record<SectionColumns, string> = {
  1: "grid-cols-1", 2: "grid-cols-2", 3: "grid-cols-3", 4: "grid-cols-4",
};
const CELL_SPAN: Record<number, string> = {
  1: "col-span-1", 2: "col-span-2", 3: "col-span-3", 4: "col-span-4",
};

const PALETTE_SPACER_ID = "pal:__spacer__";

// entityName: MỘT XƯỞNG Entity Hub "Bố cục" tab (NEXUS-MOT-XUONG-2026-07-29.md §2b) mounts this
// with the table already chosen by the workspace's left tree — the internal entity dropdown +
// below hides itself and every fetch skips straight to that one table. Absent →
// unchanged standalone behaviour (own tenant-wide entity list + dropdown), kept for a component
// that stays "dùng được độc lập".
// initialView: replaces the old self-parsed `?tab=form|list|detail` now that the HUB owns `?tab=`
// for its OWN 4-tab bar — the caller reads the legacy carrier (`?view=`, translated by
// LegacyPanelRedirect from old `?tab=` chain-strip links) and passes the resolved view down.
// hideChrome (specs/075 L1c, TEMPORARY until F3 L6 deletes this file): the screen editor's ScreenBar above already picks
// the table and the view, so this builder drops its own table box and tab strip.
// entityNames: optional soft filter for the standalone entity picker (null/absent = every table).
export function LayoutBuilder({ entityName, initialView, entityNames: scopeEntities = null, hideChrome }: { entityName?: string; initialView?: TabKey; entityNames?: Set<string> | null; hideChrome?: boolean } = {}) {
  // Nước 1 deep-links: land on the right view at mount (the chain strip after "Thêm trường" sends
  // makers here with intent — landing on List when they came to edit the Form was one of the six
  // know-it-or-stall moments). Prefer the explicit prop; URL self-parsing is the standalone-mount
  // fallback (no caller passes ?tab= this way anymore — the hub owns that query key). Read ONCE.
  const [tab, setTab] = useState<TabKey>(() => {
    if (initialView) return initialView;
    if (typeof window === "undefined") return "List";
    const t = new URLSearchParams(window.location.search).get("tab")?.toLowerCase();
    return t === "form" || t === "edit" ? "Form" : t === "detail" ? "Detail" : "List";
  });

  const [entitiesLoading, setEntitiesLoading] = useState(true);
  const [entitiesErr, setEntitiesErr] = useState<string | null>(null);
  const [entities, setEntities] = useState<EntityMeta[]>([]);
  const [selectedEntity, setSelectedEntity] = useState<EntityMeta | null>(null);

  // App-scope (soft filter, INV-2): scoped → entity picker lists only the app's entities;
  // the current selection keeps rendering even when out of scope (no silent blank select).
  const entityOptions = useMemo(
    () => scopeEntityOptions(entities, scopeEntities, selectedEntity?.name),
    [entities, scopeEntities, selectedEntity?.name],
  );

  const [layoutLoading, setLayoutLoading] = useState(false);
  const [layoutErr, setLayoutErr] = useState<string | null>(null);
  const [fields, setFields] = useState<FieldMeta[]>([]);
  const [layout, setLayout] = useState<EntityLayout | null>(null);
  const [layoutIds, setLayoutIds] = useState<Record<string, string>>({});

  // List view state (+ baseline cho dirty check)
  const [listCols, setListCols] = useState<string[]>([]);
  const [listSortField, setListSortField] = useState<SortField>("created_at");
  const [listSortOrder, setListSortOrder] = useState<"asc" | "desc">("desc");
  // Declared view fields (DAILY-QUEUE #5) — "" means "không khai", so the app keeps its
  // first-of-kind guess. Before these existed the Kanban board grouped by whatever picklist came
  // first in metadata and the Calendar used the first date field, with no way to say otherwise.
  const [listKanbanField, setListKanbanField] = useState("");
  const [listCalendarField, setListCalendarField] = useState("");
  const [listBaseline, setListBaseline] = useState("");

  // W5: Detail/Form draft (normalized grid model) + baseline sections cho dirty/restore
  const [draft, setDraft] = useState<DraftSection[]>([]);
  const [baseline, setBaseline] = useState<LayoutSection[]>([]);
  const [selectedCellId, setSelectedCellId] = useState<string | null>(null);
  const [rightTab, setRightTab] = useState<"props" | "preview">("props");
  const [paletteQ, setPaletteQ] = useState("");

  // W5: bản ghi thật đầu tiên cho WYSIWYG + preview (honest empty khi entity chưa có data)
  const [previewRecord, setPreviewRecord] = useState<ApiRecord | null>(null);
  const [previewLoaded, setPreviewLoaded] = useState(false);

  const [saving, setSaving] = useState(false);
  const [saveOk, setSaveOk] = useState(false);
  const [saveErr, setSaveErr] = useState<string | null>(null);
  const [saveConflict, setSaveConflict] = useState(false); // 409 — layout đổi ở nơi khác

  const { setBlocked, confirmLeave } = useNavigationBlocker();

  useEffect(() => {
    (async () => {
      try {
        await ensureAuth();
        if (entityName) {
          // Fixed mode (Entity Hub): the caller already resolved the table — skip the tenant-wide
          // fetch/dropdown entirely. Only `.name` is read anywhere below `selectedEntity` in this
          // component, so a synthetic EntityMeta is honest (not a stand-in for real metadata).
          setSelectedEntity({ name: entityName, label: entityName, tableName: "" });
          return; // `finally` below still runs — entitiesLoading clears either way
        }
        const data = await listEntities();
        setEntities(data);
        if (data.length > 0) {
          // Cold-start: ?entity= (deep-link intent, e.g. the workshop tree/an empty-state link)
          // → scope's first entity → first entity. The old blind data[0] landed on the TENANT's
          // first entity — typically another app's — and the maker edited the wrong layout or
          // had to notice and re-pick. (lib/workingEntity.ts's cross-builder memory was removed
          // in the MỘT XƯỞNG refactor — the URL is the only source of truth now.)
          const want = new URLSearchParams(window.location.search).get("entity");
          const hit = want ? data.find((e) => e.name === want) : undefined;
          const scoped = !hit && scopeEntities ? data.find((e) => scopeEntities.has(e.name)) : undefined;
          setSelectedEntity(hit ?? scoped ?? data[0]);
        }
      } catch (e) {
        setEntitiesErr(e instanceof Error ? e.message : "Lỗi tải danh sách entity");
      } finally {
        setEntitiesLoading(false);
      }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- one-shot mount cold-start
  }, []);

  // Đổi entity → reset trạng thái bố cục ngay trong lúc render ("adjust state on prop change").
  const [loadedEntity, setLoadedEntity] = useState<EntityMeta | null>(null);
  if (selectedEntity !== loadedEntity) {
    setLoadedEntity(selectedEntity);
    if (selectedEntity) {
      setLayoutLoading(true);
      setLayoutErr(null);
      setFields([]);
      setLayout(null);
      setLayoutIds({});
      setSaveOk(false);
      setSaveErr(null);
      setSaveConflict(false);
      setSelectedCellId(null);
      setPreviewRecord(null);
      setPreviewLoaded(false);
    }
  }

  useEffect(() => {
    if (!selectedEntity) return;
    (async () => {
      try {
        const [f, l] = await Promise.all([
          listFields(selectedEntity.name),
          getLayout(selectedEntity.name),
        ]);
        setFields(f);
        setLayout(l);
        const ids: Record<string, string> = {};
        if (l?.views) {
          (Object.entries(l.views) as [string, { id?: string }][]).forEach(([k, v]) => {
            if (v?.id) ids[k] = v.id;
          });
        }
        setLayoutIds(ids);
        seedListState(l, f);
        seedSectionState(l, f, tab === "List" ? "detail" : tabView(tab));
        // Bản ghi mẫu — không chặn UI, honest empty nếu không có
        listRecords(selectedEntity.name, "limit=1")
          .then((rs) => setPreviewRecord(rs[0] ?? null))
          .catch(() => setPreviewRecord(null))
          .finally(() => setPreviewLoaded(true));
      } catch (e) {
        setLayoutErr(e instanceof Error ? e.message : "Lỗi tải bố cục entity");
      } finally {
        setLayoutLoading(false);
      }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selectedEntity]);

  function tabView(t: TabKey): LayoutViewKind { return t === "Form" ? "edit" : t === "Detail" ? "detail" : "list"; }

  // ONE serialized shape for the whole list tab. The dirty check, the seed baseline and the
  // post-save baseline all go through here: a control added to the tab but left out of this
  // snapshot would be invisible to the dirty check, so the maker could change it, navigate away
  // with no unsaved-changes warning, and lose the choice silently.
  function listSnapshot(
    cols: string[], sf: SortField, so: "asc" | "desc", kanban: string, calendar: string,
  ): string {
    return JSON.stringify({ cols, sf, so, kanban, calendar });
  }

  function seedListState(l: EntityLayout | null, f: FieldMeta[]) {
    const listView = l?.views?.list;
    const cols = listView?.columns?.map((c) => c.field) ?? f.filter((x) => !["relation", "polymorphic_relation", "rich_text", "multi_select"].includes(x.fieldType)).slice(0, 5).map((x) => x.name);
    const sf = (listView?.defaultSort?.field ?? "created_at") as SortField;
    const so = listView?.defaultSort?.order ?? "desc";
    const kanban = listView?.kanbanField ?? "";
    const calendar = listView?.calendarField ?? "";
    setListCols(cols);
    setListSortField(sf);
    setListSortOrder(so);
    setListKanbanField(kanban);
    setListCalendarField(calendar);
    setListBaseline(listSnapshot(cols, sf, so, kanban, calendar));
  }

  function seedSectionState(l: EntityLayout | null, f: FieldMeta[], viewKey: LayoutViewKind) {
    const view = l?.views?.[viewKey] ?? l?.views?.detail ?? l?.views?.edit ?? null;
    const sections: LayoutSection[] = view?.sections?.length
      ? view.sections
      : [{ title: "Thông tin cơ bản", columns: 2, fields: f.map((x) => x.name) }];
    setBaseline(sections);
    setDraft(draftFromSections(sections));
  }

  const fieldMap = useMemo(() => new Map<string, FieldMeta>(fields.map((f) => [f.name, f])), [fields]);
  const fieldsByName = useMemo(() => Object.fromEntries(fields.map((f) => [f.name, f])), [fields]);

  // WYSIWYG binds REAL record data to REAL widgets. Detail-mode widgets (FieldDisplay) read the
  // API's stored shape directly (e.g. currency = {amount, currencyCode}) — see LayoutDetail.tsx,
  // no transform there either. Edit-mode widgets (FieldInput) expect the WIDGET-NATIVE shape
  // (currency = decimal string) — the same seedValues() LayoutForm uses for the real create/edit
  // screen. Skipping this for Form mode would show "[object Object]" in a currency box.
  const previewValues = useMemo<Record<string, unknown>>(() => {
    const data = (previewRecord?.data as Record<string, unknown>) ?? undefined;
    if (tab !== "Form") return data ?? {};
    return seedValues(fields, data);
  }, [previewRecord, fields, tab]);

  // ── dirty tracking + navigation guard (B5) ──
  const draftJson = useMemo(() => JSON.stringify(sectionsToJson(draft)), [draft]);
  const baselineJson = useMemo(() => JSON.stringify(sectionsToJson(draftFromSections(baseline))), [baseline]);
  const sectionDirty = tab !== "List" && draftJson !== baselineJson;
  const listDirty = tab === "List" && listBaseline !== "" &&
    listSnapshot(listCols, listSortField, listSortOrder, listKanbanField, listCalendarField) !== listBaseline;
  const dirty = sectionDirty || listDirty;

  useEffect(() => {
    setBlocked(dirty && !saving);
    return () => setBlocked(false);
  }, [dirty, saving, setBlocked]);

  function switchTab(t: TabKey) {
    if (t === tab) return;
    if (dirty && !confirmLeave()) return;
    setTab(t);
    // Re-seed khi đổi tab (đã guard dirty ở trên) — chạy trong handler, không qua effect.
    if (layout || fields.length) {
      if (t === "List") seedListState(layout, fields);
      else seedSectionState(layout, fields, tabView(t));
      setSaveOk(false); setSaveErr(null); setSaveConflict(false);
      setSelectedCellId(null);
    }
  }
  function switchEntity(name: string) {
    if (dirty && !confirmLeave()) return;
    setSelectedEntity(entities.find((en) => en.name === name) ?? null);
  }

  function buildListJson(): Record<string, unknown> {
    return {
      version: 1,
      // No `sortable` here: the builder is a writer, and the server decides that flag at serve
      // time from the field's real sort capability (indexed / unique / relation / system column).
      // This used to flag only created_at/updated_at/id — columns a list layout can never hold —
      // so every saved layout shipped dead headers (DAILY-QUEUE #4).
      columns: listCols.map((name) => {
        const f = fieldMap.get(name);
        return { field: name, labelOverride: f?.label ?? name };
      }),
      defaultSort: { field: listSortField, order: listSortOrder },
      // Omitted (not sent as "") when the maker declares nothing: an absent key is what "keep the
      // app's guess" means on the wire, and the server strict-rejects a blank declaration.
      ...(listKanbanField ? { kanbanField: listKanbanField } : {}),
      ...(listCalendarField ? { calendarField: listCalendarField } : {}),
    };
  }

  async function handleSave() {
    if (!selectedEntity) return;
    setSaving(true); setSaveOk(false); setSaveErr(null); setSaveConflict(false);
    try {
      const viewType = tabView(tab);
      const layoutJson = viewType === "list" ? buildListJson() : sectionsToJson(draft) as unknown as Record<string, unknown>;
      const existingId = layoutIds[viewType];

      if (existingId) {
        await updateLayout(existingId, layoutJson);
      } else {
        const ref = await createLayout({ entityName: selectedEntity.name, viewType, layoutJson, isDefault: false });
        setLayoutIds((prev) => ({ ...prev, [viewType]: ref.id }));
      }
      setSaveOk(true);
      const refreshed = await getLayout(selectedEntity.name);
      setLayout(refreshed);
      // Baseline = trạng thái vừa lưu → hết dirty, guard tự nhả
      if (viewType === "list") {
        setListBaseline(listSnapshot(listCols, listSortField, listSortOrder, listKanbanField, listCalendarField));
      } else {
        setBaseline(sectionsToJson(draft).sections as unknown as LayoutSection[]);
      }
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        setSaveConflict(true);
        setSaveErr(e.message);
      } else {
        setSaveErr(e instanceof Error ? e.message : "Lỗi lưu bố cục");
      }
    } finally {
      setSaving(false);
    }
  }

  async function reloadAfterConflict() {
    if (!selectedEntity) return;
    setSaveConflict(false); setSaveErr(null);
    setLayoutLoading(true);
    try {
      const refreshed = await getLayout(selectedEntity.name);
      setLayout(refreshed);
      if (tab === "List") seedListState(refreshed, fields);
      else seedSectionState(refreshed, fields, tabView(tab));
    } catch (e) {
      setLayoutErr(e instanceof Error ? e.message : "Lỗi tải lại bố cục");
    } finally {
      setLayoutLoading(false);
    }
  }

  async function handleDeleteLayout() {
    const viewType = tabView(tab);
    const existingId = layoutIds[viewType];
    if (!existingId) return;
    setSaving(true); setSaveOk(false); setSaveErr(null);
    try {
      await deleteLayout(existingId);
      setLayoutIds((prev) => { const n = { ...prev }; delete n[viewType]; return n; });
      if (tab === "List") seedListState(null, fields);
      else seedSectionState(null, fields, viewType);
      setSaveOk(true);
    } catch (e) {
      setSaveErr(e instanceof Error ? e.message : "Lỗi xoá bố cục");
    } finally {
      setSaving(false);
    }
  }

  function handleRestore() {
    if (tab === "List") seedListState(layout, fields);
    else setDraft(draftFromSections(baseline));
    setSaveOk(false); setSaveErr(null); setSaveConflict(false);
    setSelectedCellId(null);
  }

  // ── canvas DnD: palette item → section | cell → section/vị trí khác ──
  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 4 } }));
  function onCanvasDragEnd(e: DragEndEvent) {
    const { active, over } = e;
    if (!over) return;
    const activeId = String(active.id);
    const overId = String(over.id);
    if (activeId === overId) return;

    // Xác định đích: một cell (→ section + index của nó) hay một section (→ append).
    let targetSectionId: string | null = null;
    let targetIndex = 0;
    const overCell = findCell(draft, overId);
    if (overCell) {
      targetSectionId = overCell.section.id;
      targetIndex = overCell.index;
    } else if (draft.some((s) => s.id === overId)) {
      targetSectionId = overId;
      targetIndex = draft.find((s) => s.id === overId)!.cells.length;
    }
    if (!targetSectionId) return;

    if (activeId === PALETTE_SPACER_ID) {
      setDraft((d) => addSpacer(d, targetSectionId!, targetIndex));
      return;
    }
    if (activeId.startsWith("pal:")) {
      const fieldName = activeId.slice(4);
      setDraft((d) => addFieldCell(d, targetSectionId!, fieldName, targetIndex));
      return;
    }
    // cell di chuyển (cùng hoặc khác section)
    setDraft((d) => moveCell(d, activeId, targetSectionId!, targetIndex));
  }

  const currentViewType = tabView(tab);
  const hasExistingLayout = !!layoutIds[currentViewType];
  const unplaced = useMemo(() => unplacedFields(fields, draft), [fields, draft]);
  const missingReq = useMemo(
    () => (tab === "Form" ? missingRequired(fields, draft) : []),
    [tab, fields, draft],
  );
  const selectedCell = selectedCellId ? findCell(draft, selectedCellId) : null;

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden">
      {/* Header — flex-wrap: scope chip xuống dòng khi hẹp thay vì tràn */}
      <div className="flex flex-wrap items-center gap-3 border-b border-bd px-5 py-3">
        {hideChrome ? null : entityName ? (
          // Fixed mode: the left tree already IS the entity picker — a second one here would be
          // the exact "hai mô hình trong đầu cho cùng một sự thật" the hub tabs are meant to end.
          <div className="flex h-9 items-center rounded-input border border-bd bg-panel2 px-3 text-body font-medium text-fg">
            {entityName}
          </div>
        ) : entitiesLoading ? (
          <div className="flex h-9 items-center gap-2 rounded-input border border-bd bg-panel2 px-3 text-body text-mut">
            <Loader2 className="size-3.5 animate-spin text-ac" strokeWidth={2} /> Đang tải…
          </div>
        ) : entitiesErr ? (
          <div className="flex h-9 items-center gap-2 rounded-input border border-dg/30 bg-dgd px-3 text-12-5px text-dgf">
            <AlertCircle className="size-3.5 shrink-0" strokeWidth={2} /> {entitiesErr}
          </div>
        ) : entities.length === 0 ? (
          <div className="flex h-9 items-center rounded-input border border-bd bg-panel2 px-3 text-body text-fnt">Chưa có entity nào</div>
        ) : (
          <select value={selectedEntity?.name ?? ""} onChange={(e) => switchEntity(e.target.value)}
            className="h-9 rounded-input border border-bd bg-panel px-2.5 text-body font-medium text-fg">
            {entityOptions.map((en) => <option key={en.name} value={en.name}>{en.label}</option>)}
          </select>
        )}

        {!hideChrome && <div className="flex rounded-input border border-bd bg-panel2 p-0.5">
          {TABS.map((t) => (
            <button key={t.key} onClick={() => switchTab(t.key)}
              className={clsx("rounded-[8px] px-3.5 py-1.5 text-body font-medium transition-colors",
                t.key === tab ? "bg-panel text-fg shadow-card" : "text-mut hover:text-fg")}>
              {t.key}
            </button>
          ))}
        </div>}

        {hasExistingLayout ? (
          <span className="rounded-pill bg-acd px-2 py-0.5 text-caption font-medium text-ac-hi">Đã tuỳ biến</span>
        ) : (
          // Nước 3: the inverse state was invisible — makers hand-built screens the engine had
          // already generated. Say it: defaults are live, customizing is optional.
          <span className="rounded-pill bg-panel2 px-2 py-0.5 text-caption font-medium text-mut" title="Engine đang phục vụ bố cục mặc định sinh tự động cho view này — app khách dùng được ngay, tuỳ chỉnh là tuỳ chọn.">
            Đang dùng bố cục mặc định
          </span>
        )}
        {/* specs/063 S7-3: the saved custom screen is an unpublished draft (the customer still sees the old one). */}
        {layout?.views?.[currentViewType]?.draftState && (
          <span className="rounded-pill bg-wnd px-2 py-0.5 text-caption font-medium text-wnf" title={t("draft.badgeHint")}>
            {t("draft.badge")}
          </span>
        )}
        {dirty && (
          <span className="rounded-pill bg-wnd px-2 py-0.5 text-caption font-medium text-wnf">Chưa lưu</span>
        )}

        <div className="ml-auto flex items-center gap-2">
          {hasExistingLayout && (
            <button onClick={handleDeleteLayout} disabled={saving}
              className="flex h-9 items-center gap-1.5 rounded-input border border-bd bg-panel px-3 text-body font-medium text-dgf hover:bg-dgd disabled:opacity-50">
              <Trash2 className="size-3.5" strokeWidth={1.8} /> Xoá bố cục
            </button>
          )}
          {tab !== "List" && (
            <button onClick={() => setRightTab((v) => (v === "preview" ? "props" : "preview"))}
              className={clsx("flex h-9 items-center gap-1.5 rounded-input border px-3 text-body font-medium transition-colors",
                rightTab === "preview" ? "border-ac bg-acd text-ac-hi" : "border-bd bg-panel text-fg hover:bg-panel2")}>
              <Eye className="size-3.5" strokeWidth={1.8} /> Xem thử
            </button>
          )}
          <button onClick={handleRestore}
            className="flex h-9 items-center gap-1.5 rounded-input px-3 text-body font-medium text-mut hover:bg-panel2">
            <RotateCcw className="size-3.5" strokeWidth={1.8} /> Khôi phục
          </button>
          <button onClick={handleSave} disabled={saving}
            className="flex h-9 items-center gap-1.5 rounded-input bg-ac px-4 text-body font-medium text-acf hover:bg-ac2 disabled:opacity-60">
            {saving ? <Loader2 className="size-4 animate-spin" strokeWidth={2} /> : <Check className="size-4" strokeWidth={2.5} />}
            {saving ? "Đang lưu…" : saveOk && !dirty ? "Đã lưu" : hasExistingLayout ? "Cập nhật" : "Lưu bố cục"}
          </button>
        </div>
      </div>

      {/* Save error / conflict banner — 422 LAYOUT_INVALID hiện nguyên văn, 409 kèm nút tải lại */}
      {saveErr && (
        <div className="flex items-start gap-2 border-b border-dg/30 bg-dgd px-5 py-2">
          <AlertCircle className="mt-0.5 size-3.5 shrink-0 text-dgf" strokeWidth={2} />
          <p className="min-w-0 flex-1 text-12-5px text-dgf">{saveErr}</p>
          {saveConflict && (
            <button onClick={reloadAfterConflict}
              className="flex shrink-0 items-center gap-1 rounded-input border border-dgb/50 px-2 py-1 text-11-5px font-medium text-dgf hover:bg-dg hover:text-white">
              <RefreshCw className="size-3" strokeWidth={2} /> Tải bản mới nhất (bỏ sửa đổi)
            </button>
          )}
        </div>
      )}

      <div className="flex min-h-0 flex-1 overflow-hidden">
        {tab === "List" ? (
          <>
            {/* List tab giữ nguyên: palette thông tin + config */}
            <div className="w-[220px] shrink-0 border-r border-bd bg-panel p-3">
              <p className="mb-2 text-caption font-semibold tracking-wider text-fnt">FIELD KHẢ DỤNG</p>
              {layoutLoading ? (
                <div className="flex items-center gap-1.5 py-2 text-12-5px text-mut">
                  <Loader2 className="size-3.5 animate-spin text-ac" strokeWidth={2} /> Đang tải…
                </div>
              ) : layoutErr ? (
                <p className="text-11-5px text-dgf">{layoutErr}</p>
              ) : fields.length === 0 ? (
                <p className="text-11-5px text-fnt">Chưa có field nào.</p>
              ) : (
                <div className="space-y-1.5">
                  {fields.map((f) => (
                    <div key={f.name} className="flex items-center gap-2 rounded-input border border-bd bg-bg px-2.5 py-1.5 text-12-5px">
                      <span className="flex-1 truncate text-fg">{f.label ?? f.name}</span>
                      <span className="font-mono text-9-5px text-fnt">{f.fieldType}</span>
                    </div>
                  ))}
                </div>
              )}
              <p className="mt-3 text-10-5px text-fnt">relation / rich_text / multi_select bị loại khỏi cột List auto-gen.</p>
              <p className="mt-2 text-10-5px text-wn font-medium">List sort chỉ chấp nhận: created_at / updated_at / id</p>
            </div>
            <div className="min-w-0 flex-1 overflow-y-auto p-5">
              {layoutLoading && (
                <div className="mb-4 flex items-center gap-2 rounded-card border border-bd bg-panel p-4 text-body text-mut">
                  <Loader2 className="size-4 animate-spin text-ac" strokeWidth={2} /> Đang tải bố cục…
                </div>
              )}
              {!layoutLoading && !layoutErr && (
                <ListConfig
                  fields={fields}
                  listCols={listCols} setListCols={setListCols}
                  sortField={listSortField} setSortField={setListSortField}
                  sortOrder={listSortOrder} setSortOrder={setListSortOrder}
                  kanbanField={listKanbanField} setKanbanField={setListKanbanField}
                  calendarField={listCalendarField} setCalendarField={setListCalendarField}
                />
              )}
            </div>
          </>
        ) : (
          <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={onCanvasDragEnd}>
            {/* ── Palette: field chưa đặt (kéo vào canvas) + spacer ── */}
            <div className="flex w-[230px] shrink-0 flex-col border-r border-bd bg-panel">
              <div className="px-3 pb-1 pt-3">
                <p className="mb-2 text-caption font-semibold tracking-wider text-fnt">FIELD CHƯA ĐẶT · {unplaced.length}</p>
                <div className="flex h-8 items-center gap-2 rounded-input border border-bd bg-bg px-2.5 text-sub focus-within:border-ac">
                  <SearchIcon className="size-3.5 shrink-0 text-fnt" strokeWidth={1.8} />
                  <input value={paletteQ} onChange={(e) => setPaletteQ(e.target.value)} placeholder="Tìm field…"
                    className="min-w-0 flex-1 bg-transparent text-fg placeholder:text-fnt focus:outline-none" />
                </div>
              </div>
              <div className="min-h-0 flex-1 space-y-1.5 overflow-y-auto px-3 py-2">
                {layoutLoading ? (
                  <div className="flex items-center gap-1.5 py-2 text-12-5px text-mut">
                    <Loader2 className="size-3.5 animate-spin text-ac" strokeWidth={2} /> Đang tải…
                  </div>
                ) : unplaced.length === 0 ? (
                  <p className="rounded-input border border-dashed border-bd3 px-2.5 py-2 text-11-5px text-fnt">
                    Tất cả field đã có mặt trong bố cục.
                  </p>
                ) : (
                  unplaced
                    .filter((f) => {
                      const q = paletteQ.trim().toLowerCase();
                      return !q || f.name.toLowerCase().includes(q) || (f.label ?? "").toLowerCase().includes(q);
                    })
                    .map((f) => (
                      <PaletteItem key={f.name} f={f}
                        onAdd={() => {
                          // Click = append vào section cuối (kéo-thả cho vị trí chính xác)
                          const last = draft[draft.length - 1];
                          if (last) setDraft((d) => addFieldCell(d, last.id, f.name));
                        }} />
                    ))
                )}
              </div>
              <div className="border-t border-bd2 p-3">
                <PaletteSpacer onAdd={() => {
                  const last = draft[draft.length - 1];
                  if (last) setDraft((d) => addSpacer(d, last.id));
                }} />
                {tab === "Form" && (
                  <p className="mt-2 text-10-5px leading-relaxed text-fnt">
                    Form (edit) bắt buộc chứa MỌI field required — server từ chối bố cục thiếu (422).
                  </p>
                )}
              </div>
            </div>

            {/* ── Canvas ── */}
            <div className="min-w-0 flex-1 overflow-y-auto p-5">
              {layoutLoading && (
                <div className="mb-4 flex items-center gap-2 rounded-card border border-bd bg-panel p-4 text-body text-mut">
                  <Loader2 className="size-4 animate-spin text-ac" strokeWidth={2} /> Đang tải bố cục…
                </div>
              )}
              {layoutErr && !layoutLoading && (
                <div className="mb-4 flex items-start gap-2.5 rounded-card border border-dg/30 bg-dgd p-4">
                  <AlertCircle className="mt-0.5 size-4 shrink-0 text-dgf" strokeWidth={2} />
                  <div>
                    <p className="text-body font-medium text-dgf">Không tải được bố cục</p>
                    <p className="mt-0.5 font-mono text-11-5px text-mut">{layoutErr}</p>
                  </div>
                </div>
              )}

              {!layoutLoading && !layoutErr && (
                <div className="mx-auto max-w-[860px]">
                  {/* Floor rule warning (Form) — cảnh báo TRƯỚC, 422 vẫn hiện rõ nếu cứ lưu */}
                  {missingReq.length > 0 && (
                    <div className="mb-4 rounded-card border border-wn/30 bg-wnd p-3">
                      <p className="text-sub font-medium text-wnf">
                        Thiếu {missingReq.length} field bắt buộc: {missingReq.map((f) => f.label ?? f.name).join(", ")}
                      </p>
                      <p className="mt-0.5 text-10-5px text-mut">
                        Server sẽ từ chối lưu Form thiếu field bắt buộc — kéo chúng từ palette vào một section.
                      </p>
                    </div>
                  )}
                  {previewLoaded && !previewRecord && (
                    <p className="mb-3 text-11-5px text-fnt">
                      Entity chưa có bản ghi — widget hiển thị giá trị trống (không có dữ liệu mẫu để bind).
                    </p>
                  )}

                  <div className="space-y-4">
                    {draft.map((sec, idx) => (
                      <CanvasSection key={sec.id} sec={sec} idx={idx} total={draft.length}
                        mode={tab === "Form" ? "edit" : "detail"}
                        fieldsByName={fieldsByName}
                        values={previewValues}
                        selectedCellId={selectedCellId}
                        onSelectCell={(id) => { setSelectedCellId(id); setRightTab("props"); }}
                        onRemoveCell={(id) => { setDraft((d) => removeCell(d, id)); if (selectedCellId === id) setSelectedCellId(null); }}
                        onTitle={(v) => setDraft((d) => updateSection(d, sec.id, { title: v }))}
                        onColumns={(n) => setDraft((d) => updateSection(d, sec.id, { columns: n }))}
                        onMove={(dir) => setDraft((d) => moveSection(d, sec.id, dir))}
                        onRemove={() => { setDraft((d) => removeSection(d, sec.id)); setSelectedCellId(null); }}
                      />
                    ))}
                    <button onClick={() => setDraft((d) => addSection(d))}
                      className="flex w-full items-center justify-center gap-1.5 rounded-card border border-dashed border-bd3 py-2.5 text-12-5px font-medium text-mut hover:border-ac hover:text-ac">
                      <Plus className="size-3.5" strokeWidth={2} /> Thêm section
                    </button>
                  </div>
                </div>
              )}
            </div>

            {/* ── Rail phải: inspector ↔ preview ── */}
            <div className="flex w-[340px] shrink-0 flex-col border-l border-bd bg-panel">
              <div className="flex items-center gap-1 border-b border-bd px-3 py-2.5">
                {([["props", "Thuộc tính ô", SlidersHorizontal], ["preview", "Xem thử", Eye]] as const).map(([k, label, Icon]) => (
                  <button key={k} onClick={() => setRightTab(k)}
                    className={clsx("flex h-8 items-center gap-1.5 rounded-input px-2.5 text-12-5px font-medium transition-colors",
                      rightTab === k ? "bg-acd text-ac-hi" : "text-mut hover:bg-panel2 hover:text-fg")}>
                    <Icon className="size-3.5" strokeWidth={1.8} /> {label}
                  </button>
                ))}
              </div>
              <div className="min-h-0 flex-1 overflow-y-auto">
                {rightTab === "props" ? (
                  selectedCell ? (
                    <CellInspector
                      key={selectedCell.cell.id}
                      cell={selectedCell.cell}
                      columns={selectedCell.section.columns}
                      meta={selectedCell.cell.kind === "field" ? fieldMap.get(selectedCell.cell.field) : undefined}
                      isFormView={tab === "Form"}
                      onPatch={(patch) => setDraft((d) => updateCell(d, selectedCell.cell.id, patch))}
                      onRemove={() => { setDraft((d) => removeCell(d, selectedCell.cell.id)); setSelectedCellId(null); }}
                      onClose={() => setSelectedCellId(null)}
                    />
                  ) : (
                    <div className="px-4 py-6 text-center">
                      <p className="text-12-5px text-fnt">Bấm vào một ô trên canvas để chỉnh nhãn, trợ giúp, độ rộng…</p>
                    </div>
                  )
                ) : (
                  <PreviewPane
                    draft={draft}
                    mode={tab === "Form" ? "edit" : "detail"}
                    fieldsByName={fieldsByName}
                    record={previewRecord}
                    values={previewValues}
                    loaded={previewLoaded}
                  />
                )}
              </div>
            </div>
          </DndContext>
        )}
      </div>
    </div>
  );
}

// ── Palette ──
function PaletteItem({ f, onAdd }: { f: FieldMeta; onAdd: () => void }) {
  const { attributes, listeners, setNodeRef, transform, isDragging } = useDraggablePalette(`pal:${f.name}`);
  return (
    <div ref={setNodeRef} style={{ transform: CSS.Translate.toString(transform), opacity: isDragging ? 0.4 : 1 }}
      className="group flex items-center gap-1.5 rounded-input border border-bd bg-bg px-2 py-1.5 text-12-5px">
      <button {...attributes} {...listeners} aria-label={`Kéo ${f.label ?? f.name} vào bố cục`}
        className="cursor-grab touch-none text-bd3 hover:text-mut active:cursor-grabbing">
        <GripVertical className="size-3.5" strokeWidth={1.8} />
      </button>
      <span className="min-w-0 flex-1 truncate text-fg">{f.label ?? f.name}</span>
      {f.isRequired && <span className="shrink-0 text-dg" title="Bắt buộc — Form phải chứa field này">*</span>}
      <span className="shrink-0 font-mono text-9-5px text-fnt">{f.fieldType}</span>
      <button onClick={onAdd} aria-label={`Thêm ${f.label ?? f.name} vào section cuối`}
        className="grid size-5 shrink-0 place-items-center rounded-[6px] text-fnt opacity-0 transition-opacity hover:bg-acd hover:text-ac-hi group-hover:opacity-100">
        <Plus className="size-3" strokeWidth={2} />
      </button>
    </div>
  );
}

function PaletteSpacer({ onAdd }: { onAdd: () => void }) {
  const { attributes, listeners, setNodeRef, transform, isDragging } = useDraggablePalette(PALETTE_SPACER_ID);
  return (
    <div ref={setNodeRef} style={{ transform: CSS.Translate.toString(transform), opacity: isDragging ? 0.4 : 1 }}
      className="group flex items-center gap-1.5 rounded-input border border-dashed border-bd3 px-2 py-1.5 text-sub text-mut">
      <button {...attributes} {...listeners} aria-label="Kéo ô trống vào bố cục"
        className="cursor-grab touch-none text-bd3 hover:text-mut active:cursor-grabbing">
        <GripVertical className="size-3.5" strokeWidth={1.8} />
      </button>
      <SquareDashed className="size-3.5 shrink-0" strokeWidth={1.8} />
      <span className="flex-1">Ô trống (spacer)</span>
      <button onClick={onAdd} aria-label="Thêm ô trống vào section cuối"
        className="grid size-5 shrink-0 place-items-center rounded-[6px] text-fnt opacity-0 transition-opacity hover:bg-acd hover:text-ac-hi group-hover:opacity-100">
        <Plus className="size-3" strokeWidth={2} />
      </button>
    </div>
  );
}

// dnd-kit's useDraggable, wrapped so both palette item kinds share one call shape.
function useDraggablePalette(id: string) {
  return useDraggable({ id });
}

// ── Canvas section ──
function CanvasSection({
  sec, idx, total, mode, fieldsByName, values, selectedCellId,
  onSelectCell, onRemoveCell, onTitle, onColumns, onMove, onRemove,
}: {
  sec: DraftSection; idx: number; total: number;
  mode: "edit" | "detail";
  fieldsByName: Record<string, FieldMeta>;
  values: Record<string, unknown>;
  selectedCellId: string | null;
  onSelectCell: (id: string) => void;
  onRemoveCell: (id: string) => void;
  onTitle: (v: string) => void;
  onColumns: (n: SectionColumns) => void;
  onMove: (dir: -1 | 1) => void;
  onRemove: () => void;
}) {
  const { setNodeRef, isOver } = useDroppable({ id: sec.id });
  return (
    <div className={clsx("rounded-card border bg-panel p-4 transition-colors", isOver ? "border-ac" : "border-bd")}>
      <div className="mb-3 flex items-center gap-2 border-b border-bd2 pb-2">
        <input value={sec.title} onChange={(e) => onTitle(e.target.value)} placeholder="Tiêu đề section"
          className="min-w-0 flex-1 bg-transparent font-display text-14px font-semibold text-fg placeholder:text-fnt focus:outline-none" />
        <select value={sec.columns} onChange={(e) => onColumns(parseInt(e.target.value) as SectionColumns)}
          className="h-7 rounded-input border border-bd bg-panel2 px-2 text-sub text-fg">
          {[1, 2, 3, 4].map((n) => <option key={n} value={n}>{n} cột</option>)}
        </select>
        <button onClick={() => onMove(-1)} disabled={idx === 0} aria-label="Chuyển section lên"
          className="grid size-7 place-items-center rounded-input text-fnt hover:bg-panel2 hover:text-fg disabled:opacity-30">
          <ArrowUp className="size-3.5" strokeWidth={1.8} />
        </button>
        <button onClick={() => onMove(1)} disabled={idx === total - 1} aria-label="Chuyển section xuống"
          className="grid size-7 place-items-center rounded-input text-fnt hover:bg-panel2 hover:text-fg disabled:opacity-30">
          <ArrowDown className="size-3.5" strokeWidth={1.8} />
        </button>
        {total > 1 && (
          <Tooltip content="Xoá section — field trong đó quay về palette (chưa xoá gì trên server tới khi Lưu)." side="top">
            <button onClick={onRemove} aria-label="Xoá section"
              className="grid size-7 place-items-center rounded-input text-fnt hover:bg-dgd hover:text-dgf">
              <Trash2 className="size-3.5" strokeWidth={1.8} />
            </button>
          </Tooltip>
        )}
      </div>

      <SortableContext items={sec.cells.map((c) => c.id)} strategy={rectSortingStrategy}>
        <div ref={setNodeRef} className={clsx("grid gap-2.5", GRID_COLS[sec.columns], sec.cells.length === 0 && "min-h-16")}>
          {sec.cells.length === 0 && (
            <p className="col-span-full grid min-h-16 place-items-center rounded-input border border-dashed border-bd3 text-sub text-fnt">
              Kéo field từ palette vào đây
            </p>
          )}
          {sec.cells.map((cell) => (
            <CanvasCell key={cell.id} cell={cell} columns={sec.columns} mode={mode}
              fieldsByName={fieldsByName} values={values}
              selected={selectedCellId === cell.id}
              onSelect={() => onSelectCell(cell.id)}
              onRemove={() => onRemoveCell(cell.id)} />
          ))}
        </div>
      </SortableContext>
    </div>
  );
}

// ── Canvas cell: widget THẬT (CellRenderer) + chrome kéo/xoá/chọn ──
function CanvasCell({
  cell, columns, mode, fieldsByName, values, selected, onSelect, onRemove,
}: {
  cell: DraftSection["cells"][number];
  columns: SectionColumns;
  mode: "edit" | "detail";
  fieldsByName: Record<string, FieldMeta>;
  values: Record<string, unknown>;
  selected: boolean;
  onSelect: () => void;
  onRemove: () => void;
}) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({ id: cell.id });
  const style = { transform: CSS.Transform.toString(transform), transition, opacity: isDragging ? 0.4 : 1 };

  const spanClass = cell.kind === "field" && cell.fullWidth
    ? "col-span-full"
    : cell.kind === "field" && typeof cell.colspan === "number"
      ? CELL_SPAN[Math.min(cell.colspan, columns)] ?? ""
      : "";

  return (
    <div ref={setNodeRef} style={style} className={spanClass}>
      <div onClick={onSelect} role="button" tabIndex={0}
        onKeyDown={(e) => { if (e.key === "Enter") onSelect(); }}
        className={clsx("group relative cursor-pointer rounded-input border bg-bg p-2.5 transition-colors",
          selected ? "border-ac shadow-[0_0_0_1px_var(--ac)]" : "border-bd hover:border-bd3")}>
        {/* chrome: grip + xoá (hiện khi hover/chọn) */}
        <div className={clsx("absolute -top-2.5 right-2 z-10 flex items-center gap-0.5 rounded-badge border border-bd bg-panel px-1 py-0.5 shadow-card transition-opacity",
          selected ? "opacity-100" : "opacity-0 group-hover:opacity-100")}>
          <button {...attributes} {...listeners} aria-label="Kéo để di chuyển ô" onClick={(e) => e.stopPropagation()}
            className="cursor-grab touch-none text-bd3 hover:text-mut active:cursor-grabbing">
            <GripVertical className="size-3.5" strokeWidth={1.8} />
          </button>
          <button onClick={(e) => { e.stopPropagation(); onRemove(); }} aria-label="Bỏ ô khỏi bố cục"
            className="grid size-5 place-items-center rounded-[4px] text-fnt hover:bg-dgd hover:text-dgf">
            <X className="size-3" strokeWidth={2} />
          </button>
        </div>

        {cell.kind === "spacer" ? (
          <div className="grid min-h-12 place-items-center rounded-[6px] border border-dashed border-bd3 text-caption text-fnt">
            <span className="flex items-center gap-1"><SquareDashed className="size-3" strokeWidth={1.8} /> ô trống</span>
          </div>
        ) : fieldsByName[cell.field] ? (
          // WYSIWYG: widget thật, trơ (pointer-events-none) — canvas chỉ để sắp đặt
          <div className="pointer-events-none select-none">
            <CellRenderer
              cell={serializeForRender(cell)}
              mode={mode}
              fieldsByName={fieldsByName}
              value={values[cell.field]}
            />
          </div>
        ) : (
          <div className="rounded-[6px] border border-dashed border-wn/50 bg-wnd px-2.5 py-2 text-11-5px text-wnf">
            Field <span className="font-mono">{cell.field}</span> không còn trong metadata.
          </div>
        )}
      </div>
    </div>
  );
}

// Draft cell → LayoutCell object cho CellRenderer (giữ override để preview trung thực).
function serializeForRender(cell: DraftFieldCell) {
  return {
    field: cell.field,
    labelOverride: cell.labelOverride || undefined,
    required: cell.required,
    readonly: cell.readonly || undefined,
    helpText: cell.helpText || undefined,
  };
}

// ── Cell inspector (rail phải) ──
function CellInspector({
  cell, columns, meta, isFormView, onPatch, onRemove, onClose,
}: {
  cell: DraftSection["cells"][number];
  columns: SectionColumns;
  meta?: FieldMeta;
  isFormView: boolean;
  onPatch: (patch: Partial<Omit<DraftFieldCell, "id" | "kind" | "field">>) => void;
  onRemove: () => void;
  onClose: () => void;
}) {
  if (cell.kind === "spacer") {
    return (
      <div className="space-y-4 px-4 py-4">
        <div className="flex items-center gap-2">
          <SquareDashed className="size-4 text-mut" strokeWidth={1.8} />
          <span className="font-display text-14px font-semibold text-fg">Ô trống</span>
          <button onClick={onClose} className="ml-auto grid size-6 place-items-center rounded-input text-fnt hover:bg-panel2">
            <X className="size-3.5" strokeWidth={2} />
          </button>
        </div>
        <p className="text-sub text-fnt">Giữ chỗ trong lưới để các cột thẳng hàng. Không render gì trên App.</p>
        <button onClick={onRemove}
          className="flex items-center gap-1.5 text-12-5px font-medium text-fnt hover:text-dgf">
          <Trash2 className="size-3.5" strokeWidth={1.8} /> Bỏ ô này
        </button>
      </div>
    );
  }

  const baseRequired = !!meta?.isRequired;
  const typeLabel = meta ? (FIELD_TYPE_META[meta.fieldType as FieldType]?.label ?? meta.fieldType) : "?";
  const helpLen = (cell.helpText ?? "").length;

  return (
    <div className="space-y-4 px-4 py-4">
      <div className="flex items-center gap-2">
        <span className="min-w-0 truncate font-display text-14px font-semibold text-fg">{meta?.label ?? cell.field}</span>
        <span className="shrink-0 rounded-badge bg-panel2 px-1.5 py-0.5 font-mono text-10px text-fnt">{typeLabel}</span>
        <button onClick={onClose} className="ml-auto grid size-6 shrink-0 place-items-center rounded-input text-fnt hover:bg-panel2">
          <X className="size-3.5" strokeWidth={2} />
        </button>
      </div>
      <p className="-mt-3 font-mono text-10-5px text-fnt">{cell.field}</p>

      <div>
        <label className="mb-1 block text-11-5px font-medium text-fnt">Nhãn riêng cho view này</label>
        <input value={cell.labelOverride ?? ""} onChange={(e) => onPatch({ labelOverride: e.target.value || undefined })}
          placeholder={meta?.label ?? cell.field}
          className="h-8 w-full rounded-[6px] border border-bd bg-panel px-2 text-12-5px text-fg placeholder:text-fnt focus:border-ac focus:outline-none" />
      </div>

      <div>
        <label className="mb-1 block text-11-5px font-medium text-fnt">Trợ giúp dưới ô nhập (helpText)</label>
        <textarea value={cell.helpText ?? ""} onChange={(e) => onPatch({ helpText: e.target.value.slice(0, HELP_TEXT_MAX) || undefined })}
          rows={2} placeholder="Ví dụ: dùng email công ty, không dùng gmail cá nhân"
          className="w-full resize-none rounded-[6px] border border-bd bg-panel px-2 py-1.5 text-12-5px text-fg placeholder:text-fnt focus:border-ac focus:outline-none" />
        <p className={clsx("mt-0.5 text-right font-mono text-10px", helpLen >= HELP_TEXT_MAX ? "text-wnf" : "text-fnt")}>{helpLen}/{HELP_TEXT_MAX}</p>
      </div>

      {/* Độ rộng: colspan 1..columns hoặc full width (loại trừ nhau) */}
      <div>
        <label className="mb-1 block text-11-5px font-medium text-fnt">Độ rộng</label>
        <div className="flex items-center gap-1.5">
          {Array.from({ length: columns }, (_, i) => i + 1).map((n) => (
            <button key={n} onClick={() => onPatch({ fullWidth: undefined, colspan: n === 1 ? undefined : n })}
              className={clsx("h-7 flex-1 rounded-[6px] border text-11-5px font-medium transition-colors",
                !cell.fullWidth && (cell.colspan ?? 1) === n ? "border-ac bg-acd text-ac-hi" : "border-bd text-mut hover:bg-panel2")}>
              {n}
            </button>
          ))}
          <button onClick={() => onPatch({ fullWidth: !cell.fullWidth || undefined, colspan: undefined })}
            className={clsx("h-7 flex-1 rounded-[6px] border text-11-5px font-medium transition-colors",
              cell.fullWidth ? "border-ac bg-acd text-ac-hi" : "border-bd text-mut hover:bg-panel2")}>
            Full
          </button>
        </div>
        <p className="mt-1 text-10-5px text-fnt">Số cột ô chiếm trong section {columns} cột.</p>
      </div>

      {isFormView && (
        <div className="space-y-2.5">
          <button type="button" onClick={() => onPatch({ readonly: !cell.readonly || undefined })}
            className="flex items-center gap-2 text-12-5px text-fg">
            <span className={clsx("relative h-5 w-9 shrink-0 rounded-full transition-colors", cell.readonly ? "bg-ac" : "bg-bd3")}>
              <span className={clsx("absolute top-0.5 size-4 rounded-full bg-white transition-all", cell.readonly ? "left-[18px]" : "left-0.5")} />
            </span>
            Chỉ đọc trên form này
          </button>
          {baseRequired ? (
            <div className="flex items-center gap-2 text-sub text-mut">
              <Lock className="size-3.5 shrink-0 text-bd3" strokeWidth={1.8} />
              Bắt buộc (từ Mô hình dữ liệu) — layout chỉ được SIẾT thêm, không nới.
            </div>
          ) : (
            <button type="button" onClick={() => onPatch({ required: cell.required ? undefined : true })}
              className="flex items-center gap-2 text-12-5px text-fg">
              <span className={clsx("relative h-5 w-9 shrink-0 rounded-full transition-colors", cell.required ? "bg-ac" : "bg-bd3")}>
                <span className={clsx("absolute top-0.5 size-4 rounded-full bg-white transition-all", cell.required ? "left-[18px]" : "left-0.5")} />
              </span>
              Bắt buộc riêng trên form này
            </button>
          )}
        </div>
      )}

      <div className="border-t border-bd2 pt-3">
        <button onClick={onRemove}
          className="flex items-center gap-1.5 text-12-5px font-medium text-fnt hover:text-dgf">
          <Trash2 className="size-3.5" strokeWidth={1.8} /> Bỏ khỏi bố cục
        </button>
      </div>
    </div>
  );
}

// ── Preview pane: draft hiện tại render qua ĐÚNG L2 renderer (SectionGrid → CellRenderer →
// FieldInput/FieldDisplay) bind bản ghi thật — cái admin thấy = cái App face sẽ render. ──
function PreviewPane({
  draft, mode, fieldsByName, record, values, loaded,
}: {
  draft: DraftSection[];
  mode: "edit" | "detail";
  fieldsByName: Record<string, FieldMeta>;
  record: ApiRecord | null;
  values: Record<string, unknown>;
  loaded: boolean;
}) {
  const sections = sectionsToJson(draft).sections as unknown as LayoutSection[];
  return (
    <div className="px-4 py-4">
      {!loaded ? (
        <div className="flex items-center gap-2 text-12-5px text-mut">
          <Loader2 className="size-3.5 animate-spin text-ac" strokeWidth={2} /> Đang tải bản ghi mẫu…
        </div>
      ) : record ? (
        <p className="mb-3 text-caption text-fnt">
          Render thật (L2) trên bản ghi <span className="font-mono">{record.id.slice(0, 8)}…</span> — chỉ xem, không sửa được.
        </p>
      ) : (
        <p className="mb-3 text-caption text-fnt">
          Entity chưa có bản ghi — render khung với giá trị trống (không bịa dữ liệu mẫu).
        </p>
      )}
      <div className="pointer-events-none select-none" aria-label="Xem thử bố cục (chỉ đọc)">
        <SectionGrid
          sections={sections}
          mode={mode}
          fieldsByName={fieldsByName}
          values={values}
        />
      </div>
    </div>
  );
}

// ── List tab config (giữ từ v1 — drag reorder cột đã hoạt động) ──
function SortableCol({ f, onRemove }: { f: FieldMeta; onRemove: () => void }) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({ id: f.name });
  const style = { transform: CSS.Transform.toString(transform), transition, opacity: isDragging ? 0.5 : 1 };
  return (
    <div ref={setNodeRef} style={style} className="flex items-center gap-3 border-b border-bd2 bg-panel px-3 py-2.5 text-12-5px last:border-0">
      <button {...attributes} {...listeners} aria-label="Kéo để sắp xếp" className="cursor-grab touch-none text-bd3 hover:text-mut active:cursor-grabbing">
        <GripVertical className="size-4" strokeWidth={1.8} />
      </button>
      <span className="flex-1 font-medium text-fg">{f.label ?? f.name} <span className="font-mono text-10px text-fnt">{f.name}</span></span>
      <span className="w-28 font-mono text-10-5px text-fnt">{f.fieldType}</span>
      <button onClick={onRemove} aria-label="Xoá cột" className="text-fnt hover:text-dgf"><Trash2 className="size-3.5" strokeWidth={1.8} /></button>
    </div>
  );
}

function ListConfig({
  fields, listCols, setListCols, sortField, setSortField, sortOrder, setSortOrder,
  kanbanField, setKanbanField, calendarField, setCalendarField,
}: {
  fields: FieldMeta[];
  listCols: string[]; setListCols: (v: string[]) => void;
  sortField: SortField; setSortField: (v: SortField) => void;
  sortOrder: "asc" | "desc"; setSortOrder: (v: "asc" | "desc") => void;
  kanbanField: string; setKanbanField: (v: string) => void;
  calendarField: string; setCalendarField: (v: string) => void;
}) {
  // The two option lists mirror what each view can actually render — offering a text field for the
  // Kanban column would just earn a 422 from the layout validator.
  const picklistFields = fields.filter((f) => f.fieldType === "picklist");
  const dateFields = fields.filter((f) => f.fieldType === "date" || f.fieldType === "datetime");

  // A declaration saved earlier can stop fitting: the field was deleted, or deleted and recreated
  // with another type. Without this the <select> would render BLANK — identical to "Tự đoán" — and
  // every later save of this layout would 422 naming a JSON key the maker never typed. Show the
  // stale name for what it is so the choice to drop it is theirs, made with eyes open.
  const staleOption = (declared: string, usable: FieldMeta[]) =>
    declared && !usable.some((f) => f.name === declared)
      ? <option value={declared}>{declared} — không dùng được nữa</option>
      : null;
  const available = fields.filter((f) => !["relation", "polymorphic_relation", "rich_text", "multi_select"].includes(f.fieldType));
  const inList = listCols.map((name) => fields.find((f) => f.name === name)).filter(Boolean) as FieldMeta[];
  const notInList = available.filter((f) => !listCols.includes(f.name));

  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 4 } }));
  function onColDragEnd(e: DragEndEvent) {
    const { active, over } = e;
    if (!over || active.id === over.id) return;
    const from = listCols.indexOf(String(active.id));
    const to = listCols.indexOf(String(over.id));
    if (from >= 0 && to >= 0) setListCols(arrayMove(listCols, from, to));
  }

  return (
    <>
      <p className="mb-3 text-12-5px text-mut">Cấu hình cột lưới. Cột sort chỉ nhận: created_at / updated_at / id.</p>

      <div className="mb-4 flex items-center gap-3 rounded-card border border-bd bg-panel p-3">
        <span className="text-sub font-semibold text-fg">Sort mặc định:</span>
        <select value={sortField} onChange={(e) => setSortField(e.target.value as SortField)}
          className="h-8 rounded-input border border-bd bg-panel2 px-2 text-12-5px text-fg">
          {SORT_FIELDS.map((f) => <option key={f} value={f}>{f}</option>)}
        </select>
        <select value={sortOrder} onChange={(e) => setSortOrder(e.target.value as "asc" | "desc")}
          className="h-8 rounded-input border border-bd bg-panel2 px-2 text-12-5px text-fg">
          <option value="asc">Tăng dần</option>
          <option value="desc">Giảm dần</option>
        </select>
      </div>

      <div className="mb-4 rounded-card border border-bd bg-panel p-3">
        <p className="mb-2 text-sub font-semibold text-fg">Kanban &amp; Lịch</p>
        <p className="mb-3 text-12-5px text-mut">
          Chưa khai thì app tự đoán: Kanban lấy picklist đầu tiên, Lịch lấy trường ngày đầu tiên —
          thường không đúng nghiệp vụ.
        </p>
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
          <label className="flex items-center gap-2 text-sub text-fg">
            Cột Kanban
            <select value={kanbanField} onChange={(e) => setKanbanField(e.target.value)}
              disabled={picklistFields.length === 0 && !kanbanField}
              className="h-8 rounded-input border border-bd bg-panel2 px-2 text-12-5px text-fg disabled:opacity-60">
              <option value="">Tự đoán</option>
              {staleOption(kanbanField, picklistFields)}
              {picklistFields.map((f) => <option key={f.name} value={f.name}>{f.label ?? f.name}</option>)}
            </select>
          </label>
          <label className="flex items-center gap-2 text-sub text-fg">
            Trường ngày cho Lịch
            <select value={calendarField} onChange={(e) => setCalendarField(e.target.value)}
              disabled={dateFields.length === 0 && !calendarField}
              className="h-8 rounded-input border border-bd bg-panel2 px-2 text-12-5px text-fg disabled:opacity-60">
              <option value="">Tự đoán</option>
              {staleOption(calendarField, dateFields)}
              {dateFields.map((f) => <option key={f.name} value={f.name}>{f.label ?? f.name}</option>)}
            </select>
          </label>
        </div>
        {picklistFields.length === 0 && (
          <p className="mt-2 text-12-5px text-fnt">Bảng này chưa có trường picklist nào để dựng Kanban.</p>
        )}
        {dateFields.length === 0 && (
          <p className="mt-2 text-12-5px text-fnt">Bảng này chưa có trường ngày nào để dựng Lịch.</p>
        )}
      </div>

      <div className="mb-3 overflow-hidden rounded-card border border-bd">
        <div className="flex items-center gap-3 border-b border-bd bg-panel2 px-3 py-2 text-caption font-semibold tracking-wide text-fnt">
          <span className="size-4 shrink-0" />
          <span className="flex-1">CỘT</span>
          <span className="w-28">LOẠI</span>
          <button onClick={() => setListCols([])} className="text-10-5px font-medium text-dgf hover:underline">Xoá tất cả</button>
        </div>
        {inList.length === 0 ? (
          <div className="px-3 py-4 text-center text-12-5px text-fnt">Chưa có cột nào — thêm từ danh sách bên dưới.</div>
        ) : (
          <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={onColDragEnd}>
            <SortableContext items={listCols} strategy={verticalListSortingStrategy}>
              {inList.map((f) => (
                <SortableCol key={f.name} f={f} onRemove={() => setListCols(listCols.filter((c) => c !== f.name))} />
              ))}
            </SortableContext>
          </DndContext>
        )}
      </div>

      {notInList.length > 0 && (
        <div>
          <p className="mb-1.5 text-caption font-semibold tracking-wider text-fnt">THÊM CỘT</p>
          <div className="flex flex-wrap gap-1.5">
            {notInList.map((f) => (
              <button key={f.name} onClick={() => setListCols([...listCols, f.name])}
                className="flex items-center gap-1.5 rounded-input border border-dashed border-bd3 px-2.5 py-1.5 text-sub text-mut hover:border-ac hover:text-ac">
                <Plus className="size-3" strokeWidth={2} /> {f.label ?? f.name}
              </button>
            ))}
          </div>
        </div>
      )}
    </>
  );
}
