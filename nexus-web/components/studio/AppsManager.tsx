"use client";

// AppsManager — /studio/apps (App container Phase 2, ../specs/010-app-container/spec.md §7). Admin CRUD
// for sys_apps + the nav editor. An app is a NAV LENS: deleting it never touches data.
// SLDS (specs/038 T039): the ObjectHomePage template (page-headers/object-home inside a card, the
// page action a neutral button) + data-tables (row actions menu) + modals (delete confirm;
// create/edit = CreateAppDialog). Save of the nav editor does a REPLACE-ALL PUT (array order
// = nav order). Edit app = read-modify-write full PUT. ?new=1 deep-link opens the create dialog
// (project front page).
//
// NavEditor — blueprint dueling-picklist: left "Có sẵn" (entities from metadata + dashboards from the
// catalog, not yet in the menu), right "Menu của app" in display order. Keyboard model is the
// blueprint's own (docs: Space grabs/drops a menu item, arrows move it, Cmd/Ctrl + ←/→ crosses lists)
// with the positions announced in its assertive live region; pointer drag-to-reorder is
// @dnd-kit/sortable. The per-item label override edits the selected menu item below the lists.

import { useEffect, useId, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import Link from "next/link";
import { usePathname, useSearchParams } from "next/navigation";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import clsx from "clsx";
import {
  AppWindow, ArrowLeft, Boxes, ChevronDown, ChevronLeft, ChevronRight, ChevronUp, LayoutDashboard,
  ListTree, Plus, Search,
} from "lucide-react";
import {
  DndContext, PointerSensor, closestCenter, useSensor, useSensors, type DragEndEvent,
} from "@dnd-kit/core";
import { SortableContext, arrayMove, useSortable, verticalListSortingStrategy } from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import { useEntities } from "@/lib/metadata/hooks";
import {
  APP_HOME_BACKEND, ApiError, adminApp, adminApps, createApp, deleteApp, hasDraftChange, listDashboards, replaceAppNav, updateApp,
  type AdminApp, type AdminAppDetail, type AppNavReplaceItem,
} from "@/lib/api";
import { qk } from "@/lib/query/keys";
import { appIconByName, appToneIcon, isValidAppSlug, slugifyAppName } from "@/lib/apps";
import { t } from "@/lib/i18n";
import { StatusText, draftStateLabel } from "@/components/studio/StatusText";
import { useDraftView } from "@/components/analytics/useDraftView";
import { useProposedChanges } from "@/components/studio/assistant/ProposalOverlay";
import type { I18nKey } from "@/lib/i18n/vi";
import { studioHref } from "@/lib/studioRoutes";
import { projectSlugAt } from "@/lib/studioContext";
import { AppHomePanel } from "./AppHomePanel";
import { CreateAppDialog } from "@/components/apps/CreateAppDialog";
import { toast } from "@/components/ui/Toaster";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Card } from "@/components/slds/Card";
import { DataTable } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { Icon } from "@/components/slds/Icon";
import { Input } from "@/components/slds/Input";
import { MenuItem } from "@/components/slds/Menu";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { ObjectHomePage } from "@/components/slds/templates/ObjectHomePage";

/** New-app strip, in the page (specs/074 FR-052): label + code, no dialog. Never touches the shared active-app lens. */
function NewAppForm({ onCreated, onCancel }: { onCreated: (name: string) => void; onCancel: () => void }) {
  const qc = useQueryClient();
  const [label, setLabel] = useState("");
  const [slug, setSlug] = useState("");
  const [touched, setTouched] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const valid = label.trim() !== "" && isValidAppSlug(slug);
  async function submit() {
    if (!valid || busy) return;
    setBusy(true);
    setErr(null);
    try {
      const created = await createApp({ name: slug, label: label.trim() });
      await qc.invalidateQueries({ queryKey: qk.apps() });
      await qc.invalidateQueries({ queryKey: qk.adminApps() });
      toast.success(t("page.apps.dialog.toast.created").replace("{label}", created.label));
      onCreated(created.name);
    } catch (e) {
      setErr(e instanceof ApiError && e.code === "APP_ALREADY_EXISTS" ? t("page.apps.dialog.err.exists") : e instanceof ApiError && e.message ? e.message : t("page.apps.dialog.err.generic"));
    } finally {
      setBusy(false);
    }
  }
  return (
    <form
      className="slds-box slds-theme_shade slds-m-bottom_small"
      onSubmit={(e) => { e.preventDefault(); void submit(); }}
      onKeyDown={(e) => { if (e.key === "Escape" && !busy) onCancel(); }}
    >
      {err && <ScopedNotification theme="error">{err}</ScopedNotification>}
      <Input
        id="new-app-label"
        label={t("page.apps.dialog.label")}
        required
        autoFocus
        value={label}
        onChange={(e) => { setLabel(e.target.value); if (!touched) setSlug(slugifyAppName(e.target.value)); }}
        disabled={busy}
        maxLength={100}
        placeholder={t("page.apps.dialog.labelPlaceholder")}
      />
      <Input
        id="new-app-slug"
        label={t("page.apps.dialog.slug")}
        value={slug}
        onChange={(e) => { setTouched(true); setSlug(e.target.value); }}
        disabled={busy}
        spellCheck={false}
        autoComplete="off"
        error={slug !== "" && !isValidAppSlug(slug) ? t("page.apps.dialog.slugHint") : undefined}
        help={t("page.apps.dialog.slugAuto")}
      />
      <div className="slds-m-top_small slds-grid slds-grid_align-end">
        <Button type="button" onClick={onCancel} disabled={busy}>{t("page.apps.cancel")}</Button>
        <Button type="submit" variant="brand" className="slds-m-left_x-small" disabled={!valid || busy}>{t("page.apps.dialog.submitCreate")}</Button>
      </div>
    </form>
  );
}

