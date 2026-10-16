"use client";

// W4 Dashboard Viewer v2 — /dashboards/:id
// • Filter bar THẬT (giết W0-hidden no-op): DateRangePicker preset + owner scope → đổ vào
//   ?dateFrom/?dateTo/?ownerId của GET /dashboards/:id/run (overlay AND lên mọi widget —
//   api-conventions §4.4, BE live 2026-07-03). Chip gỡ được + URL-sync (link chia sẻ giữ filter).
// • Grid v2: react-grid-layout v2 (dep-spike OK Next16/React19) — kéo + resize 12 cột.
//   x/y/w/h lưu SERVER-side trên định nghĩa dashboard DÙNG CHUNG (PUT /admin/dashboards/:id,
//   debounced). Bố cục là thuộc tính của dashboard → MỌI người xem thấy cùng cách sắp xếp mà
//   admin đặt. Chỉ admin sửa được (nút "Sửa bố cục" ẩn với người không phải admin — họ xem
//   read-only). Mapper both-or-neither extent: lib/dashWidgetPayload (không bao giờ gửi extent lệch).
// • Bucket time-series (config.bucket): zero-fill bucket rỗng (BE bỏ qua bucket rỗng),
//   nhãn trục theo bucket (dd/MM · Tuần w · MM/yyyy · Qq yyyy · yyyy) — lib/timeBuckets.
// • Funnel: widget bar có groupBy được admin toggle cột ↔ phễu — lưu SERVER-side bằng cách đổi
//   widget `type` (bar ↔ funnel) trên định nghĩa dùng chung (BE validWidgetTypes có "funnel").
// • Per-widget error isolation GIỮ NGUYÊN (run partial-success 200 + error card riêng).
// • DASH-MERGE (2026-07-25): TOÀN BỘ authoring gộp vào đây (admin-only) — đổi tên, thêm/sửa/xoá
//   widget (panel DashboardAuthoringPanel), xoá dashboard. Studio DashboardBuilder đã bị xoá;
//   /studio/dashboards chỉ còn redirect. MỘT artifact — MỘT màn soạn.
// SLDS (specs/038 T029, RESEARCH §5 row /dashboards/[id] — no dashboard/chart blueprint, so it is
// composed): page-headers/record-home (label · name · highlights · button-group actions) over the
// run filter bar (popovers + datepickers/range + combobox + pills), then one `cards` card per widget
// on the react-grid-layout grid; recharts series paint with --chart-* (SLDS palette hooks).
// Rename = modals, delete = prompt, layout hint = scoped-notifications. Back navigation is the
// shell's crumb strip ("lên cấp" + "Bảng điều khiển / Chi tiết").

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useRouter } from "next/navigation";
import { keepPreviousData, useQuery, useQueryClient } from "@tanstack/react-query";
import GridLayout, { useContainerWidth, type Layout } from "react-grid-layout";
import "react-grid-layout/css/styles.css";
import {
  BarChart2, Check, Filter as FunnelIcon, LayoutDashboard, Pencil, PencilRuler, RefreshCw, RotateCcw,
  SlidersHorizontal,
} from "lucide-react";
import {
  ApiError, deleteDashboard, hasDraftChange, runDashboard, updateDashboard, listFields,
  type AggGroupRow, type DashboardWidgetDef, type WidgetRun,
} from "@/lib/api";
import { qk } from "@/lib/query/keys";
import { useAuth } from "@/lib/auth/AuthProvider";
import { t, tf } from "@/lib/i18n";
import { toast } from "@/components/ui/Toaster";
import { errorMessage } from "@/lib/errorMessages";
import { widgetGroupsToChartData, formatWidgetValue, orderChartByCategory, relabelChartData } from "@/lib/analytics";
import { filtersToQueryKey, type RunFilters } from "@/lib/runFilters";
import { isBucketId, bucketSeriesToChartData } from "@/lib/timeBuckets";
import {
  GRID_COLS, GRID_MARGIN, GRID_ROW_HEIGHT, reconcileLayout,
  type GridRect, type WidgetShape, type WidgetViz,
} from "@/lib/dashLayout";
import {
  buildWidgetLayoutPayload, serverWidgetsToRects, type WidgetLayout,
} from "@/lib/dashWidgetPayload";
import { useWorkshopScope } from "@/lib/workshopScope";
import { useRunFilters } from "@/components/analytics/useRunFilters";
import { RunFilterBar } from "@/components/analytics/RunFilterBar";
import { DashboardAuthoringPanel } from "@/components/analytics/DashboardAuthoring";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { ButtonGroup } from "@/components/slds/ButtonGroup";
import { Card } from "@/components/slds/Card";
import { Input } from "@/components/slds/Input";
import { Modal, Prompt } from "@/components/slds/Modal";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { DraftViewButton, DraftViewNotice, useDraftView } from "@/components/analytics/useDraftView";
import { emitDraftsChanged } from "@/lib/draftEvents";
import { PageHeader } from "@/components/slds/PageHeader";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { NxBar } from "@/components/charts/NxBar";
import { NxLine } from "@/components/charts/NxLine";
import { NxPie } from "@/components/charts/NxPie";
import { NxKPI } from "@/components/charts/NxKPI";
import { NxFunnel } from "@/components/charts/NxFunnel";

