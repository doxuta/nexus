"use client";

// SavedViews — the list's saved-views switcher (W9c, ../specs/018-saved-views/spec.md + api-conventions
// §13.2) on SLDS 2.264.1 (specs/038 T022): the object-home header's list-view switcher —
// page-headers/object-home `slds-page-header__name-switcher` holding a menus/dropdown — plus
// modals/base (save, rename) and prompt/base (delete).
//
// The menu lists the entity's views (personal + shared, shared marked "chung"); selecting one APPLIES
// its config (filters + sort + density + hidden columns) via the parent's single-fetch applyView.
// The active view's name becomes the page title (onStatusChange), and the parent says when the live
// list has drifted from it. Owner/admin can update (resend config), rename (resend name), or delete
// the active view; anyone can save the current list state as a new view; only admins may make a
// view shared.
//
// Ownership rules (canMutateView / canCreateShared) mirror the backend ViewService exactly — but the
// server is the real gate: a hidden button is UX, a 403/404 toast is the enforcement.

import { ChevronDown } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import {
  ensureAuth, listViews, createView, updateView, deleteView, type SavedView,
} from "@/lib/api";
import {
  serializeViewConfig, parseViewConfig, applyViewConfig, viewConfigEqual,
  canMutateView, canCreateShared, type ViewState,
} from "@/lib/viewConfig";
import {
  loadLastAppliedViewId, saveLastAppliedViewId, clearLastAppliedViewId,
} from "@/lib/listState";
import { errorMessage, errorCode } from "@/lib/errorMessages";
import { useAuth } from "@/lib/auth/AuthProvider";
import { t } from "@/lib/i18n";
import { toast } from "@/components/ui/Toaster";
import { Button } from "@/components/slds/Button";
import { Checkbox } from "@/components/slds/Checkbox";
import { Input } from "@/components/slds/Input";
import { Menu, MenuCheckboxItem, MenuDivider, MenuGroup, MenuItem } from "@/components/slds/Menu";
import { Modal, Prompt } from "@/components/slds/Modal";

export interface SavedViewStatus {
  /** Name of the applied view, null when none is applied. */
  name: string | null;
  /** The live list has drifted from the applied view's saved config. */
  dirty: boolean;
}

export interface SavedViewsProps {
  entity: string;
  /** Live list state a save/dirty-check captures. */
  current: ViewState;
  /** Apply a view's decoded state — the parent runs ONE explicit fetch. */
  onApply: (state: ViewState) => void;
  /**
   * F7: re-apply the view the user LAST APPLIED for this entity (localStorage
   * `list:<entity>:lastView`) once the list loads — the honest client-side stand-in for a
   * backend "default view" flag. The parent passes false when the URL carried explicit list
   * state (deep links stay authoritative). Runs at most once per mount.
   */
  autoApply?: boolean;
  /** Active view name + dirty flag, for the page title and meta line. */
  onStatusChange?: (status: SavedViewStatus) => void;
}

// Defer opening a dialog until AFTER the Radix menu has closed + returned focus — opening in the same
// tick races the menu's focus return against the dialog's focus trap (dialog can flash-close).
function deferOpen(fn: () => void) {
  return () => setTimeout(fn, 0);
}

