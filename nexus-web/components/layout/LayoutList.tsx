"use client";

// LayoutList — the dynamic record LIST view (doc ../specs/028-layout-l2-renderer/spec.md §4.3, EPIC 2.5 T6)
// on SLDS 2.264.1 (specs/038 T022). Design read: object home for the customer face — blueprints
// page-headers/object-home (the ObjectHomePage template) + data-tables/advanced (components/slds
// DataTable) + panels/filtering (kit FilterPanel via ListFilters) + menus/dropdown (saved views,
// list settings) + modals / prompt (mass edit, quick create, mass delete) + illustration (empty).
//
// Renders `layout.views.list.columns[]` (column ORDER, WIDTH, SORTABLE flag) + `defaultSort`, with
// NO per-entity hard-coding. Cell values go through FieldDisplay (the same read-mode renderer the
// detail view uses), so currency/date/relation/picklist all format correctly. The record name (the
// row header) links to the detail.
//
//   • Columns come from the layout, in layout order, each at its declared width; the field type/label
//     is looked up from listFields() metadata (the layout references fields by name only).
//   • Sorting: defaultSort drives the initial server sort (?sort=&order=). Clicking a SORTABLE column
//     header toggles asc/desc and re-fetches from the server (header carries aria-sort). `sortable` is
//     stamped by the SERVER at serve time from the field's real sort capability (indexed / unique /
//     relation / system column — the same allowlist `?sort=` enforces), so a clickable header is one
//     the engine answers 200 to.
//   • Filters: 11 operators per field type; emits ?field[op]=value URL params (lib/filterModel).
//   • Full-text search: ?q= debounced 350ms; triggers a fresh first page.
//   • Pagination (FE-cursor-bootstrap-a, specs/039): a sort on a keyset-able column (system column,
//     relation, unique, indexed — lib/pagination choosePaginationMode) pages by CURSOR: page 1 is
//     `paginate=cursor`, "Tải thêm" sends `after=<nextCursor>` with the same sort, so it runs past the
//     offset wall (page 1001 → 422 PAGE_TOO_DEEP) and never skips/repeats rows while others insert.
//     Any other sort (or none) stays OFFSET (?page=). A new sort / search / filter starts a new chain.
//     Cursor pages carry no total: the header says "50+ mục" while more pages exist (Lightning's own
//     wording), the exact count once the chain ends.
//   • Bulk bar: select rows → mass edit, mass delete, export.
//   • Density: compact / normal / relaxed row padding.
//   • View modes: table (default), kanban (drag-to-move via KanbanBoard), calendar (month grid
//     with its own range query + drag-to-reschedule + click-day quick-create via CalendarView).
//   • Export CSV: GET /:entity/export.csv with the current filters (Bearer header, blob download).

import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import type { ColumnSizingState, RowSelectionState, SortingState } from "@tanstack/react-table";
import clsx from "clsx";
import Link from "next/link";
import { ChevronDown, Database, Download, Inbox, PenLine, Settings2, Trash2, X } from "lucide-react";
import {
  ensureAuth,
  getLayout,
  listFields,
  bulkDeleteRecords,
  MassOpStoppedError,
  exportCsv,
  ApiError,
  type EntityLayout,
  type FieldMeta,
  type ApiRecord,
  type LayoutListColumn,
  type LayoutDefaultSort,
  type MassOpResult,
} from "@/lib/api";
import { useEntities } from "@/lib/metadata/hooks";
import { usePermissions } from "@/lib/metadata/permissions";
import { isExpandableType } from "@/lib/metadata";
import { t } from "@/lib/i18n";
import { toast } from "@/components/ui/Toaster";
import { buildListGridColumns, toDataTableColumns } from "@/components/layout/listColumns";
import { ListFilterDock, ListFilterPills } from "@/components/layout/ListFilters";
import { KanbanBoard } from "@/components/layout/KanbanBoard";
import { CalendarView } from "@/components/layout/CalendarView";
import { MassEditModal } from "@/components/layout/MassEditModal";
import { SavedViews, type SavedViewStatus } from "@/components/layout/SavedViews";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { DataTable } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { Menu, MenuCheckboxItem, MenuDivider, MenuGroup } from "@/components/slds/Menu";
import { Prompt } from "@/components/slds/Modal";
import { useEmbedded } from "@/components/slds/EmbeddedSurface";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { MassEditBand } from "@/components/studio/operate/MassEditBand";
import { useScopeGate } from "@/components/studio/operate/useScopeGate";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { ObjectHomePage } from "@/components/slds/templates/ObjectHomePage";
import { filtersToQuery, withOwnerFilter } from "@/lib/filterModel";
import type { FilterRow } from "@/lib/filterModel";
import {
  parseListSearch, urlStateIsExplicit, buildListSearch,
  resolveInitialModes, loadListPrefs, saveListPrefs,
} from "@/lib/listState";
import { buildListPageQuery, choosePaginationMode, fetchListPage, type ListPageResult, type PageMode } from "@/lib/pagination";
import { loadColWidths, saveColWidths } from "@/lib/dataGrid";
import type { ViewState } from "@/lib/viewConfig";
import { aclHiddenChipLabel } from "@/lib/hiddenColumns";
import { formatSaveError } from "@/lib/saveErrors";

type LoadState = "loading" | "ok" | "error";
type ViewMode = "table" | "kanban" | "calendar";
type Density = "compact" | "normal" | "relaxed";

const PAGE_LIMIT = 50;