// Card chrome around the body content of a widget cell, in px (measured in the browser): borders ·
// header padding + a title row · header/body gap · sub line · body bottom margin. A header button
// (the admin's bar↔funnel toggle) makes the header row 12px taller. The chart / table gets the rest.
const CARD_CHROME = 78;
const HEADER_BUTTON = 12;

/** The bar↔funnel viz of each bar-family widget, from its persisted server `type`. */
function vizSeed(widgets: DashboardWidgetDef[]): Record<string, WidgetViz> {
  const seed: Record<string, WidgetViz> = {};
  for (const w of widgets) {
    if (w.type === "bar" || w.type === "funnel") seed[w.id] = w.type === "funnel" ? "funnel" : "bar";
  }
  return seed;
}

// Build a drill-through URL: navigate to /:entity list filtered by the groupBy field.
function drillUrl(entity: string, groupByField: string | undefined, key: string): string {
  if (!groupByField || !entity) return `/${entity}`;
  return `/${entity}?${encodeURIComponent(groupByField)}%5Beq%5D=${encodeURIComponent(key)}`;
}

function PageSpinner() {
  return (
    <div className="relative min-h-40 flex-1">
      <Spinner />
    </div>
  );
}

/** Mounted by /dashboards/[id] and, inside an app workshop, by ?panel=dashboard&dashboard=<id> (#302). */
export function DashboardViewer({ id, embedded = false, onDeleted }: { id: string; embedded?: boolean; onDeleted?: () => void }) {
  const router = useRouter();
  const workshop = useWorkshopScope();
  const [filters, setFilters] = useRunFilters();
  // specs/063 S7-2: the builder can open this screen on the unpublished draft (?draft=1); the flag is in
  // the query key so the live and the draft run never share a cache entry.
  const draftView = useDraftView();
  const draftKey: Record<string, string> = draftView.on ? { draft: "1" } : {};

  const { data, isLoading, isError, error, refetch, isFetching, dataUpdatedAt } = useQuery({
    queryKey: qk.dashboardRun(id, { ...filtersToQueryKey(filters), ...draftKey }),
    queryFn: () => runDashboard(id, filters),
    staleTime: 60_000,
    retry: 1,
    // Đổi filter → giữ grid cũ mờ đi thay vì nháy skeleton toàn trang.
    placeholderData: keepPreviousData,
  });

  const defs = useMemo(
    () => new Map<string, DashboardWidgetDef>((data?.dashboard.widgets ?? []).map((w) => [w.id, w])),
    [data],
  );
  const runs = useMemo(
    () => new Map<string, WidgetRun>((data?.widgets ?? []).map((w) => [w.widgetId, w])),
    [data],
  );

  // ── Grid layout: persisted SERVER-side on the shared dashboard definition ──────────────────
  // Layout (x/y/w/h) + the funnel widget type live on sys_dashboards.widgets (admin-owned). Only
  // an admin can write (PUT /admin/dashboards/:id is admin-gated); non-admins get a read-only view
  // of the admin-set arrangement. The server is the single source of truth (no per-browser store).
  const { user } = useAuth();
  const isAdmin = Boolean(user?.isAdmin);

  const [rects, setRects] = useState<GridRect[]>([]);
  const [editMode, setEditMode] = useState(false);
  const [viz, setViz] = useState<Record<string, WidgetViz>>({});
  const [saveState, setSaveState] = useState<"idle" | "saving" | "saved" | "error">("idle");
  const widgetIdsKey = (data?.dashboard.widgets ?? []).map((w) => w.id).join(",");

  // DASH-MERGE: admin authoring state — panel toggle, rename-inline, delete confirm.
  const queryClient = useQueryClient();
  const [authOpen, setAuthOpen] = useState(embedded);
  const [authSaving, setAuthSaving] = useState(false);
  const [renaming, setRenaming] = useState(false);
  const [nameDraft, setNameDraft] = useState("");
  const [descDraft, setDescDraft] = useState("");
  const [delOpen, setDelOpen] = useState(false);
  const [delLoading, setDelLoading] = useState(false);

  // Refs read by the debounced saver so the timer callback always sees the LATEST values (drag
  // fires many onLayoutChange; only the final state should be persisted).
  const dashRef = useRef<{ name: string; description: string; widgets: DashboardWidgetDef[] } | null>(null);
  const rectsRef = useRef<GridRect[]>([]);
  const vizRef = useRef<Record<string, WidgetViz>>({});
  const persistTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const mountedRef = useRef(true);

  // A save answered with draftState went to the DRAFT (delivered workspace): say so, tell the draft bar,
  // and move this screen onto the draft so the grid shows what was just saved. Held in a ref so the
  // savers below keep a stable identity (their unmount-flush effect must not re-run when the router does).
  const afterDraftSave = useRef<() => void>(() => {});
  useEffect(() => {
    afterDraftSave.current = () => {
      toast.success(t("draft.saved"));
      emitDraftsChanged("dashboard");
      // Skip the navigation when the screen is already gone (flush on unmount) — it would pull the user back.
      if (mountedRef.current && !draftView.on) draftView.set(true);
    };
  });

  // Snapshot the full dashboard definition (name + ALL widget objects) for the full-replace PUT.
  useEffect(() => {
    if (data) {
      dashRef.current = {
        name: data.dashboard.name,
        description: data.dashboard.description ?? "",
        widgets: data.dashboard.widgets,
      };
    }
  }, [data]);

  // Load layout + viz from the SERVER widget array whenever the widget SET changes — not on every
  // run refresh. A render-time adjustment (React "storing information from previous renders"), not
  // an effect: setState in an effect paints the stale grid first. serverWidgetsToRects omits
  // unset/degenerate widgets → reconcileLayout assigns them defaults (legacy-dashboard migration).
  const layoutKey = data ? `${id}|${widgetIdsKey}` : null;
  const [seededKey, setSeededKey] = useState<string | null>(null);
  if (data && layoutKey !== seededKey) {
    setSeededKey(layoutKey);
    const widgets = data.dashboard.widgets;
    const shapes: WidgetShape[] = widgets.map((w) => ({
      id: w.id,
      type: w.type,
      grouped: Boolean(w.groupBy),
    }));
    setRects(reconcileLayout(serverWidgetsToRects(widgets), shapes));
    setViz(vizSeed(widgets));
  }
  // The debounced saver reads refs (a timer sees the LATEST values); keep them in step with state.
  useEffect(() => {
    rectsRef.current = rects;
  }, [rects]);
  useEffect(() => {
    vizRef.current = viz;
  }, [viz]);

  // Build the full-replace widget array: overlay current rects + funnel toggles onto the stored
  // widgets, preserving every field we don't model. Type override is restricted to bar-family ids.
  const buildWidgets = useCallback((): DashboardWidgetDef[] | null => {
    const dash = dashRef.current;
    if (!dash) return null;
    const rectById: Record<string, WidgetLayout> = {};
    for (const r of rectsRef.current) rectById[r.i] = { x: r.x, y: r.y, w: r.w, h: r.h };
    const typeById: Record<string, string> = {};
    for (const w of dash.widgets) {
      if (w.type === "bar" || w.type === "funnel") {
        const v = vizRef.current[w.id];
        if (v) typeById[w.id] = v === "funnel" ? "funnel" : "bar";
      }
    }
    return buildWidgetLayoutPayload(dash.widgets, rectById, typeById);
  }, []);

  const flushPersist = useCallback(async () => {
    if (persistTimer.current) {
      clearTimeout(persistTimer.current);
      persistTimer.current = null;
    }
    const dash = dashRef.current;
    const widgets = buildWidgets();
    if (!dash || !widgets) return;
    try {
      const saved = await updateDashboard(id, dash.name, widgets, dash.description || undefined);
      if (mountedRef.current) setSaveState("saved");
      if (hasDraftChange(saved?.draftState)) afterDraftSave.current();
    } catch (e) {
      if (mountedRef.current) {
        setSaveState("error");
        toast.error(errorMessage(e));
      }
    }
  }, [id, buildWidgets]);

  // Debounced save — coalesces a burst of drag/resize events into one PUT ~600ms after the last one.
  const schedulePersist = useCallback(() => {
    if (!isAdmin) return; // non-admins cannot write; the edit UI is hidden for them anyway
    setSaveState("saving");
    if (persistTimer.current) clearTimeout(persistTimer.current);
    persistTimer.current = setTimeout(() => { void flushPersist(); }, 600);
  }, [isAdmin, flushPersist]);

  // Flush a pending save on unmount so a drag-then-navigate never drops the last edit.
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      if (persistTimer.current) {
        clearTimeout(persistTimer.current);
        persistTimer.current = null;
        void flushPersist();
      }
    };
  }, [flushPersist]);

  const onLayoutChange = (layout: Layout) => {
    if (!editMode) return;
    const next = layout.map((l) => ({ i: l.i, x: l.x, y: l.y, w: l.w, h: l.h }));
    const merged = next.map((r) => ({ ...rectsRef.current.find((p) => p.i === r.i), ...r }));
    setRects(merged);
    rectsRef.current = merged;
    schedulePersist();
  };

  const resetLayout = () => {
    if (!data) return;
    const shapes: WidgetShape[] = data.dashboard.widgets.map((w) => ({
      id: w.id, type: w.type, grouped: Boolean(w.groupBy),
    }));
    const def = reconcileLayout([], shapes);
    setRects(def);
    rectsRef.current = def;
    schedulePersist();
  };

  const toggleViz = (widgetId: string) => {
    setViz((prev) => {
      const next: Record<string, WidgetViz> = {
        ...prev,
        [widgetId]: prev[widgetId] === "funnel" ? "bar" : "funnel",
      };
      vizRef.current = next;
      return next;
    });
    schedulePersist();
  };

  // Leaving edit mode ("Xong") flushes any pending debounced write immediately. Flush OUTSIDE the
  // state updater (updaters are double-invoked under React StrictMode → would double-PUT in dev).
  const toggleEditMode = () => {
    if (editMode) void flushPersist();
    setEditMode((e) => !e);
  };

  // DASH-MERGE: one write path for every authoring edit (rename / widget add/edit/remove).
  // Cancels any pending debounced layout PUT first — the layout-aware base (buildWidgets)
  // already carries those rects, so the authoring PUT supersedes it instead of racing it.
  const persistDashboard = useCallback(async (
    patch: { name?: string; description?: string; widgets?: DashboardWidgetDef[] },
  ): Promise<boolean> => {
    if (persistTimer.current) {
      clearTimeout(persistTimer.current);
      persistTimer.current = null;
    }
    const dash = dashRef.current;
    if (!dash) return false;
    const name = patch.name ?? dash.name;
    const description = patch.description ?? dash.description;
    const widgets = patch.widgets ?? buildWidgets() ?? dash.widgets;
    setAuthSaving(true);
    try {
      const saved = await updateDashboard(id, name, widgets, description || undefined);
      dashRef.current = { name, description, widgets };
      if (hasDraftChange(saved?.draftState)) afterDraftSave.current();
      // Re-seed the funnel toggle map from the SAVED types so a bar↔funnel type edit in the
      // authoring form is not clobbered back by a stale toggle on the next layout persist.
      const seed = vizSeed(widgets);
      setViz(seed);
      vizRef.current = seed;
      // Refetch the run (new/changed widgets) + the catalog (name, widget count).
      await queryClient.invalidateQueries({ queryKey: ["dashboard", id] });
      queryClient.invalidateQueries({ queryKey: qk.dashboards() });
      return true;
    } catch (e) {
      toast.error(errorMessage(e));
      return false;
    } finally {
      setAuthSaving(false);
    }
  }, [id, buildWidgets, queryClient]);

  const handleDeleteDashboard = async () => {
    setDelLoading(true);
    try {
      await deleteDashboard(id);
      queryClient.invalidateQueries({ queryKey: qk.dashboards() });
      toast.success(t("page.dashboards.viewer.deleted"));
      if (embedded) onDeleted?.();
      else router.push(workshop ? workshop.panelHref("dashboard") : "/dashboards");
    } catch (e) {
      toast.error(errorMessage(e));
      setDelLoading(false);
      setDelOpen(false);
    }
  };

  const { width, containerRef, mounted } = useContainerWidth();

  const openRename = () => {
    if (!data) return;
    setNameDraft(data.dashboard.name);
    setDescDraft(data.dashboard.description ?? "");
    setRenaming(true);
  };

  const saveRename = async () => {
    if (!nameDraft.trim() || authSaving) return;
    if (await persistDashboard({ name: nameDraft.trim(), description: descDraft.trim() })) setRenaming(false);
  };

  // Edit controls are admin-only: PUT /admin/dashboards/:id is admin-gated, and the layout is a
  // property of the SHARED dashboard. Non-admins get a read-only view.
  const actions = data
    ? [
        isAdmin && editMode && saveState !== "idle" ? <SaveIndicator key="save" state={saveState} /> : null,
        <ButtonGroup key="actions" variant="list" label={t("page.dashboards.viewer.actions")} className="flex-wrap [&_button]:whitespace-nowrap">
          <Button iconLeft={RefreshCw} onClick={() => refetch()} disabled={isFetching}>
            {t("action.refresh")}
          </Button>
          <DraftViewButton view={draftView} />
          {isAdmin && editMode && (
            <Button iconLeft={RotateCcw} onClick={resetLayout} title={t("page.dashboards.viewer.resetHint")}>
              {t("page.dashboards.viewer.reset")}
            </Button>
          )}
          {isAdmin && (
            <Button
              variant={editMode ? "brand" : "neutral"}
              iconLeft={editMode ? Check : PencilRuler}
              onClick={toggleEditMode}
              title={t("page.dashboards.viewer.editLayoutHint")}
            >
              {t(editMode ? "page.dashboards.viewer.done" : "page.dashboards.viewer.editLayout")}
            </Button>
          )}
          {/* DASH-MERGE: full authoring (widget CRUD / delete) — admin-only panel toggle. */}
          {isAdmin && (
            <Button
              variant={authOpen ? "brand" : "neutral"}
              iconLeft={SlidersHorizontal}
              aria-pressed={authOpen}
              onClick={() => setAuthOpen((o) => !o)}
              title={t("page.dashboards.viewer.manageHint")}
            >
              {t("page.dashboards.viewer.manage")}
            </Button>
          )}
          {isAdmin && (
            <Button iconLeft={Pencil} onClick={openRename} title={t("page.dashboards.viewer.renameHint")}>
              {t("page.dashboards.viewer.rename")}
            </Button>
          )}
        </ButtonGroup>,
      ].filter(Boolean)
    : undefined;

  const renameFooter = (
<>
            <Button onClick={() => setRenaming(false)} disabled={authSaving}>
              {t("action.cancel")}
            </Button>
            <Button type="submit" form="nx-rename-dashboard" variant="brand" disabled={authSaving || !nameDraft.trim()}>
              {t("page.dashboards.viewer.saveName")}
            </Button>
          </>
  );
  const renameBody = (
        <form
          id="nx-rename-dashboard"
          className="flex flex-col gap-3"
          onSubmit={(e) => {
            e.preventDefault();
            void saveRename();
          }}
        >
          <Input
            label={t("page.dashboards.viewer.nameLabel")}
            required
            value={nameDraft}
            onChange={(e) => setNameDraft(e.target.value)}
          />
          <Input
            label={t("page.dashboards.viewer.descLabel")}
            placeholder={t("page.dashboards.viewer.descPlaceholder")}
            value={descDraft}
            onChange={(e) => setDescDraft(e.target.value)}
          />
        </form>
  );

  const authPanel = data && isAdmin && authOpen ? (
    <DashboardAuthoringPanel
      widgets={data.dashboard.widgets}
      draftState={data.dashboard.draftState}
      saving={authSaving}
      getBaseWidgets={() => buildWidgets() ?? data.dashboard.widgets}
      onSave={(next) => persistDashboard({ widgets: next })}
      onDeleteDashboard={() => setDelOpen(true)}
    />
  ) : null;

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto p-4">
      {/* Loading (first run only — a filter change keeps the previous grid via keepPreviousData) */}
      {isLoading && <PageSpinner />}

      {/* Dashboard-level error (per-widget errors render as cards in-grid) */}
      {isError && !isLoading && (
        <ScopedNotification theme="error">
          {embedded && error instanceof ApiError && error.status === 404
            ? t("studio.build.reports.dashboard.notFound")
            : error instanceof Error ? error.message : t("page.dashboards.viewer.loadError")}
        </ScopedNotification>
      )}

      {data && (
        <>
          <PageHeader
            variant="record-home"
            icon={{ glyph: LayoutDashboard, name: "dashboard" }}
            label={t("page.dashboards.label")}
            title={data.dashboard.name}
            actions={actions}
            details={[
              { label: t("page.dashboards.viewer.description"), value: data.dashboard.description || "—" },
              { label: t("page.dashboards.viewer.widgetCount"), value: String(data.widgets.length) },
              {
                label: t("page.dashboards.viewer.updatedAt"),
                value: dataUpdatedAt > 0
                  ? new Date(dataUpdatedAt).toLocaleTimeString("vi-VN", { hour: "2-digit", minute: "2-digit" })
                  : "—",
              },
            ]}
          />

          <DraftViewNotice view={draftView} state={data.dashboard.draftState} />

          {/* Studio frame: viewer in the middle, authoring a 300px right column (specs/074 D-UI-28) */}
          <div className={embedded ? "flex flex-col lg:flex-row lg:items-start lg:gap-4" : undefined}>
            <div className="min-w-0 flex-1">
          {/* W4: viewer filter bar — wired straight into the run overlay, never a no-op */}
          <div className="mt-3 flex items-center gap-2">
            <RunFilterBar filters={filters} onChange={setFilters} />
            {isFetching && (
              <div className="relative size-8 shrink-0">
                <Spinner size="x-small" />
              </div>
            )}
          </div>

          {!embedded && authPanel && <div className="mt-3">{authPanel}</div>}

          {editMode && (
            <ScopedNotification className="mt-3">{t("page.dashboards.viewer.editHint")}</ScopedNotification>
          )}

          {/* 12-col drag/resize grid */}
          <div ref={containerRef} className="mt-3" aria-busy={isFetching}>
            {mounted && rects.length > 0 && (
              <GridLayout
                width={width}
                layout={rects}
                gridConfig={{
                  cols: GRID_COLS,
                  rowHeight: GRID_ROW_HEIGHT,
                  margin: [GRID_MARGIN, GRID_MARGIN],
                  containerPadding: [0, 0],
                }}
                dragConfig={{ enabled: editMode, cancel: ".nx-no-drag" }}
                resizeConfig={{ enabled: editMode, handles: ["se"] }}
                onLayoutChange={onLayoutChange}
                className={editMode ? "nx-grid-editing" : undefined}
              >
                {rects.map((r) => {
                  const def = defs.get(r.i);
                  const run = runs.get(r.i);
                  const itemPx = r.h * GRID_ROW_HEIGHT + (r.h - 1) * GRID_MARGIN;
                  return (
                    <div key={r.i} className="min-h-0">
                      {def && run ? (
                        <WidgetCell
                          def={def}
                          run={run}
                          heightPx={itemPx}
                          filters={filters}
                          viz={viz[r.i] ?? (def.type === "funnel" ? "funnel" : "bar")}
                          onToggleViz={isAdmin ? () => toggleViz(r.i) : undefined}
                          onDrill={(key) => {
                            if (editMode || !def.entity) return;
                            router.push(drillUrl(def.entity, def.groupBy, key));
                          }}
                        />
                      ) : (
                        <Card title={t("page.dashboards.viewer.missing")} hideHeader className="h-full">
                          <p className="slds-text-color_weak">{t("page.dashboards.viewer.missing")}</p>
                        </Card>
                      )}
                    </div>
                  );
                })}
              </GridLayout>
            )}
          </div>
            </div>
            {embedded && authPanel && (
              <aside className="mt-3 w-full shrink-0 lg:w-[300px]" aria-label={t("page.dashboards.viewer.authoringAside")}>
                {authPanel}
              </aside>
            )}
          </div>
        </>
      )}

      {/* DASH-MERGE: rename + description, saved through the same full-replace PUT (widgets ride
          through layout-aware, untouched). Inside the Studio frame it is a form in the page, not a dialog. */}
      {embedded ? (
        renaming && isAdmin && (
          <section aria-label={t("page.dashboards.viewer.renameTitle")} className="slds-border_top slds-m-top_medium slds-p-top_small">
            <p className="slds-text-heading_small slds-m-bottom_small">{t("page.dashboards.viewer.renameTitle")}</p>
            {renameBody}
            <div className="slds-grid slds-grid_align-end flex-wrap gap-2 slds-m-top_small">{renameFooter}</div>
          </section>
        )
      ) : (
        <Modal
          open={renaming && isAdmin}
          onOpenChange={(o) => !authSaving && setRenaming(o)}
          title={t("page.dashboards.viewer.renameTitle")}
          size="small"
          footer={renameFooter}
        >
          {renameBody}
        </Modal>
      )}

      {/* DASH-MERGE: delete-dashboard safety net — the artifact is SHARED, every viewer loses it. */}
      {embedded ? (
        delOpen && (
          <div className="slds-m-top_medium">
            <InlineConfirm
              tone="error"
              message={tf("page.dashboards.viewer.deleteBody", { name: data?.dashboard.name ?? "" })}
              confirmLabel={t("page.dashboards.viewer.deleteConfirm")}
              onConfirm={() => void handleDeleteDashboard()}
              onCancel={() => setDelOpen(false)}
              busy={delLoading}
            />
          </div>
        )
      ) : (
      <Prompt
        open={delOpen}
        onOpenChange={(o) => !delLoading && setDelOpen(o)}
        theme="error"
        title={t("page.dashboards.viewer.deleteTitle")}
        footer={
          <>
            <Button onClick={() => setDelOpen(false)} disabled={delLoading}>
              {t("action.cancel")}
            </Button>
            <Button variant="destructive" onClick={handleDeleteDashboard} disabled={delLoading}>
              {t("page.dashboards.viewer.deleteConfirm")}
            </Button>
          </>
        }
      >
        <p>{tf("page.dashboards.viewer.deleteBody", { name: data?.dashboard.name ?? "" })}</p>
      </Prompt>
      )}
    </div>
  );
}