export function AppsManager({ app: selectedCode, onSelectApp }: { app?: string | null; onSelectApp?: (name: string | null) => void } = {}) {
  // onSelectApp present = the Studio page owns the menu editor beside this list (AppsPage); absent = old stacked screen.
  const split = !!onSelectApp;
  // Inside a Studio project a new app stays on this page; on the old /studio/apps route it still lands on its overview.
  const pathname = usePathname() ?? "";
  const qc = useQueryClient();
  const appsQ = useQuery({ queryKey: qk.adminApps(), queryFn: adminApps });
  const apps = useMemo(() => appsQ.data ?? [], [appsQ.data]);
  // Delivered workspace: label and menu go through drafts, but creating/deleting an app applies at once
  // (specs/063 S7-4) - say so. useDraftView owns the (cached) publish-state read; canPreview = delivered workspace.
  const delivered = useDraftView().canPreview;

  const [createOpen, setCreateOpen] = useState(false);
  const [editing, setEditing] = useState<AdminApp | null>(null);
  const [deleting, setDeleting] = useState<AdminApp | null>(null);
  const [deleteBusy, setDeleteBusy] = useState(false);
  const [navAppId, setNavAppId] = useState<string | null>(null);

  async function invalidateAll() {
    // ["apps"] prefix covers both the switcher catalog and every appNav; ["admin","apps"]
    // covers the admin list + details.
    await qc.invalidateQueries({ queryKey: qk.apps() });
    await qc.invalidateQueries({ queryKey: qk.adminApps() });
  }

  async function onDelete() {
    if (!deleting) return;
    setDeleteBusy(true);
    try {
      await deleteApp(deleting.id);
      await invalidateAll();
      toast.success(t("page.apps.toast.deleted").replace("{label}", deleting.label));
      setDeleting(null);
      if (navAppId === deleting.id) setNavAppId(null);
      if (selectedCode === deleting.name) onSelectApp?.(null);
    } catch {
      toast.error(t("page.apps.toast.deleteFailed"));
    } finally {
      setDeleteBusy(false);
    }
  }

  async function setDefault(app: AdminApp) {
    try {
      // FULL-REPLACE PUT: carry every mutable field; server clears the old default in-tx.
      await updateApp(app.id, {
        label: app.label,
        description: app.description,
        icon: app.icon,
        color: app.color,
        isDefault: true,
        sortOrder: app.sortOrder,
      });
      await invalidateAll();
      toast.success(t("page.apps.toast.defaultSet").replace("{label}", app.label));
    } catch {
      toast.error(t("page.apps.toast.defaultFailed"));
    }
  }

  const columns = useMemo<ColumnDef<AdminApp>[]>(() => [
    {
      id: "label",
      accessorKey: "label",
      header: t("page.apps.col.app"),
      cell: ({ row }) => {
        const a = row.original;
        return (
          <span className="flex items-center gap-2">
            <Icon icon={appIconByName(a.icon)} category="custom" name={appToneIcon(a.color)} size="small" />
            {/* DP-3.1: the name opens the app's overview ("hồ sơ dự án"). */}
            <Link
              href={studioHref(projectSlugAt(pathname), "dung/man-hinh/app", {}, { app: a.name })}
              className="slds-truncate"
              title={a.label}
              aria-current={selectedCode === a.name ? "true" : undefined}
              onClick={onSelectApp ? (e) => { e.preventDefault(); onSelectApp(a.name); } : undefined}
            >
              {a.label}
            </Link>
            {a.isDefault && <Badge variant="lightest">{t("page.apps.default")}</Badge>}
            {hasDraftChange(a.draftState) && <span className="slds-text-color_weak">{t(`draft.state.${a.draftState}` as I18nKey)}</span>}
          </span>
        );
      },
    },
    // Beside the menu editor the list is narrow: the code stays in the URL, the row keeps name + menu count.
    ...(split ? [] : [{ id: "name", accessorKey: "name", header: t("page.apps.col.slug") } as ColumnDef<AdminApp>]),
    {
      id: "navCount",
      accessorKey: "navCount",
      header: t("page.apps.col.menu"),
      cell: ({ row }) => t("page.apps.menuCount").replace("{n}", String(row.original.navCount)),
    },
  ], [pathname, selectedCode, onSelectApp, split]);

  const navApp = apps.find((a) => a.id === navAppId) ?? null;
  // ?app=<slug> (the list's name link) opens that app's home + default-app panel under the list.
  const appParam = useSearchParams()?.get("app");
  const homeApp = APP_HOME_BACKEND ? apps.find((a) => a.name === appParam) ?? null : null;

  const newForm = createOpen && (
    <NewAppForm
      onCancel={() => setCreateOpen(false)}
      onCreated={(name) => { setCreateOpen(false); onSelectApp?.(name); }}
    />
  );
  // Delete is confirmed in the page and names what goes (the menu) and what stays (tables and data).
  const confirmDelete = deleting && (
    <div className="slds-m-bottom_small">
      <InlineConfirm
        tone="error"
        message={`${t("page.apps.delete.title").replace("{label}", deleting.label)} ${t("page.apps.delete.body")}${delivered ? ` ${t("draft.applyNow")}: ${t("page.apps.delete.applyNow")}` : ""}`}
        confirmLabel={t("page.apps.delete.confirm")}
        cancelLabel={t("page.apps.cancel")}
        busy={deleteBusy}
        onCancel={() => setDeleting(null)}
        onConfirm={() => void onDelete()}
      />
    </div>
  );
  const table = (
    <DataTable
      label={t("page.apps.table")}
      columns={columns}
      data={apps}
      getRowId={(a) => a.id}
      loading={appsQ.isLoading}
      rowActions={(a) => (
        <>
          {!split && <MenuItem label={t("page.apps.action.editMenu")} icon={ListTree} onSelect={() => setNavAppId(a.id)} />}
          <MenuItem label={t("page.apps.action.edit")} onSelect={() => setEditing(a)} />
          {!a.isDefault && (
            <MenuItem label={t("page.apps.action.setDefault")} onSelect={() => void setDefault(a)} />
          )}
          <MenuItem label={t("page.apps.action.delete")} onSelect={() => setDeleting(a)} />
        </>
      )}
    />
  );
  const editDialog = (
    <CreateAppDialog
      open={!!editing}
      onOpenChange={(o) => { if (!o) setEditing(null); }}
      app={editing}
    />
  );

  if (split) {
    return (
      <div>
        <div className="slds-m-bottom_small">
          <p className="slds-text-color_weak slds-m-bottom_x-small">
            {appsQ.isLoading ? "" : t("page.apps.meta").replace("{n}", String(apps.length))}
            {delivered ? ` ${t("draft.applyNow")}: ${t("page.apps.applyNow.meta")}` : ""}
          </p>
          <Button variant="neutral" onClick={() => setCreateOpen(true)} disabled={createOpen}>{t("studio.build.apps.new")}</Button>
        </div>
        {newForm}
        {confirmDelete}
        {!appsQ.isLoading && apps.length === 0 && !createOpen ? (
          <EmptyState plain title={t("page.apps.empty.title")} description={t("page.apps.empty.desc")} />
        ) : table}
        {editDialog}
      </div>
    );
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
      <div className="slds-p-around_medium">
        {navApp ? (
          <NavEditor app={navApp} onBack={() => setNavAppId(null)} onSaved={invalidateAll} autoFocus />
        ) : (
          <ObjectHomePage
            icon={{ glyph: AppWindow, name: "apps" }}
            label={t("page.apps.objectLabel")}
            title={t("page.apps.title")}
            meta={appsQ.isLoading ? undefined : t("page.apps.meta").replace("{n}", String(apps.length)) + (delivered ? ` ${t("draft.applyNow")}: ${t("page.apps.applyNow.meta")}` : "")}
            actions={[{ label: t("page.apps.create"), onSelect: () => setCreateOpen(true) }]}
            empty={!appsQ.isLoading && apps.length === 0 && !createOpen && (
              <EmptyState
                size="large"
                icon={AppWindow}
                title={t("page.apps.empty.title")}
                description={t("page.apps.empty.desc")}
                action={
                  <Button variant="brand" iconLeft={Plus} onClick={() => setCreateOpen(true)}>
                    {t("page.apps.empty.cta")}
                  </Button>
                }
              />
            )}
          >
            {newForm}
            {confirmDelete}
            {table}
          </ObjectHomePage>
        )}
        {!navApp && homeApp && <div className="slds-m-top_medium"><AppHomePanel key={homeApp.id + (homeApp.updatedAt ?? "")} app={homeApp} /></div>}
      </div>
      {editDialog}
    </div>
  );
}