// SLDS 2.264.1 tables have ONE row height (cells padded 0.25rem, ≈32px rows) — Lightning's
// Comfy/Compact switch is an org setting, not a class. Density therefore pads body cells with
// layout utilities: compact = the SLDS row itself, normal/relaxed add vertical padding (≈40/48px,
// the old --row-cozy/--row-comfy heights).
const DENSITY_PADDING: Record<Density, string | undefined> = {
  compact: undefined,
  normal: "[&_tbody_:is(td,th)]:py-2",
  relaxed: "[&_tbody_:is(td,th)]:py-3",
};

// Filter model + UI live in lib/filterModel.ts + ListFilters. `filtersToQuery` and `FilterRow` are
// re-exported from this module so the ?field[op]=value contract keeps its historical import path —
// lib/slice2.test.ts and any deep-link builder import them from "@/components/layout/LayoutList".
export { filtersToQuery };
export type { FilterRow };

export interface LayoutListProps {
  entity: string;
  onOpen?: (id: string) => void;
  onCreate?: () => void;
  /** Client navigation (guarded router) for header links: import, batch create, recycle bin. */
  onNavigate?: (href: string) => void;
  /** Calendar view: create from a clicked day on a full page instead of the quick-create modal (Studio frame). */
  onCreateAt?: (field: string, value: string) => void;
}

interface SortState { field: string; order: "asc" | "desc" }

// Columns whose value is a bare id and must be sent through ?expand= to become a human label.
// Membership lives in ONE place (lib/metadata isExpandableType) shared with LayoutDetail — this
// list and the detail one drifted apart before and both missed `user` fields entirely.
function relationColumns(
  cols: LayoutListColumn[] | undefined,
  fields: FieldMeta[],
): string[] {
  const byName = new Map(fields.map((f) => [f.name, f]));
  return (cols ?? [])
    .filter((c) => isExpandableType(byName.get(c.field)?.fieldType))
    .map((c) => c.field);
}

const fill = (key: Parameters<typeof t>[0], values: Record<string, string | number>) =>
  t(key).replace(/\{(\w+)\}/g, (m, k: string) => (k in values ? String(values[k]) : m));

// URL state (filters `?field[op]=value` + q + sort/order + view) is read ONLY inside the client
// mount effect via lib/listState's parseListSearch (never during SSR → no hydration mismatch),
// and written back shallowly (history.replaceState) by the debounced effect below — F7 fix for
// "list amnesia": back/refresh/share land on the same filters/search/sort/view.