// ── Inline auto-save state (admin edit mode) ─────────────────────────────────

function SaveIndicator({ state }: { state: "idle" | "saving" | "saved" | "error" }) {
  if (state === "idle") return null;
  const cfg = {
    saving: { key: "page.dashboards.viewer.saving", cls: "slds-text-color_weak" },
    saved: { key: "page.dashboards.viewer.saved", cls: "slds-text-color_success" },
    error: { key: "page.dashboards.viewer.saveError", cls: "slds-text-color_error" },
  } as const;
  return (
    <span className={`slds-text-body_small ${cfg[state].cls}`} aria-live="polite">
      {t(cfg[state].key)}
    </span>
  );
}

// ── One grid cell — routes a widget run to the right renderer ────────────────

function WidgetCell({
  def, run, heightPx, filters, viz, onToggleViz, onDrill,
}: {
  def: DashboardWidgetDef;
  run: WidgetRun;
  heightPx: number;
  filters: RunFilters;
  viz: WidgetViz;
  onToggleViz?: () => void; // undefined = read-only (non-admin): render the saved viz, no toggle
  onDrill: (key: string) => void;
}) {
  // Field metadata of the widget's entity, fetched for ANY grouped widget (react-query dedupes
  // across widgets sharing an entity). Two consumers:
  //   • DASH-FUNNEL-ORDER: a funnel renders stages in the picklist's DECLARED order.
  //   • REPORT-LABELS (WAVE 6): group axes show options.valueLabels ("Proposal" → "Báo giá") —
  //     the detail screen already speaks Vietnamese and the charts were still speaking codes.
  const isFunnel = viz === "funnel" && !!def.groupBy;
  const { data: entityFields } = useQuery({
    queryKey: qk.fields(def.entity),
    queryFn: () => listFields(def.entity),
    staleTime: 5 * 60_000,
    // A widget the caller may not read (run.code FORBIDDEN) must not probe that entity's fields: the
    // server answers 404 and the console shows a failed request for data the user is not meant to see.
    enabled: !!def.groupBy && !run.error,
  });
  const groupByField = def.groupBy ? entityFields?.find((f) => f.name === def.groupBy) : undefined;
  const funnelOrder = (isFunnel && (groupByField?.options?.values as string[] | undefined)) || [];
  const groupLabels = groupByField?.options?.valueLabels as Record<string, string> | undefined;

  if (run.error) return <WidgetErrorCard w={run} />;
  if (!run.result) return <Card title={run.title} loading className="h-full" />;

  const grouped = run.result.grouped;
  const cc = run.result.currencyCode; // DASH-CURRENCY: field's own currency (empty for number/rating)
  const barFamily = def.type === "bar" || def.type === "funnel";
  const chartH = Math.max(120, heightPx - CARD_CHROME - (onToggleViz && barFamily && !isBucketId(def.bucket) ? HEADER_BUTTON : 0));

  // Scalar KPI
  if (!grouped) {
    return (
      <NxKPI
        className="h-full overflow-hidden"
        label={run.title}
        value={formatWidgetValue(run.result.value, def.metric, cc)}
        sub={tf("page.dashboards.viewer.kpiSub", { count: (run.result.count ?? 0).toLocaleString("vi-VN"), metric: def.metric })}
      />
    );
  }

  const groups = run.result.groups ?? [];
  const bucket = isBucketId(def.bucket) ? def.bucket : null;

  // W4-D: bucketed time series — zero-fill + chronological + bucket axis labels.
  // No drill: a bucket key ("2026-07") is not a field value the list can filter on.
  if (bucket) {
    const pts = bucketSeriesToChartData(groups, bucket, { from: filters.dateFrom, to: filters.dateTo });
    const Chart = barFamily ? NxBar : NxLine;
    return (
      <ChartCard title={run.title} sub={widgetSub(def)} empty={pts.length === 0}>
        <Chart data={pts} height={chartH} formatValue={(n) => formatWidgetValue(n, def.metric, cc)} />
      </ChartCard>
    );
  }

  // Display labels on, drill codes preserved: charts hand `key` to onSelect, so the label swap
  // must come WITH a way back to the code — the drilled list filters ?field[eq]=<code>.
  const { pts, codeOf } = relabelChartData(widgetGroupsToChartData(run), groupLabels);
  const drill = (display: string) => onDrill(codeOf(display));

  // W4-E: funnel ↔ bar for a grouped (stage-like) widget. The chosen viz is the widget's persisted
  // server `type` (bar|funnel); the admin toggle (onToggleViz) flips it. Non-admins see it read-only.
  if (barFamily) {
    return (
      <ChartCard
        title={run.title}
        sub={widgetSub(def)}
        empty={pts.length === 0}
        action={
          onToggleViz ? (
            <Button
              className="nx-no-drag"
              iconLeft={viz === "funnel" ? BarChart2 : FunnelIcon}
              onClick={onToggleViz}
              title={t(viz === "funnel" ? "page.dashboards.viewer.viewBar" : "page.dashboards.viewer.viewFunnel")}
            >
              {t(viz === "funnel" ? "page.dashboards.viewer.bar" : "page.dashboards.viewer.funnel")}
            </Button>
          ) : undefined
        }
      >
        {viz === "funnel" ? (
          <NxFunnel data={orderChartByCategory(pts, funnelOrder.map((v) => groupLabels?.[v] ?? v))} height={chartH} formatValue={(n) => formatWidgetValue(n, def.metric, cc)} onSelect={drill} />
        ) : (
          <NxBar data={pts.slice(0, 12)} height={chartH} formatValue={(n) => formatWidgetValue(n, def.metric, cc)} onSelect={drill} />
        )}
      </ChartCard>
    );
  }

  if (def.type === "line") {
    return (
      <ChartCard title={run.title} sub={widgetSub(def)} empty={pts.length === 0}>
        <NxLine data={pts.slice(0, 20)} height={chartH} formatValue={(n) => formatWidgetValue(n, def.metric, cc)} onSelect={drill} />
      </ChartCard>
    );
  }

  if (def.type === "table") {
    return <TableCard w={run} def={def} bodyPx={heightPx - CARD_CHROME} labels={groupLabels} onDrill={onDrill} />;
  }

  // kpi + grouped = "part of the whole" → a real donut (NxPie, from W1a).
  return (
    <ChartCard title={run.title} sub={widgetSub(def)} empty={pts.length === 0}>
      <NxPie
        data={pts}
        height={chartH}
        formatValue={(n) => formatWidgetValue(n, def.metric, cc)}
        centerLabel={t(def.metric === "count" ? "page.dashboards.viewer.records" : "page.dashboards.viewer.total")}
        onSelect={drill}
      />
    </ChartCard>
  );
}