// ── Nav editor — replace-all, order = array order ──────────────────────────────

interface DraftNavItem {
  itemType: "entity" | "dashboard";
  refId: string;
  /** Override label; "" = inherit the entity/dashboard's own. */
  label: string;
}

type ListId = "available" | "menu";

/** What every dueling-list option gets: listbox role, selection, roving tabindex, keys. */
interface OptionProps {
  role: "option";
  "aria-selected": boolean;
  tabIndex: number;
  ref: (el: HTMLElement | null) => void;
  onFocus: () => void;
  onClick: () => void;
  onDoubleClick: () => void;
  onKeyDown: (e: KeyboardEvent) => void;
}

const keyOf = (i: { itemType: string; refId: string }) => `${i.itemType}:${i.refId}`;
const fill = (template: string, values: Record<string, string | number>) =>
  template.replace(/\{(\w+)\}/g, (m, k: string) => (k in values ? String(values[k]) : m));

// Exported (Nước 1): a project page embeds this editor inline, and the
// workspace mounts it as ?panel=menu, so "sửa nav" never requires backtracking to the list screen.
/** Draft label of a table in the menu lists ("Mới…" / "Sẽ xoá…"); nothing before delivery (the API sends no draftState). */
function DraftMark({ state }: { state?: "new" | "deleted" }) {
  const mark = draftStateLabel(state);
  return mark ? <StatusText tone={mark.tone} className="slds-m-left_x-small">{mark.label}</StatusText> : null;
}