export function SavedViews({ entity, current, onApply, autoApply = false, onStatusChange }: SavedViewsProps) {
  const { user } = useAuth();
  const viewer = { userId: user?.id, isAdmin: !!user?.isAdmin };

  const [views, setViews] = useState<SavedView[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadErr, setLoadErr] = useState("");
  const [activeId, setActiveId] = useState<string | null>(null);

  // Save / rename dialog.
  const [dialogMode, setDialogMode] = useState<"create" | "rename" | null>(null);
  const [nameInput, setNameInput] = useState("");
  const [sharedInput, setSharedInput] = useState(false);
  const [saving, setSaving] = useState(false);

  // Delete confirm.
  const [deleteTarget, setDeleteTarget] = useState<SavedView | null>(null);
  const [deleting, setDeleting] = useState(false);

  const activeView = activeId ? views.find((v) => v.id === activeId) ?? null : null;
  const canMutateActive = activeView ? canMutateView(activeView, viewer) : false;

  // Dirty = the live list has drifted from the applied view's saved config.
  const dirty =
    !!activeView && !viewConfigEqual(serializeViewConfig(current), parseViewConfig(activeView.config));

  const activeName = activeView?.name ?? null;
  useEffect(() => {
    onStatusChange?.({ name: activeName, dirty });
  }, [activeName, dirty, onStatusChange]);

  // Returns the loaded list ([] on error) so the first load can auto-apply from it.
  const loadViews = useCallback(async (): Promise<SavedView[]> => {
    setLoading(true);
    setLoadErr("");
    try {
      await ensureAuth();
      const vs = await listViews(entity);
      setViews(vs);
      return vs;
    } catch (e) {
      setLoadErr(errorMessage(e));
      return [];
    } finally {
      setLoading(false);
    }
  }, [entity]);

  const handleApply = (v: SavedView) => {
    onApply(applyViewConfig(parseViewConfig(v.config)));
    setActiveId(v.id);
    saveLastAppliedViewId(entity, v.id); // F7: remembered → auto-applies on the next clean visit
  };

  // Fetch on mount. The parent passes key={entity} so this component remounts (and this effect
  // re-runs, active view reset) whenever the list switches entity.
  // F7 auto-apply rides the FIRST load only (mutation reloads never re-apply), from the list it
  // just fetched rather than from a second effect watching `views` — applying from that effect set
  // state synchronously in an effect body. Quietly skips when the stored id no longer exists
  // (deleted elsewhere / another user's browser profile).
  const loadedRef = useRef(false);
  useEffect(() => {
    if (loadedRef.current) return;
    loadedRef.current = true;
    void (async () => {
      const vs = await loadViews();
      if (!autoApply) return;
      const lastId = loadLastAppliedViewId(entity);
      const v = lastId ? vs.find((x) => x.id === lastId) : undefined;
      if (v) handleApply(v);
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [loadViews]);

  const openCreate = () => {
    setDialogMode("create");
    setNameInput("");
    setSharedInput(false);
  };
  const openRename = () => {
    if (!activeView) return;
    setDialogMode("rename");
    setNameInput(activeView.name);
  };

  const closeDialog = () => {
    if (saving) return;
    setDialogMode(null);
  };

  const submitDialog = async () => {
    const name = nameInput.trim();
    if (!name) {
      toast.error(t("page.list.views.nameRequired"));
      return;
    }
    setSaving(true);
    try {
      if (dialogMode === "create") {
        const { id } = await createView(entity, {
          name,
          config: serializeViewConfig(current) as unknown as Record<string, unknown>,
          shared: sharedInput,
        });
        await loadViews();
        setActiveId(id);
        toast.success(t("page.list.views.saved").replace("{name}", name));
      } else if (dialogMode === "rename" && activeView) {
        // PUT is a full-replace of {name, config} → resend the view's EXISTING config unchanged.
        await updateView(entity, activeView.id, { name, config: activeView.config });
        await loadViews();
        toast.success(t("page.list.views.renamed"));
      }
      setDialogMode(null);
    } catch (e) {
      toast.error(errorMessage(e));
      // Keep the dialog open for a fixable validation error so the user can correct the name.
      const code = errorCode(e);
      if (code !== "VIEW_NAME_REQUIRED" && code !== "VIEW_NAME_TOO_LONG") setDialogMode(null);
    } finally {
      setSaving(false);
    }
  };

  // "Cập nhật view" — persist the current live state into the active view (resend its name).
  const updateActive = async () => {
    if (!activeView) return;
    try {
      await updateView(entity, activeView.id, {
        name: activeView.name,
        config: serializeViewConfig(current) as unknown as Record<string, unknown>,
      });
      await loadViews();
      toast.success(t("page.list.views.updated").replace("{name}", activeView.name));
    } catch (e) {
      toast.error(errorMessage(e));
    }
  };

  const confirmDelete = async () => {
    if (!deleteTarget) return;
    setDeleting(true);
    try {
      await deleteView(entity, deleteTarget.id);
      if (activeId === deleteTarget.id) setActiveId(null);
      // Match-guarded: forgetting view A must not drop a later-applied view B.
      clearLastAppliedViewId(entity, deleteTarget.id);
      await loadViews();
      toast.success(t("page.list.views.deleted"));
      setDeleteTarget(null);
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setDeleting(false);
    }
  };

  // The dialogs open from this menu's items (after the menu closed and gave focus back to it), so
  // closing one returns focus here. The kit Modal remembers its opener only when Radix moves focus
  // in; the name input's autoFocus gets there first, so the opener is named explicitly.
  const triggerRef = useRef<HTMLButtonElement>(null);
  const switchLabel = t("page.list.views.switch");
  const shared = t("page.list.views.shared");

  return (
    <>
      <Menu
        size="small"
        length={10}
        trigger={
          <button ref={triggerRef} type="button" className="slds-button slds-button_icon slds-button_icon-small" title={switchLabel}>
            <ChevronDown aria-hidden="true" className="slds-button__icon slds-icon_x-small nx-lucide" />
            <span className="slds-assistive-text">{switchLabel}</span>
          </button>
        }
      >
        <MenuGroup label={t("page.list.views.title")}>
          {loading ? (
            <MenuItem label={t("page.list.views.loading")} disabled />
          ) : loadErr ? (
            <MenuItem label={loadErr} tone="error" disabled />
          ) : views.length === 0 ? (
            <MenuItem label={t("page.list.views.empty")} disabled />
          ) : (
            views.map((v) => (
              <MenuCheckboxItem
                key={v.id}
                label={v.shared ? `${v.name} · ${shared}` : v.name}
                checked={v.id === activeId}
                onCheckedChange={() => handleApply(v)}
              />
            ))
          )}
        </MenuGroup>
        <MenuDivider />
        {activeView && dirty && canMutateActive && (
          <MenuItem label={t("page.list.views.update")} onSelect={() => void updateActive()} />
        )}
        {activeView && canMutateActive && <MenuItem label={t("page.list.views.rename")} onSelect={deferOpen(openRename)} />}
        {activeView && canMutateActive && (
          <MenuItem label={t("page.list.views.delete")} onSelect={deferOpen(() => setDeleteTarget(activeView))} />
        )}
        <MenuItem label={t("page.list.views.saveCurrent")} onSelect={deferOpen(openCreate)} />
      </Menu>

      {/* Save / rename dialog (modals/base, small) */}
      <Modal
        open={dialogMode !== null}
        onOpenChange={(o) => !o && closeDialog()}
        returnFocus={() => triggerRef.current}
        size="small"
        title={t(dialogMode === "rename" ? "page.list.views.renameTitle" : "page.list.views.saveTitle")}
        tagline={t(dialogMode === "rename" ? "page.list.views.renameHint" : "page.list.views.saveHint")}
        footer={
          <>
            <Button onClick={closeDialog} disabled={saving}>
              {t("action.cancel")}
            </Button>
            <Button variant="brand" onClick={() => void submitDialog()} disabled={saving || !nameInput.trim()}>
              {t(dialogMode === "rename" ? "page.list.views.saveName" : "page.list.views.save")}
            </Button>
          </>
        }
      >
        <Input
          label={t("page.list.views.name")}
          value={nameInput}
          onChange={(e) => setNameInput(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && nameInput.trim() && !saving) void submitDialog();
          }}
          maxLength={150}
          autoFocus
          placeholder={t("page.list.views.namePlaceholder")}
        />
        {dialogMode === "create" && canCreateShared(viewer) && (
          <div className="slds-m-top_small">
            <Checkbox
              label={t("page.list.views.sharedLabel")}
              help={t("page.list.views.sharedHelp")}
              checked={sharedInput}
              onChange={(e) => setSharedInput(e.target.checked)}
            />
          </div>
        )}
      </Modal>

      {/* Delete confirm (prompt/base) */}
      <Prompt
        open={deleteTarget !== null}
        onOpenChange={(o) => !o && !deleting && setDeleteTarget(null)}
        returnFocus={() => triggerRef.current}
        title={t("page.list.views.deleteTitle")}
        theme="error"
        footer={
          <>
            <Button onClick={() => setDeleteTarget(null)} disabled={deleting}>
              {t("action.cancel")}
            </Button>
            <Button variant="destructive" onClick={() => void confirmDelete()} disabled={deleting}>
              {t("page.list.views.delete")}
            </Button>
          </>
        }
      >
        {deleteTarget && <p>{t("page.list.views.deleteBody").replace("{name}", deleteTarget.name)}</p>}
      </Prompt>
    </>
  );
}