// ── Widget card for charts (fills the grid cell; header + sub line + empty state) ──

function ChartCard({
  title, sub, empty, action, children,
}: {
  title: string; sub?: string; empty: boolean; action?: React.ReactNode; children: React.ReactNode;
}) {
  return (
    <Card title={title} actions={action} className="h-full overflow-hidden">
      {sub && (
        <p className="slds-text-body_small slds-text-color_weak slds-truncate" title={sub}>
          {sub}
        </p>
      )}
      {empty ? (
        <p className="slds-text-color_weak slds-text-align_center py-6">{t("page.dashboards.viewer.noGroups")}</p>
      ) : (
        children
      )}
    </Card>
  );
}

function widgetSub(def: DashboardWidgetDef): string {
  const base = tf("page.dashboards.viewer.sub", { metric: def.metric, field: def.field ?? "", groupBy: def.groupBy ?? "" });
  return def.bucket ? `${base} · ${def.bucket}` : base;
}

// ── Table widget with drill-through (blueprint data-tables base, inside the card) ──

function TableCard({
  w, def, bodyPx, labels, onDrill,
}: {
  w: WidgetRun; def: DashboardWidgetDef; bodyPx: number; labels?: Record<string, string>; onDrill: (key: string) => void;
}) {
  const groups = w.result?.groups ?? [];
  // Display label per group CODE (REPORT-LABELS) — the row keeps drilling by g.key, the code.
  const show = (k: string) => labels?.[k] ?? k;
  return (
    <Card
      title={w.title}
      padded={false}
      className="h-full overflow-hidden"
      actions={
        <span className="slds-text-body_small slds-text-color_weak">{tf("page.dashboards.viewer.groups", { n: groups.length })}</span>
      }
    >
      <p className="slds-text-body_small slds-text-color_weak slds-truncate px-4" title={widgetSub(def)}>
        {widgetSub(def)}
      </p>
      <div className="nx-no-drag overflow-auto" style={{ maxHeight: Math.max(96, bodyPx) }}>
        <table className="slds-table slds-table_cell-buffer slds-table_bordered" aria-label={w.title}>
          <thead>
            <tr className="slds-line-height_reset">
              <th scope="col">
                <div className="slds-truncate">{t("page.dashboards.viewer.colGroup")}</div>
              </th>
              <th scope="col" className="slds-text-align_right">
                <div className="slds-truncate">{t("page.dashboards.viewer.colValue")}</div>
              </th>
              <th scope="col" className="slds-text-align_right">
                <div className="slds-truncate">{t("page.dashboards.viewer.colCount")}</div>
              </th>
            </tr>
          </thead>
          <tbody>
            {groups.length === 0 ? (
              <tr>
                <td colSpan={3} className="slds-text-align_center slds-text-color_weak">
                  {t("page.dashboards.viewer.noData")}
                </td>
              </tr>
            ) : (
              groups.map((g: AggGroupRow) => (
                <tr key={g.key} className="slds-hint-parent">
                  <th scope="row">
                    <div className="slds-truncate">
                      <button
                        type="button"
                        className="slds-button"
                        title={tf("page.dashboards.viewer.drillHint", { key: show(g.key) })}
                        onClick={() => onDrill(g.key)}
                      >
                        {show(g.key)}
                      </button>
                    </div>
                  </th>
                  <td className="slds-text-align_right tabular-nums">
                    {formatWidgetValue(g.value, def.metric, w.result?.currencyCode)}
                  </td>
                  <td className="slds-text-align_right tabular-nums">{g.count}</td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </Card>
  );
}

// ── Per-widget error card (isolation kept) ───────────────────────────────────

function WidgetErrorCard({ w }: { w: WidgetRun }) {
  // ACL denial: a plain Vietnamese state, never the raw code / server sentence naming the entity.
  if (w.code === "FORBIDDEN") {
    return (
      <Card title={w.title} className="h-full overflow-hidden">
        <ScopedNotification theme="warning">{t("page.dashboards.viewer.widgetForbidden")}</ScopedNotification>
      </Card>
    );
  }
  return (
    <Card title={w.title} actions={w.code ? <Badge>{w.code}</Badge> : undefined} className="h-full overflow-hidden">
      {/* Blueprint scoped-notifications warning theme: glyph, colour and assistive text all say "Cảnh báo". */}
      <ScopedNotification theme="warning">{w.error ?? t("page.dashboards.viewer.widgetError")}</ScopedNotification>
      <p className="slds-text-body_small slds-text-color_weak slds-truncate mt-2" title={t("page.dashboards.viewer.widgetIsolated")}>
        {t("page.dashboards.viewer.widgetIsolated")}
      </p>
    </Card>
  );
}