export function NavEditor({
  app,
  onBack,
  onSaved,
  autoFocus,
}: {
  app: AdminApp;
  /** Absent = the editor sits beside the app list (AppsPage): no "back" button, and it stays open after a save. */
  onBack?: () => void;
  onSaved: () => Promise<void>;
  /** Move focus into the editor on mount — for callers whose opening control goes away or sits far
   *  before the editor in the tab order. */
  autoFocus?: boolean;
}) {
  // Delivered workspace: read tables over the draft overlay so a new table can join the menu before publish (specs/074 FR-064).
  const delivered = useDraftView().canPreview;
  const detailQ = useQuery({
    queryKey: qk.adminApp(app.id),
    queryFn: () => adminApp(app.id),
  });
  const { data: entities = [], isLoading: entitiesLoading } = useEntities({ draftRead: delivered });
  const dashboardsQ = useQuery({ queryKey: qk.dashboards(), queryFn: listDashboards });
  const dashboards = useMemo(() => dashboardsQ.data ?? [], [dashboardsQ.data]);

  // The draft starts as the loaded detail (raw rows are already sortOrder-sorted) and becomes the
  // user's own copy on the first edit; until then it follows the server.
  const base = useMemo<DraftNavItem[] | null>(
    () => detailQ.data?.items.map((r) => ({ itemType: r.itemType, refId: r.refId, label: r.label ?? "" })) ?? null,
    [detailQ.data],
  );
  const [draft, setDraft] = useState<DraftNavItem[] | null>(null);
  const items = draft ?? base;
  const setItems = (next: DraftNavItem[] | ((prev: DraftNavItem[]) => DraftNavItem[])) =>
    setDraft((d) => (typeof next === "function" ? next(d ?? base ?? []) : next));
  const [search, setSearch] = useState("");
  const [saving, setSaving] = useState(false);
  // 409 on save = someone else saved this menu meanwhile; saving again would overwrite theirs, so offer a reload.
  const [stale, setStale] = useState(false);
  // One selection PER LIST (blueprint docs: when focus leaves a list its last selected item stays
  // selected and focusable). The move buttons act on the selection of the list they move from.
  const [sel, setSel] = useState<Record<ListId, string | null>>({ available: null, menu: null });
  const [grabbed, setGrabbed] = useState(false);
  const [live, setLive] = useState("");
  // Focus follows keyboard moves. `key` = "<list>|<item key>" (the list the item lives in after the
  // move); `n` re-fires the effect when the same option moves again.
  const [focusReq, setFocusReq] = useState<{ key: string; n: number } | null>(null);
  const optionEls = useRef(new Map<string, HTMLElement>());
  const rootRef = useRef<HTMLElement>(null);
  const ids = useId();

  useLayoutEffect(() => {
    if (focusReq) optionEls.current.get(focusReq.key)?.focus();
  }, [focusReq]);
  useEffect(() => {
    if (autoFocus) rootRef.current?.focus();
  }, [autoFocus]);

  const entityById = useMemo(() => new Map(entities.map((e) => [e.id ?? "", e])), [entities]);
  const dashById = useMemo(() => new Map(dashboards.map((d) => [d.id, d])), [dashboards]);
  const menu = useMemo(() => items ?? [], [items]);
  const inNav = useMemo(() => new Set(menu.map(keyOf)), [menu]);

  const initial: AdminAppDetail | undefined = detailQ.data;
  const dirty = useMemo(() => {
    if (!initial || items === null) return false;
    const base = initial.items.map((r) => ({ itemType: r.itemType, refId: r.refId, label: r.label ?? "" }));
    return JSON.stringify(base) !== JSON.stringify(items);
  }, [initial, items]);

  // Left column: what can still be added, filtered by the search box.
  const q = search.trim().toLowerCase();
  const entityChoices = entities
    .filter((e) => (e.id ?? "") !== "" && !inNav.has(`entity:${e.id}`) && e.draftState !== "deleted") // a "Sẽ xoá" table is no valid menu target
    .filter((e) => !q || e.label.toLowerCase().includes(q) || e.name.toLowerCase().includes(q))
    .map((e) => ({ itemType: "entity" as const, refId: e.id ?? "", text: e.label, slug: e.name, title: `${e.label} (${e.name})`, draftState: e.draftState }));
  const dashChoices = dashboards
    .filter((d) => d.draftState !== "deleted" && !inNav.has(`dashboard:${d.id}`)) // a "Sẽ xoá" dashboard is no valid target (BE: APP_NAV_REF_INVALID)
    .filter((d) => !q || d.name.toLowerCase().includes(q))
    .map((d) => ({ itemType: "dashboard" as const, refId: d.id, text: d.name, slug: "", title: d.name }));
  const available = [...entityChoices, ...dashChoices];

  function resolvedLabel(it: { itemType: string; refId: string }): string {
    return it.itemType === "entity"
      ? entityById.get(it.refId)?.label ?? t("page.appMenu.orphanEntity")
      : dashById.get(it.refId)?.name ?? t("page.appMenu.orphanDashboard");
  }
  const shownLabel = (it: DraftNavItem) => it.label.trim() || resolvedLabel(it);
  // Two tables may share a label (e.g. "Khách hàng" in CRM and in ticketing): the slug tells them apart.
  const slugOf = (it: { itemType: string; refId: string }) =>
    it.itemType === "entity" ? entityById.get(it.refId)?.name ?? "" : "";

  // Assistant preview (docType app): rows its merged menu would add, drawn at the position the merge gives them.
  const proposedMenu = useProposedChanges("app", { name: app.name })[0];
  const rowsOut = useMemo(() => {
    type Slot = { it: DraftNavItem } | { ghost: { key: string; proposalKey: string; type: string; label: string } };
    const out: Slot[] = menu.map((it) => ({ it }));
    const nav = (proposedMenu?.after as { nav?: { type: string; name: string; label?: string }[] } | undefined)?.nav;
    if (!nav) return out;
    const nameOf = (it: DraftNavItem) => (it.itemType === "entity" ? entityById.get(it.refId)?.name : dashById.get(it.refId)?.name);
    const have = new Set(menu.map((it) => `${it.itemType}:${nameOf(it)}`));
    nav.forEach((n, i) => {
      if (have.has(`${n.type}:${n.name}`)) return;
      const label = n.label || entities.find((e) => e.name === n.name)?.label || n.name;
      out.splice(Math.min(i, out.length), 0, { ghost: { key: `ghost:${n.type}:${n.name}`, proposalKey: proposedMenu.proposalKey, type: n.type, label } });
    });
    return out;
  }, [menu, proposedMenu, entityById, dashById, entities]);

  const focusOn = (list: ListId, key: string) => setFocusReq((r) => ({ key: `${list}|${key}`, n: (r?.n ?? 0) + 1 }));
  const select = (list: ListId, key: string, focus = false) => {
    setSel((s) => ({ ...s, [list]: key }));
    setGrabbed(false);
    if (focus) focusOn(list, key);
  };

  // Blueprint docs: a keyboard shortcut (`follow`) keeps focus on the item, now in the target list,
  // where it is selected; the move button leaves the item unselected and focus on the button.
  function add(key: string, follow: boolean) {
    const choice = available.find((c) => keyOf(c) === key);
    if (!choice) return;
    setItems((prev) => [...prev, { itemType: choice.itemType, refId: choice.refId, label: "" }]);
    setLive(fill(t("page.appMenu.added"), { item: choice.text }));
    setSel((s) => ({ available: null, menu: follow ? key : s.menu }));
    setGrabbed(false);
    if (follow) focusOn("menu", key);
  }
  function remove(key: string, follow: boolean) {
    const it = menu.find((i) => keyOf(i) === key);
    if (!it) return;
    setItems((prev) => prev.filter((i) => keyOf(i) !== key));
    setLive(fill(t("page.appMenu.removed"), { item: shownLabel(it) }));
    setSel((s) => ({ menu: null, available: follow ? key : s.available }));
    setGrabbed(false);
    if (follow) focusOn("available", key);
  }
  function move(key: string, dir: -1 | 1, announce: "moved" | "none" = "none") {
    const idx = menu.findIndex((i) => keyOf(i) === key);
    const j = idx + dir;
    if (idx < 0 || j < 0 || j >= menu.length) return;
    setItems(arrayMove(menu, idx, j));
    if (announce === "moved") {
      setLive(fill(t("page.appMenu.moved"), { item: shownLabel(menu[idx]), pos: j + 1, total: menu.length }));
    }
  }
  function setLabel(key: string, label: string) {
    setItems((prev) => prev.map((it) => (keyOf(it) === key ? { ...it, label } : it)));
  }

  // Blueprint keyboard interaction (dueling-picklist docs).
  function onOptionKeyDown(e: KeyboardEvent, list: ListId, key: string) {
    const keys = list === "menu" ? menu.map(keyOf) : available.map(keyOf);
    const idx = keys.indexOf(key);
    const cross = e.metaKey || e.ctrlKey;
    if (list === "menu" && e.key === " ") {
      e.preventDefault();
      const it = menu[idx];
      const next = !grabbed;
      setSel((s) => ({ ...s, menu: key }));
      setGrabbed(next);
      setLive(fill(t(next ? "page.appMenu.grabbed" : "page.appMenu.dropped"), {
        item: shownLabel(it), pos: idx + 1, total: menu.length,
      }));
      return;
    }
    if (e.key === "ArrowUp" || e.key === "ArrowDown") {
      e.preventDefault();
      const dir = e.key === "ArrowUp" ? -1 : 1;
      if (list === "menu" && grabbed) {
        move(key, dir, "moved");
        focusOn("menu", key);
        return;
      }
      const nextKey = keys[idx + dir];
      if (nextKey) select(list, nextKey, true);
      return;
    }
    if (e.key === "Escape" && grabbed) {
      e.preventDefault();
      setGrabbed(false);
      setLive(fill(t("page.appMenu.dropped"), { item: shownLabel(menu[idx]), pos: idx + 1, total: menu.length }));
      return;
    }
    if (cross && e.key === "ArrowRight" && list === "available") {
      e.preventDefault();
      add(key, true);
    } else if (cross && e.key === "ArrowLeft" && list === "menu") {
      e.preventDefault();
      remove(key, true);
    }
  }

  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 4 } }));
  function onDragEnd({ active, over }: DragEndEvent) {
    const from = menu.findIndex((i) => keyOf(i) === active.id);
    const to = over ? menu.findIndex((i) => keyOf(i) === over.id) : -1;
    if (from < 0 || to < 0) return;
    if (from !== to) setItems(arrayMove(menu, from, to));
    setSel((s) => ({ ...s, menu: String(active.id) }));
    setLive(fill(t("page.appMenu.dropped"), { item: shownLabel(menu[from]), pos: to + 1, total: menu.length }));
  }

  async function save() {
    if (items === null) return;
    setSaving(true);
    try {
      const payload: AppNavReplaceItem[] = items.map((i) => ({
        itemType: i.itemType,
        refId: i.refId,
        ...(i.label.trim() !== "" ? { label: i.label.trim() } : {}),
      }));
      await replaceAppNav(app.id, payload);
      await onSaved();
      toast.success(fill(t("page.appMenu.toast.saved"), { label: app.label }));
      setStale(false);
      if (onBack) onBack();
      else setDraft(null); // follow the server again (the refetch carries what was just saved)
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) setStale(true);
      else toast.error(t("page.appMenu.toast.saveFailed"));
    } finally {
      setSaving(false);
    }
  }

  async function reload() {
    setDraft(null);
    setStale(false);
    await detailQ.refetch();
  }
  const loading = detailQ.isLoading || items === null;
  const selMenuIdx = sel.menu ? menu.findIndex((i) => keyOf(i) === sel.menu) : -1;
  const selMenuItem = selMenuIdx >= 0 ? menu[selMenuIdx] : null;
  // The search box can hide the selected option: the add button only moves what is on screen.
  const selAvailable = sel.available && available.some((c) => keyOf(c) === sel.available) ? sel.available : null;
  // Roving tabindex: the selected option of each list, else its first option.
  const tabStop = (list: ListId, keys: string[]) => {
    const k = sel[list];
    return k && keys.includes(k) ? k : keys[0];
  };
  const availableTab = tabStop("available", available.map(keyOf));
  const menuTab = tabStop("menu", menu.map(keyOf));
  const isSel = (list: ListId, key: string) => sel[list] === key;

  const optionProps = (list: ListId, key: string, tab: string | undefined): OptionProps => ({
    role: "option",
    "aria-selected": isSel(list, key),
    tabIndex: key === tab ? 0 : -1,
    ref: (el: HTMLElement | null) => {
      if (el) optionEls.current.set(`${list}|${key}`, el);
      else optionEls.current.delete(`${list}|${key}`);
    },
    // Blueprint docs: "When an item is focused, it becomes selected."
    onFocus: () => {
      if (!isSel(list, key)) select(list, key);
    },
    onClick: () => select(list, key),
    // The double-clicked node leaves its list: focus follows it (as the keyboard shortcut does).
    onDoubleClick: () => (list === "available" ? add(key, true) : remove(key, true)),
    onKeyDown: (e: KeyboardEvent) => onOptionKeyDown(e, list, key),
  });
  const labelAvailable = `${ids}-available`;
  const labelMenu = `${ids}-menu`;
  const hintId = `${ids}-hint`;

  const title = fill(t("page.appMenu.title"), { label: app.label });
  return (
    <section ref={rootRef} tabIndex={-1} aria-label={title}>
      <Card
        title={title}
        icon={{ glyph: ListTree, name: "apps" }}
        actions={
          <div className="flex gap-2">
            {onBack && <Button iconLeft={ArrowLeft} onClick={onBack}>{t("page.appMenu.back")}</Button>}
            <Button variant="brand" onClick={() => void save()} disabled={loading || !dirty || saving}>
              {t("page.appMenu.save")}
            </Button>
          </div>
        }
      >
        {stale && (
          <div className="slds-m-bottom_small">
            <ScopedNotification theme="warning">
              {t("studio.build.apps.stale")}{" "}
              <Button variant="neutral" onClick={() => void reload()}>{t("studio.build.apps.reload")}</Button>
            </ScopedNotification>
          </div>
        )}
        <p className="slds-text-color_weak slds-m-bottom_small">{t("page.appMenu.help")}</p>
        <div className="max-w-sm slds-m-bottom_small">
          <Input
            label={t("page.appMenu.search")}
            labelHidden
            type="search"
            iconLeft={Search}
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder={t("page.appMenu.searchPlaceholder")}
          />
        </div>

        <div className="slds-form-element" role="group" aria-labelledby={`${ids}-legend`}>
          <span id={`${ids}-legend`} className="slds-form-element__label slds-form-element__legend">
            {t("page.appMenu.legend")}
          </span>
          <div className="slds-form-element__control">
            <div className="slds-dueling-list">
              <div className="slds-assistive-text" aria-live="assertive">{live}</div>
              <div className="slds-assistive-text" id={hintId}>{t("page.appMenu.dragHint")}</div>

              {/* Column 1 — available */}
              <div className="slds-dueling-list__column slds-dueling-list__column_responsive">
                <span className="slds-form-element__label" id={labelAvailable}>{t("page.appMenu.available")}</span>
                <div className="slds-dueling-list__options h-96">
                  <ul aria-describedby={hintId} aria-labelledby={labelAvailable} className="slds-listbox slds-listbox_vertical" role="listbox">
                    <GroupHeader text={t("page.appMenu.groupEntities")} />
                    {entitiesLoading ? (
                      <PlainRow text={t("page.appMenu.loading")} />
                    ) : entityChoices.length === 0 ? (
                      <PlainRow text={t("page.appMenu.noMatch")} />
                    ) : (
                      entityChoices.map((c) => (
                        <li key={keyOf(c)} role="presentation" className="slds-listbox__item">
                          <div
                            {...optionProps("available", keyOf(c), availableTab)}
                            className={clsx(OPTION_CLASS, isSel("available", keyOf(c)) && "slds-is-selected")}
                          >
                            <span className="slds-media__body">
                              <span className="slds-truncate" title={c.title}>
                                {c.text}
                                <Slug slug={c.slug} selected={isSel("available", keyOf(c))} />
                                <DraftMark state={"draftState" in c ? c.draftState : undefined} />
                              </span>
                            </span>
                          </div>
                        </li>
                      ))
                    )}
                    <GroupHeader text={t("page.appMenu.groupDashboards")} />
                    {dashboardsQ.isLoading ? (
                      <PlainRow text={t("page.appMenu.loading")} />
                    ) : dashChoices.length === 0 ? (
                      <PlainRow text={dashboards.length === 0 && !q ? t("page.appMenu.noDashboards") : t("page.appMenu.noMatch")} />
                    ) : (
                      dashChoices.map((c) => (
                        <li key={keyOf(c)} role="presentation" className="slds-listbox__item">
                          <div
                            {...optionProps("available", keyOf(c), availableTab)}
                            className={clsx(OPTION_CLASS, isSel("available", keyOf(c)) && "slds-is-selected")}
                          >
                            <span className="slds-media__body">
                              <span className="slds-truncate" title={c.title}>
                                {c.text}
                                <Slug slug={c.slug} selected={isSel("available", keyOf(c))} />
                              </span>
                            </span>
                          </div>
                        </li>
                      ))
                    )}
                  </ul>
                </div>
              </div>

              {/* Column 2 — move between lists */}
              <div className="slds-dueling-list__column">
                {/* Never disabled (blueprint): a button that disabled itself under the user would drop
                    keyboard focus. With nothing selected in its list it does nothing. */}
                <ButtonIcon
                  variant="container"
                  icon={ChevronRight}
                  label={t("page.appMenu.add")}
                  onClick={() => selAvailable && add(selAvailable, false)}
                />
                <ButtonIcon
                  variant="container"
                  icon={ChevronLeft}
                  label={t("page.appMenu.remove")}
                  onClick={() => selMenuItem && remove(keyOf(selMenuItem), false)}
                />
              </div>

              {/* Column 3 — the app's menu, in display order */}
              <div className="slds-dueling-list__column slds-dueling-list__column_responsive">
                <span className="slds-form-element__label" id={labelMenu}>
                  {fill(t("page.appMenu.selected"), { n: menu.length })}
                </span>
                <div className="slds-dueling-list__options h-96">
                  {loading ? (
                    <PlainList><PlainRow text={t("page.appMenu.loading")} /></PlainList>
                  ) : (
                    <DndContext
                      sensors={sensors}
                      collisionDetection={closestCenter}
                      onDragStart={({ active }) => select("menu", String(active.id))}
                      onDragEnd={onDragEnd}
                      accessibility={DND_SILENT}
                    >
                      <SortableContext items={menu.map(keyOf)} strategy={verticalListSortingStrategy}>
                        <ul aria-describedby={hintId} aria-labelledby={labelMenu} className="slds-listbox slds-listbox_vertical" role="listbox">
                          {menu.length === 0 && <PlainRow text={t("page.appMenu.emptyMenu")} />}
                          {rowsOut.map((slot) => {
                            if ("ghost" in slot) {
                              const g = slot.ghost;
                              return (
                                <li key={g.key} role="presentation" className="slds-listbox__item nx-proposed" data-proposal-key={g.proposalKey}>
                                  <div className={`${OPTION_CLASS} slds-text-color_default`} role="presentation">
                                    <span className="slds-media__figure slds-listbox__option-icon">
                                      <span className="slds-icon_container slds-current-color">
                                        {g.type === "dashboard"
                                          ? <LayoutDashboard aria-hidden="true" className="slds-icon slds-icon_x-small nx-lucide" />
                                          : <Boxes aria-hidden="true" className="slds-icon slds-icon_x-small nx-lucide" />}
                                      </span>
                                    </span>
                                    <span className="slds-media__body">
                                      <span className="slds-truncate">
                                        {fill(t("studio.build.ai.menu.add"), { label: g.label })} <StatusText tone="proposed">{t("studio.status.proposed")}</StatusText>
                                      </span>
                                    </span>
                                  </div>
                                </li>
                              );
                            }
                            const it = slot.it;
                            const key = keyOf(it);
                            const picked = isSel("menu", key);
                            const custom = it.label.trim();
                            return (
                              <SortableOption
                                key={key}
                                id={key}
                                grabbed={picked && grabbed}
                                selected={picked}
                                props={optionProps("menu", key, menuTab)}
                              >
                                <span className="slds-media__figure slds-listbox__option-icon">
                                  <span className="slds-icon_container slds-current-color">
                                    {it.itemType === "dashboard"
                                      ? <LayoutDashboard aria-hidden="true" className="slds-icon slds-icon_x-small nx-lucide" />
                                      : <Boxes aria-hidden="true" className="slds-icon slds-icon_x-small nx-lucide" />}
                                  </span>
                                </span>
                                <span className="slds-media__body">
                                  <span className="slds-truncate" title={shownLabel(it)}>
                                    {shownLabel(it)}
                                    {custom && custom !== resolvedLabel(it) &&
                                      ` ${fill(t("page.appMenu.original"), { label: resolvedLabel(it) })}`}
                                    <Slug slug={slugOf(it)} selected={picked} />
                                    <DraftMark state={it.itemType === "entity" ? entityById.get(it.refId)?.draftState : undefined} />
                                  </span>
                                </span>
                              </SortableOption>
                            );
                          })}
                        </ul>
                      </SortableContext>
                    </DndContext>
                  )}
                </div>
              </div>

              {/* Column 4 — reorder */}
              <div className="slds-dueling-list__column">
                <ButtonIcon
                  variant="container"
                  icon={ChevronUp}
                  label={t("page.appMenu.up")}
                  onClick={() => selMenuItem && move(keyOf(selMenuItem), -1, "moved")}
                />
                <ButtonIcon
                  variant="container"
                  icon={ChevronDown}
                  label={t("page.appMenu.down")}
                  onClick={() => selMenuItem && move(keyOf(selMenuItem), 1, "moved")}
                />
              </div>
            </div>
          </div>
        </div>

        {/* Per-item label override for the selected menu item. */}
        <div className="max-w-md slds-m-top_medium">
          {selMenuItem ? (
            <Input
              label={fill(t("page.appMenu.labelField"), { item: resolvedLabel(selMenuItem) })}
              help={t("page.appMenu.labelHelp")}
              value={selMenuItem.label}
              maxLength={100}
              placeholder={resolvedLabel(selMenuItem)}
              onChange={(e) => setLabel(keyOf(selMenuItem), e.target.value)}
            />
          ) : (
            <p className="slds-text-color_weak">{t("page.appMenu.labelNone")}</p>
          )}
        </div>
      </Card>
    </section>
  );
}