export function LayoutList({ entity, onOpen, onCreate, onNavigate, onCreateAt }: LayoutListProps) {
  // A view-only employee was shown Tạo mới / Thêm nhiều / Nhập dữ liệu / Sửa hàng loạt / Xoá hàng
  // loạt and got a 403 on every one of them. Offering an action that always fails is worse than
  // not offering it: it reads as a broken app rather than as a boundary.
  const { can: canDo } = usePermissions();
  const mayCreate = canDo("create", entity);
  const mayEdit = canDo("edit", entity);
  const mayDelete = canDo("delete", entity);

  const [loadState, setLoadState] = useState<LoadState>("loading");
  const [errMsg, setErrMsg] = useState("");
  const [layout, setLayout] = useState<EntityLayout | null>(null);
  const [fields, setFields] = useState<FieldMeta[]>([]);
  const [records, setRecords] = useState<ApiRecord[]>([]);

  // Pagination: the chain's mode, what to ask for next (page number / cursor; null = end) and the
  // server total (null in cursor mode — keyset pages never count).
  const [pageMode, setPageMode] = useState<PageMode>("offset");
  const [next, setNext] = useState<number | string | null>(null);
  const [total, setTotal] = useState<number | null>(0);
  const [loadingMore, setLoadingMore] = useState(false);
  const hasMore = next !== null;

  // HIDDEN-COL-SIGNAL (api-conventions §14): count of this entity's fields hidden from the
  // CALLER's role by field-ACL — from the additive `meta.hiddenColumns` on the list envelope.
  // 0 (note absent) when the server omits meta: admins and roles with nothing hidden. This is
  // ACL-driven and DISTINCT from the user's own hide/show column menu (`hiddenCols` below).
  const [aclHiddenCount, setAclHiddenCount] = useState(0);

  // Sort.
  const [sort, setSort] = useState<SortState | null>(null);
  const [sortErr, setSortErr] = useState(""); // soft note when a column can't be sorted (keeps rows)
  // Did the URL we arrived on name sort/order itself? Kept so canonicalization preserves an
  // explicit choice instead of collapsing it into "same as the layout default".
  const [urlSortExplicit, setUrlSortExplicit] = useState(false);
  const reqIdRef = useRef(0);

  // Search.
  const [searchRaw, setSearchRaw] = useState("");
  const [searchQ, setSearchQ] = useState(""); // debounced

  // Filters. `filters` updates on every keystroke (keeps the inputs responsive); `debouncedFilters`
  // lags 300ms and is what actually drives the fetch — so typing a value fires ONE request with the
  // full value, not one per character (R2-02).
  const [filters, setFilters] = useState<FilterRow[]>([]);
  const [debouncedFilters, setDebouncedFilters] = useState<FilterRow[]>([]);
  const [showFilterPanel, setShowFilterPanel] = useState(false);
  const filterPanelId = useId();

  // View mode + density. Defaults here; the mount effect restores per-entity persisted prefs
  // (URL `view=` param wins) — restore must NOT happen in an initializer (SSR/hydration).
  const [viewMode, setViewMode] = useState<ViewMode>("table");
  const [density, setDensity] = useState<Density>("normal");
  // "Chủ sở hữu" column, per-user + per-entity. owner_id is a SYSTEM column, never a sys_field, so
  // the layout config has no way to express it — without this toggle the column that answers
  // "ai phụ trách" exists in code and is reachable by nobody.
  const [showOwner, setShowOwner] = useState(false);

  // Did the incoming URL carry explicit list state (deep link / drill-through)? Computed in the
  // mount effect; SavedViews suppresses its last-applied auto-apply when true (URL wins).
  const [urlHadExplicitState, setUrlHadExplicitState] = useState(false);
  const [viewStatus, setViewStatus] = useState<SavedViewStatus>({ name: null, dirty: false });

  // Bulk selection + mass ops (ENG-MASSOP: server-side bulk-update / bulk-delete).
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [bulkDeleting, setBulkDeleting] = useState(false);
  // massEditIds is a SNAPSHOT of the selection taken when the modal opens, so the post-apply
  // list refresh (which clears `selected`) doesn't blank the modal's context mid-result.
  const [massEditIds, setMassEditIds] = useState<string[]>([]);
  const [massEditOpen, setMassEditOpen] = useState(false);
  const [confirmDeleteOpen, setConfirmDeleteOpen] = useState(false);
  // Soft outcome note after a mass op ("Đã xoá 12/13 · 1 lỗi…") — non-blocking, dismissible,
  // never blanks the list.
  const [massMsg, setMassMsg] = useState("");
  // Inside the Studio frame the mass ops stay in the page (MassEditBand + InlineConfirm, specs/078 FR-054) and every
  // chunk passes the scope gate (FR-131); the customer face keeps the modal and the prompt untouched.
  const embedded = useEmbedded();
  const scope = useScopeGate(embedded);
  // The bulk bar exists only while rows are selected. "Bỏ chọn" removes itself, and a mass op's
  // refresh clears the selection under the button that opened its dialog; focus would fall to
  // <body>. It goes to the grid's select-all checkbox instead, where the selection started (and is
  // what the mass-op dialogs return to once their opener is gone).
  const gridBoxRef = useRef<HTMLDivElement>(null);
  const selectAllBox = useCallback(() => gridBoxRef.current?.querySelector<HTMLInputElement>("thead input[type='checkbox']") ?? null, []);
  const hadSelection = useRef(false);
  useEffect(() => {
    const has = selected.size > 0;
    if (hadSelection.current && !has && (!document.activeElement || document.activeElement === document.body)) selectAllBox()?.focus();
    hadSelection.current = has;
  }, [selected.size, selectAllBox]);

  // Visible columns (hidden set) + user-resized widths (persisted per user + entity, lib/dataGrid).
  const [hiddenCols, setHiddenCols] = useState<Set<string>>(new Set());
  // Read once per entity (reset block below). Safe against hydration: no header cell renders
  // before the layout loads client-side, so the server's {} is never compared with the stored widths.
  const [colSizing, setColSizing] = useState<ColumnSizingState>(() => loadColWidths(`list:${entity}`));

  const fieldsByName = useMemo(
    () => Object.fromEntries(fields.map((f) => [f.name, f])),
    [fields],
  );

  // Real entity label (from /metadata/entities) for the header — NOT the machine name.
  const { data: allEntities = [] } = useEntities();
  const entityLabel = useMemo(
    () => allEntities.find((e) => e.name === entity)?.label ?? entity,
    [allEntities, entity],
  );

  const allColumns: LayoutListColumn[] = useMemo(
    () => layout?.views.list?.columns ?? [],
    [layout],
  );
  const columns = useMemo(
    () => allColumns.filter((c) => !hiddenCols.has(c.field)),
    [allColumns, hiddenCols],
  );
  const expandFields = useMemo(() => relationColumns(columns, fields), [columns, fields]);

  // Debounce search input 350ms.
  useEffect(() => {
    const id = setTimeout(() => setSearchQ(searchRaw.trim()), 350);
    return () => clearTimeout(id);
  }, [searchRaw]);

  // Debounce filter changes 300ms → one fetch with the full value, not one per keystroke (R2-02).
  useEffect(() => {
    const id = setTimeout(() => setDebouncedFilters(filters), 300);
    return () => clearTimeout(id);
  }, [filters]);

  // Column widths are saved 300ms after a resize (per user + entity, lib/dataGrid).
  const saveTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const resizeColumns = (next: ColumnSizingState) => {
    setColSizing(next);
    if (saveTimer.current) clearTimeout(saveTimer.current);
    saveTimer.current = setTimeout(() => saveColWidths(`list:${entity}`, next), 300);
  };

  // Reset when entity changes.
  const [prevEntity, setPrevEntity] = useState(entity);
  if (prevEntity !== entity) {
    setPrevEntity(entity);
    setLoadState("loading");
    setErrMsg("");
    setLayout(null);
    setFields([]);
    setRecords([]);
    setNext(null);
    setTotal(0);
    setLoadingMore(false);
    setSort(null);
    setSearchRaw("");
    setSearchQ("");
    setFilters([]);
    setSelected(new Set());
    setHiddenCols(new Set());
    setColSizing(loadColWidths(`list:${entity}`));
    setAclHiddenCount(0);
    setMassEditOpen(false);
    setMassEditIds([]);
    setConfirmDeleteOpen(false);
    setMassMsg("");
    setViewStatus({ name: null, dirty: false });
  }

  // FIRST page of a (new) chain for this sort/search/filter set. The mode is decided here, per
  // chain: a changed sort, search or filter always lands here, so a cursor is never sent with a
  // sort it was not minted for (400 INVALID_CURSOR).
  const fetchFirstPage = useCallback(
    async (s: SortState | null, expand: string[], q: string, fltrs: FilterRow[], byName: Record<string, FieldMeta>) => {
      const mode = choosePaginationMode(s, byName);
      const query = buildListPageQuery({
        mode, sort: s, expand, q, filterQuery: filtersToQuery(fltrs), limit: PAGE_LIMIT, next: null,
      });
      return { mode, page: await fetchListPage(entity, mode, query) };
    },
    [entity],
  );

  const applyFirstPage = (mode: PageMode, page: ListPageResult) => {
    setPageMode(mode);
    setRecords(page.records);
    setNext(page.next);
    setTotal(page.total);
    setAclHiddenCount(page.hiddenColumns);
  };

  // Initial load.
  useEffect(() => {
    let cancelled = false;
    const myReq = ++reqIdRef.current;

    (async () => {
      try {
        await ensureAuth();
        if (cancelled || myReq !== reqIdRef.current) return;

        // F7 list-amnesia — read incoming URL state ONCE, before the first fetch (client effect →
        // SSR-safe). Precedence: explicit URL params (deep link / drill-through) WIN; otherwise the
        // per-entity persisted prefs restore viewMode/density (localStorage `list:<entity>:view`).
        // Applied after the auth await, still while loadState is "loading": state set in the effect
        // body itself would cascade a render (react-hooks/set-state-in-effect).
        const urlState = parseListSearch(window.location.search);
        // Ghi nhớ URL vào có tự nói sort/order hay không, để lần ghi URL lại không xoá mất ý định
        // đó chỉ vì nó trùng defaultSort của layout (xem keepSort trong lib/listState).
        setUrlSortExplicit(!!urlState.sort);
        const prefs = loadListPrefs(entity);
        setShowOwner(prefs.showOwner ?? false);
        const modes = resolveInitialModes(urlState, prefs);
        setUrlHadExplicitState(urlStateIsExplicit(urlState));
        setViewMode(modes.viewMode);
        setDensity(modes.density);
        if (urlState.q) {
          // Both raw + debounced together so neither the debounce nor the refetch effect re-fires.
          setSearchRaw(urlState.q);
          setSearchQ(urlState.q);
        }

        const [lay, flds] = await Promise.all([getLayout(entity), listFields(entity)]);
        if (cancelled || myReq !== reqIdRef.current) return;
        if (!lay) throw new ApiError(404, "LAYOUT_NOT_FOUND", t("page.list.layoutMissing"));

        const def: LayoutDefaultSort | undefined = lay.views.list?.defaultSort;
        // VIEC-HOM-NAY: a deep-link may carry ?sort=&order= (work-queue href) — URL wins over the
        // layout's defaultSort. Both keys are in RESERVED_QUERY_KEYS so the filter parser skips
        // them; they are CONSUMED here instead.
        const initialSort: SortState | null =
          urlState.sort ?? (def ? { field: def.field, order: def.order } : null);
        const expand = relationColumns(lay.views.list?.columns, flds);

        // D13: honour incoming ?field[op]=value from the URL (drill-through / deep-link) so the
        // list arrives already filtered.
        const urlFilters = urlState.filters;

        const byName = Object.fromEntries(flds.map((f) => [f.name, f]));
        const { mode, page } = await fetchFirstPage(initialSort, expand, urlState.q, urlFilters, byName);
        if (cancelled || myReq !== reqIdRef.current) return;

        setLayout(lay);
        setFields(flds);
        setSort(initialSort);
        setFilters(urlFilters);
        setDebouncedFilters(urlFilters); // keep in sync so the debounce effect doesn't refetch
        if (urlFilters.length > 0) setShowFilterPanel(true);
        applyFirstPage(mode, page);
        setLoadState("ok");
      } catch (e) {
        if (cancelled || myReq !== reqIdRef.current) return;
        setErrMsg(e instanceof ApiError ? e.message : e instanceof Error ? e.message : t("page.list.connError"));
        setLoadState("error");
      }
    })();
    return () => { cancelled = true; };
  }, [entity, fetchFirstPage]);

  // Re-fetch on search/filter change (new chain from the first page).
  useEffect(() => {
    if (loadState !== "ok") return; // don't re-trigger during initial load
    const myReq = ++reqIdRef.current;
    (async () => {
      try {
        await ensureAuth();
        if (myReq !== reqIdRef.current) return;
        setLoadState("loading"); // after the await: no synchronous state cascade from the effect
        const { mode, page } = await fetchFirstPage(sort, expandFields, searchQ, debouncedFilters, fieldsByName);
        if (myReq !== reqIdRef.current) return;
        applyFirstPage(mode, page);
        setLoadState("ok");
        setSelected(new Set());
      } catch (e) {
        if (myReq !== reqIdRef.current) return;
        setErrMsg(e instanceof ApiError ? e.message : t("page.list.searchError"));
        setLoadState("error");
      }
    })();
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [searchQ, debouncedFilters]);

  // ── F7 list-amnesia: write live state back into the URL + persist prefs ──────────────────────
  const layoutDefaultSort = layout?.views.list?.defaultSort ?? null;

  // Mirror filters/q/sort/view into the URL with SHALLOW history.replaceState — no Next
  // navigation, history.state preserved — so back/refresh/share return to this exact list.
  // Debounced ~300ms and gated on "ok" so the incoming URL is consumed before we ever overwrite
  // it. The pathname guard keeps a late timer from stamping ANOTHER route's URL after a SPA hop.
  useEffect(() => {
    if (loadState !== "ok") return;
    const path = window.location.pathname;
    const id = setTimeout(() => {
      if (window.location.pathname !== path) return;
      const search = buildListSearch({
        current: window.location.search,
        filters: debouncedFilters,
        q: searchQ,
        sort,
        defaultSort: layoutDefaultSort,
        viewMode,
        keepSort: urlSortExplicit,
      });
      const nextUrl = search ? `${path}?${search}` : path;
      if (nextUrl !== window.location.pathname + window.location.search) {
        window.history.replaceState(window.history.state, "", nextUrl);
      }
    }, 300);
    return () => clearTimeout(id);
  }, [loadState, debouncedFilters, searchQ, sort, viewMode, layoutDefaultSort, urlSortExplicit]);

  // Persist viewMode + density per entity (localStorage `list:<entity>:view`). Gated on "ok" so
  // the mount restore lands BEFORE the first write (never clobber saved prefs with defaults).
  useEffect(() => {
    if (loadState !== "ok") return;
    saveListPrefs(entity, { viewMode, density, showOwner });
  }, [entity, viewMode, density, showOwner, loadState]);

  // Server sort on a header click (DataTable reports the next sort; we fetch it).
  const applySort = (nextSort: SortState) => {
    const col = columns.find((c) => c.field === nextSort.field);
    if (!col?.sortable) return;
    const prev = sort; // revert target if the server rejects this sort
    const myReq = ++reqIdRef.current;
    setSort(nextSort);
    setLoadState("loading");
    (async () => {
      try {
        await ensureAuth();
        const { mode, page } = await fetchFirstPage(nextSort, expandFields, searchQ, debouncedFilters, fieldsByName);
        if (myReq !== reqIdRef.current) return;
        applyFirstPage(mode, page);
        setSortErr("");
        setLoadState("ok");
      } catch (e) {
        if (myReq !== reqIdRef.current) return;
        // D29: a non-sortable column must NOT blank the list. Revert the sort, keep the current
        // rows, and surface a soft dismissible note instead of the full-screen error state.
        setSort(prev);
        setSortErr(e instanceof ApiError ? e.message : t("page.list.sortFailed"));
        setLoadState("ok");
      }
    })();
  };

  // "Tải thêm": the next page of the SAME chain (same mode, sort, search, filters).
  const loadMore = () => {
    if (loadingMore || next === null) return;
    setLoadingMore(true);
    (async () => {
      try {
        await ensureAuth();
        const query = buildListPageQuery({
          mode: pageMode, sort, expand: expandFields, q: searchQ,
          filterQuery: filtersToQuery(debouncedFilters), limit: PAGE_LIMIT, next,
        });
        const res = await fetchListPage(entity, pageMode, query);
        setRecords((prev) => {
          const seen = new Set(prev.map((r) => r.id));
          return [...prev, ...res.records.filter((r) => !seen.has(r.id))];
        });
        setNext(res.next);
        if (res.total !== null) setTotal(res.total);
        setAclHiddenCount(res.hiddenColumns);
      } catch (e) {
        setErrMsg(e instanceof ApiError ? e.message : t("page.list.loadMoreError"));
        toast.error(e instanceof ApiError ? e.message : t("page.list.loadMoreError"));
      } finally {
        setLoadingMore(false);
      }
    })();
  };

  // Refetch the first page with the current sort/search/filters and clear the selection —
  // the post-mass-op resync (server-computed fields may have changed; deleted rows must drop).
  const refreshList = useCallback(() => {
    const myReq = ++reqIdRef.current;
    setLoadState("loading");
    (async () => {
      try {
        await ensureAuth();
        const { mode, page } = await fetchFirstPage(sort, expandFields, searchQ, debouncedFilters, fieldsByName);
        if (myReq !== reqIdRef.current) return;
        applyFirstPage(mode, page);
        setSelected(new Set());
        setLoadState("ok");
      } catch (e) {
        if (myReq !== reqIdRef.current) return;
        setErrMsg(e instanceof ApiError ? e.message : t("page.list.reloadError"));
        setLoadState("error");
      }
    })();
  }, [fetchFirstPage, sort, expandFields, searchQ, debouncedFilters, fieldsByName]);

  // The sentence of the first failed row of a mass-op envelope — a before-save/delete rule's own
  // message, a mapped code, or the server's text; never the error code itself (specs/048 US4).
  const firstFailMsg = (res: MassOpResult) => formatSaveError(res.results.find((r) => !r.ok));

  // Mass delete (ENG-MASSOP) — ONE POST /:entity/bulk-delete. Partial semantics server-side:
  // per-row failures come back in the envelope, siblings still land. Soft delete is reversible
  // from the recycle bin, so a plain prompt is the confirmation.
  const bulkDelete = async () => {
    if (selected.size === 0 || bulkDeleting) return;
    const ids = [...selected];
    setConfirmDeleteOpen(false);
    setBulkDeleting(true);
    setMassMsg("");
    try {
      await ensureAuth();
      const res = await (embedded ? bulkDeleteRecords(entity, ids, scope) : bulkDeleteRecords(entity, ids));
      setMassMsg(
        res.failed > 0
          ? fill("page.list.deletedPartial", { ok: res.succeeded, n: ids.length, failed: res.failed, msg: firstFailMsg(res) })
          : fill("page.list.deleted", { n: res.succeeded }),
      );
      refreshList();
    } catch (e) {
      if (e instanceof MassOpStoppedError) {
        setMassMsg(fill("studio.operate.data.mass.stopped", { ok: e.partial.succeeded, n: e.total }));
        refreshList();
        return;
      }
      setMassMsg(e instanceof ApiError ? fill("page.list.deleteFailed", { msg: e.message }) : t("page.list.deleteConnFailed"));
    } finally {
      setBulkDeleting(false);
    }
  };

  // Mass edit (ENG-MASSOP): the modal calls onApplied once per batch → note + resync. The
  // modal itself stays open showing per-row failures until the user closes it.
  const handleMassApplied = (res: MassOpResult) => {
    setMassMsg(
      res.failed > 0
        ? fill("page.list.updatedPartial", { ok: res.succeeded, n: res.succeeded + res.failed, failed: res.failed, msg: firstFailMsg(res) })
        : fill("page.list.updated", { n: res.succeeded }),
    );
    refreshList();
  };

  // Export CSV — fetch with Authorization: Bearer header, then save as blob. The export endpoint
  // takes the ?field[op]=value filters ONLY: every other key (sort, page, q, paginate…) is parsed
  // as a filter there and answered 422 LIST_UNKNOWN_FILTER — which the old list sent, and which it
  // swallowed, so "Xuất CSV" silently did nothing. Search text is therefore not applied to exports.
  const handleExport = async () => {
    try {
      await exportCsv(entity, filtersToQuery(debouncedFilters));
    } catch (e) {
      toast.error(e instanceof ApiError ? fill("page.list.exportFailedMsg", { msg: e.message }) : t("page.list.exportFailed"));
    }
  };

  const go = (href: string) => (onNavigate ? onNavigate(href) : window.location.assign(href));

  // Metadata columns → DataTable columns. Cell rendering stays with the SAME ListCell/FieldDisplay
  // pipeline (shared with the recycle bin via listColumns) — layout order + declared widths.
  const tableColumns = useMemo(
    () =>
      toDataTableColumns(
        buildListGridColumns(columns, fieldsByName, fields, {
          showOwner,
          link: { href: (r) => `/${entity}/${r.id}`, onOpen },
        }),
      ),
    [columns, fieldsByName, fields, showOwner, entity, onOpen],
  );
  const sorting: SortingState = sort && columns.some((c) => c.field === sort.field) ? [{ id: sort.field, desc: sort.order === "desc" }] : [];
  const rowSelection: RowSelectionState = useMemo(() => Object.fromEntries([...selected].map((id) => [id, true])), [selected]);

  // Saved-view state (filters/sort/density/hidden columns) a view captures / restores.
  const currentViewState: ViewState = useMemo(
    () => ({ filters, sort, density, hiddenColumns: [...hiddenCols] }),
    [filters, sort, density, hiddenCols],
  );

  // Apply a saved view in ONE explicit fetch (mirrors the initial-load seam). Setting loadState to
  // "loading" here makes the reactive [searchQ, debouncedFilters] effect bail (it guards on "ok"),
  // so setting debouncedFilters below never double-fetches. Search text is left untouched (a view
  // captures filters/sort/columns/density, not the transient search box).
  const applyView = useCallback(
    (vs: ViewState) => {
      const myReq = ++reqIdRef.current;
      const hidden = new Set(vs.hiddenColumns);
      setSort(vs.sort);
      setDensity(vs.density);
      setHiddenCols(hidden);
      setFilters(vs.filters);
      setDebouncedFilters(vs.filters); // keep in sync → the debounce effect is a no-op
      setShowFilterPanel((prev) => (vs.filters.length > 0 ? true : prev));
      setSortErr("");
      setLoadState("loading");
      (async () => {
        try {
          await ensureAuth();
          const visibleCols = allColumns.filter((c) => !hidden.has(c.field));
          const expand = relationColumns(visibleCols, fields);
          const { mode, page } = await fetchFirstPage(vs.sort, expand, searchQ, vs.filters, fieldsByName);
          if (myReq !== reqIdRef.current) return;
          applyFirstPage(mode, page);
          setSelected(new Set());
          setLoadState("ok");
        } catch (e) {
          if (myReq !== reqIdRef.current) return;
          setErrMsg(e instanceof ApiError ? e.message : t("page.list.applyViewError"));
          setLoadState("error");
        }
      })();
    },
    [fetchFirstPage, allColumns, fields, fieldsByName, searchQ],
  );

  // ── Header ──────────────────────────────────────────────────────────────────────────────
  const count = total !== null ? fill("page.list.count", { n: total }) : fill(hasMore ? "page.list.countMore" : "page.list.count", { n: records.length });
  const aclNote = aclHiddenChipLabel(aclHiddenCount);
  const meta =
    loadState === "error" ? undefined : loadState === "loading" && records.length === 0 ? t("page.list.loading") : (
      <>
        {count}
        {viewStatus.dirty && ` · ${t("page.list.views.dirty")}`}
        {/* HIDDEN-COL-SIGNAL: honest field-ACL note — only when the server reports
            meta.hiddenColumns > 0 (never for admins; see api-conventions §14). */}
        {aclNote && (
          <>
            {" · "}
            <span title={t("page.list.aclHiddenHint")}>{aclNote}</span>
          </>
        )}
      </>
    );

  const actions = [
    ...(onCreate && mayCreate ? [{ label: t("page.list.new"), onSelect: onCreate }] : []),
    ...(mayCreate
      ? [
          { label: t("page.list.import"), onSelect: () => go(`/${entity}/import`) },
          { label: t("page.list.batch"), onSelect: () => go(`/${entity}/batch`) },
        ]
      : []),
  ];

  const settingsLabel = t("page.list.settings");
  const controls = [
    // List View Controls (blueprint button-icon-more + menus/dropdown): which columns show, and
    // the owner column. The owner column lives HERE rather than in the layout's column list
    // because owner_id is a system column and never a sys_field — LayoutBuilder has nothing to
    // offer for it. Kept per-user + per-entity (lib/listState), so it changes nothing for anyone else.
    <Menu
      key="settings"
      align="right"
      size="small"
      length={10}
      trigger={
        <button type="button" className="slds-button slds-button_icon slds-button_icon-more" title={settingsLabel}>
          <Settings2 aria-hidden="true" className="slds-button__icon nx-lucide" />
          <ChevronDown aria-hidden="true" className="slds-button__icon slds-button__icon_x-small nx-lucide" />
          <span className="slds-assistive-text">{settingsLabel}</span>
        </button>
      }
    >
      <MenuGroup label={t("page.list.columns")}>
        {allColumns.map((c) => {
          const hidden = hiddenCols.has(c.field);
          return (
            <MenuCheckboxItem
              key={c.field}
              label={c.labelOverride ?? fieldsByName[c.field]?.label ?? c.field}
              checked={!hidden}
              onCheckedChange={() =>
                setHiddenCols((prev) => {
                  const s = new Set(prev);
                  if (hidden) s.delete(c.field);
                  else s.add(c.field);
                  return s;
                })
              }
            />
          );
        })}
      </MenuGroup>
      <MenuDivider />
      <MenuCheckboxItem label={t("page.list.ownerColumn")} checked={showOwner} onCheckedChange={() => setShowOwner((v) => !v)} />
    </Menu>,
    <ButtonIcon key="export" icon={Download} variant="border-filled" label={t("page.list.export")} onClick={() => void handleExport()} />,
    // Thùng rác — quiet secondary access to soft-deleted records.
    <ButtonIcon key="bin" icon={Trash2} variant="border-filled" label={t("page.list.recycleBin")} onClick={() => go(`/${entity}/deleted`)} />,
  ];

  // ── Body ────────────────────────────────────────────────────────────────────────────────
  const filtered = !!searchQ || filters.length > 0;
  const empty = (
    <EmptyState
      icon={Inbox}
      title={t(filtered ? "page.list.emptyFiltered" : "page.list.empty")}
      description={
        filtered ? t("page.list.emptyFilteredHint") : (
          <>
            {t("page.list.emptyHint")} <Link href="/about">{t("page.list.aboutLink")}</Link>.
          </>
        )
      }
      action={!filtered && onCreate && mayCreate ? <Button variant="brand" onClick={onCreate}>{t("page.list.createFirst")}</Button> : undefined}
    />
  );

  const dismiss = t("page.list.dismiss");
  const notices = (
    <>
      {loadState === "error" && (
        <ScopedNotification theme="error">
          <strong>{t("page.list.loadError")}</strong> {errMsg}
        </ScopedNotification>
      )}
      {/* Soft sort note (D29) — non-blocking: the list stays visible, sort reverts */}
      {sortErr && (
        <div className="slds-grid slds-grid_vertical-align-center">
          <ScopedNotification theme="warning" className="slds-col">{t("page.list.sortError")}</ScopedNotification>
          <ButtonIcon icon={X} label={dismiss} className="slds-m-left_x-small" onClick={() => setSortErr("")} />
        </div>
      )}
      {/* Mass-op outcome — soft, dismissible, never blanks the list */}
      {massMsg && (
        <div className="slds-grid slds-grid_vertical-align-center">
          <ScopedNotification className="slds-col">{massMsg}</ScopedNotification>
          <ButtonIcon icon={X} label={dismiss} className="slds-m-left_x-small" onClick={() => setMassMsg("")} />
        </div>
      )}
    </>
  );
  const hasNotices = loadState === "error" || !!sortErr || !!massMsg;

  return (
    <div className="flex min-h-0 flex-1 flex-col p-4">
      <ObjectHomePage
        icon={{ glyph: Database, name: "record" }}
        label={entityLabel}
        title={viewStatus.name ?? t("page.list.allRecords")}
        titleMenu={
          /* Saved views — gated on `layout` (NOT loadState) so it stays MOUNTED through
             applyView/search/sort refetches; keyed by entity so it remounts on entity switch. */
          layout ? (
            <SavedViews
              key={entity}
              entity={entity}
              current={currentViewState}
              onApply={applyView}
              // Auto-apply the last-applied view ONLY on clean visits — an explicit deep link
              // (filters/q/sort/view in the URL) stays authoritative (F7 precedence rule).
              autoApply={!urlHadExplicitState}
              onStatusChange={setViewStatus}
            />
          ) : undefined
        }
        actions={actions}
        meta={meta}
        search={{ value: searchRaw, onChange: setSearchRaw }}
        density={{ value: density, onChange: setDensity }}
        view={{ value: viewMode, onChange: setViewMode }}
        controls={controls}
        filter={{ open: showFilterPanel, onToggle: () => setShowFilterPanel((v) => !v), panelId: filterPanelId }}
        filterPanel={
          <ListFilterDock
            id={filterPanelId}
            open={showFilterPanel}
            onClose={() => setShowFilterPanel(false)}
            fields={withOwnerFilter(fields)}
            value={filters}
            onChange={setFilters}
          />
        }
      >
        {hasNotices && <div className="slds-p-around_small slds-border_top">{notices}</div>}

        {/* Active-filter pills — reflect the intended filters immediately (fetch lags 300ms). Always
            mounted (renders nothing without an active filter) so removing the last pill can still
            hand focus to the filter toggle. */}
        <ListFilterPills
          fields={withOwnerFilter(fields)}
          value={filters}
          onChange={setFilters}
          panelId={filterPanelId}
          className="slds-p-horizontal_small slds-p-vertical_x-small slds-border_top"
        />

        {/* Bulk selection bar — "N đã chọn · Sửa hàng loạt · Xoá hàng loạt · Xuất CSV · Bỏ chọn" */}
        {viewMode === "table" && selected.size > 0 && (
          <div className="slds-grid slds-grid_vertical-align-center slds-p-around_x-small slds-theme_shade slds-border_top">
            <span className="slds-text-title_bold slds-m-horizontal_small">{fill("page.list.selected", { n: selected.size })}</span>
            <div className="slds-button-group" role="group">
              {mayEdit && (
                <Button iconLeft={PenLine} onClick={() => { setMassEditIds([...selected]); setMassEditOpen(true); }}>
                  {embedded ? fill("studio.operate.data.mass.edit", { n: selected.size }) : t("page.list.massEdit")}
                </Button>
              )}
              {mayDelete && (
                <Button variant="text-destructive" iconLeft={Trash2} onClick={() => setConfirmDeleteOpen(true)} disabled={bulkDeleting}>
                  {embedded ? fill("studio.operate.data.mass.delete", { n: selected.size }) : t("page.list.massDelete")}
                </Button>
              )}
              <Button iconLeft={Download} onClick={() => void handleExport()}>
                {t("page.list.export")}
              </Button>
            </div>
            <Button variant="base" iconLeft={X} className="slds-col_bump-left slds-m-right_small" onClick={() => setSelected(new Set())}>
              {t("page.list.clearSelection")}
            </Button>
          </div>
        )}

        {embedded && massEditOpen && (
          <MassEditBand
            entity={entity}
            fields={fields}
            ids={massEditIds}
            guard={scope}
            onApplied={() => refreshList()}
            onClose={() => setMassEditOpen(false)}
          />
        )}
        {embedded && confirmDeleteOpen && (
          <InlineConfirm
            tone="error"
            message={fill("studio.operate.data.mass.deleteConfirm", { n: selected.size })}
            confirmLabel={fill("studio.operate.data.mass.deleteGo", { n: selected.size })}
            onConfirm={() => void bulkDelete()}
            onCancel={() => setConfirmDeleteOpen(false)}
            busy={bulkDeleting}
          />
        )}

        {viewMode === "kanban" && (
          <KanbanBoard
            entity={entity}
            records={records}
            fields={fields}
            columns={columns}
            loading={loadState === "loading"}
            hasMore={hasMore}
            onOpen={onOpen}
            onCreate={mayCreate ? onCreate : undefined}
            onRecordsChange={setRecords}
            laneFieldName={layout?.views.list?.kanbanField}
            filterQuery={filtersToQuery(debouncedFilters)}
            searchQuery={searchQ}
          />
        )}
        {viewMode === "calendar" && (
          <CalendarView
            entity={entity}
            fields={fields}
            onOpen={onOpen}
            dateFieldName={layout?.views.list?.calendarField}
            onCreateAt={onCreateAt}
          />
        )}
        {viewMode === "table" && loadState !== "error" && (
          <>
            <div ref={gridBoxRef} className={clsx(DENSITY_PADDING[density])}>
              <DataTable<ApiRecord>
                label={fill("page.list.gridLabel", { label: entityLabel })}
                columns={tableColumns}
                data={loadState === "ok" ? records : []}
                loading={loadState === "loading"}
                getRowId={(r) => r.id}
                sortable
                manualSorting
                sorting={sorting}
                onSortingChange={(s) => s[0] && applySort({ field: s[0].id, order: s[0].desc ? "desc" : "asc" })}
                selectable
                rowSelection={rowSelection}
                onRowSelectionChange={(sel) => setSelected(new Set(Object.keys(sel).filter((k) => sel[k])))}
                resizable
                columnSizing={colSizing}
                onColumnSizingChange={resizeColumns}
                maxHeight="max(24rem, calc(100vh - 19rem))"
                empty={empty}
              />
            </div>

            {/* Footer — loaded count + "Tải thêm" (offset page / cursor chain) */}
            {loadState === "ok" && records.length > 0 && (
              <div className="slds-grid slds-grid_vertical-align-center slds-grid_align-spread slds-p-around_small slds-border_top">
                <span className="slds-text-color_weak">
                  {total !== null
                    ? fill("page.list.loaded", { n: records.length, total })
                    : fill("page.list.loadedCursor", { n: records.length })}
                </span>
                {hasMore && (
                  <Button onClick={loadMore} disabled={loadingMore} iconRight={loadingMore ? undefined : ChevronDown}>
                    {loadingMore ? t("page.list.loadingMore") : fill("page.list.loadMore", { n: PAGE_LIMIT })}
                  </Button>
                )}
              </div>
            )}
          </>
        )}
      </ObjectHomePage>

      {/* Mass edit (ENG-MASSOP) — conditional mount so each open starts fresh. */}
      {!embedded && massEditOpen && (
        <MassEditModal
          entity={entity}
          entityLabel={entityLabel}
          fields={fields}
          ids={massEditIds}
          open
          onClose={() => setMassEditOpen(false)}
          onApplied={handleMassApplied}
          returnFocus={selectAllBox}
        />
      )}

      {/* Mass-delete confirm (prompt/base) — soft delete is reversible from the recycle bin. */}
      <Prompt
        open={!embedded && confirmDeleteOpen}
        onOpenChange={setConfirmDeleteOpen}
        returnFocus={selectAllBox}
        title={fill("page.list.confirmDeleteTitle", { n: selected.size })}
        theme="warning"
        footer={
          <>
            <Button onClick={() => setConfirmDeleteOpen(false)} disabled={bulkDeleting}>
              {t("action.cancel")}
            </Button>
            <Button variant="destructive" onClick={() => void bulkDelete()} disabled={bulkDeleting}>
              {t("page.list.massDelete")}
            </Button>
          </>
        }
      >
        <p>{t("page.list.confirmDeleteBody")}</p>
      </Prompt>
    </div>
  );
}