const OPTION_CLASS = "slds-listbox__option slds-listbox__option_plain slds-media slds-media_small slds-media_inline";

/** Secondary text after an option's label: the table's slug (weak text), nothing for dashboards.
 *  A selected option is white on brand blue, where the grey weak colour would fail contrast — there
 *  the slug keeps the option's own colour. */
function Slug({ slug, selected }: { slug: string; selected: boolean }) {
  return slug ? <span className={selected ? undefined : "slds-text-color_weak"}> {slug}</span> : null;
}

// dnd-kit announces through its own region; the blueprint's live region above carries our
// Vietnamese position messages, so dnd-kit's (English) ones are silenced.
const DND_SILENT = {
  announcements: {
    onDragStart: () => undefined,
    onDragOver: () => undefined,
    onDragEnd: () => undefined,
    onDragCancel: () => undefined,
  },
  screenReaderInstructions: { draggable: "" },
};

function SortableOption({
  id, grabbed, selected, props, children,
}: {
  id: string;
  grabbed: boolean;
  selected: boolean;
  props: OptionProps;
  children: React.ReactNode;
}) {
  // Only `listeners` (pointer) — the option keeps its listbox role/keys, not dnd-kit's button attributes.
  const { listeners, setNodeRef, transform, transition, isDragging } = useSortable({ id });
  const { ref, ...rest } = props;
  return (
    <li role="presentation" className="slds-listbox__item">
      <div
        {...rest}
        {...listeners}
        ref={(el) => { setNodeRef(el); ref(el); }}
        style={{ transform: CSS.Transform.toString(transform), transition }}
        className={clsx(OPTION_CLASS, selected && "slds-is-selected", (grabbed || isDragging) && "slds-is-grabbed")}
      >
        {children}
      </div>
    </li>
  );
}

function GroupHeader({ text }: { text: string }) {
  return (
    <li role="presentation" className="slds-listbox__item">
      <div className="slds-media slds-listbox__option slds-listbox__option_plain slds-media_small" role="presentation">
        <h3 className="slds-listbox__option-header" role="presentation">{text}</h3>
      </div>
    </li>
  );
}

function PlainRow({ text }: { text: string }) {
  return (
    <li role="presentation" className="slds-listbox__item">
      <div className="slds-media slds-listbox__option slds-listbox__option_plain slds-media_small" role="presentation">
        <span className="slds-media__body slds-text-color_weak">{text}</span>
      </div>
    </li>
  );
}

function PlainList({ children }: { children: React.ReactNode }) {
  return <ul className="slds-listbox slds-listbox_vertical" role="presentation">{children}</ul>;
}
