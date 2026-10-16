"use client";

// C4/C5 — Roles list + ACL Matrix editor
// Two-pane: left = role list, right = ACLMatrix widget for selected role.
// Endpoints: GET /admin/roles · GET /admin/roles/:id · POST /admin/roles · DELETE /admin/roles/:id
// PUT (replace-set) is handled inside ACLMatrix.tsx.
// SLDS (specs/038 T048, RESEARCH §5 /admin/roles): the left pane is split-view/base (header with the
// create button, listbox of roles, the open one marked aria-current="page" like the blueprint's
// "Selected Item"); the right pane is page-headers/record-home for the role (delete) over the matrix
// card. Create = modals/base, delete and "leave unsaved changes" = prompt.
// Deviations: the split-view title is an h2 — the role's record header on the right is the page's h1;
// the open role also carries aria-selected="true" (a listbox option must state its selection).

import { useCallback, useEffect, useId, useRef, useState, type KeyboardEvent } from "react";
import { Plus, ShieldCheck } from "lucide-react";
import {
  listRoles, createRole,
  ensureAuth, ApiError,
  type RoleSummary,
} from "@/lib/admin";
import { t } from "@/lib/i18n";
import { toast } from "@/components/ui/Toaster";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { fill } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { Icon } from "@/components/slds/Icon";
import { Input } from "@/components/slds/Input";
import { Modal, Prompt } from "@/components/slds/Modal";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { ScopeHiddenNote } from "@/components/studio/ScopeHiddenNote";
import { useScopedRoleIds, useWorkshopScope } from "@/lib/workshopScope";
import { RoleEditor, type RolesListViewAs } from "./RoleEditor";
import { useDirectApply } from "@/components/shell/useDirectApply"; // create modal

const roleText = (r: RoleSummary) => r.label ?? r.name;

export type { RolesListViewAs };

/**
 * `viewAs` (Studio only): "Xem như vai này" on the open role.
 * This is the customer face's /admin/roles (split view, editor on the right, Modal/Prompt). The Studio no longer mounts
 * it: `dung/nguoi-dung-quyen` is build/AccessPage (roles x tables matrix, "+ Vai mới" in place; specs/074 lát 15).
 */
export function RolesList({ viewAs }: { viewAs?: RolesListViewAs } = {}) {
  const [roles, setRoles] = useState<RoleSummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [matrixDirty, setMatrixDirty] = useState(false);
  // Role the user asked to open while the matrix had unsaved changes (the prompt decides).
  const [pendingId, setPendingId] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  // Inside an app workshop (#302): only the roles that reach this app, one click from "Xem tất cả".
  const workshop = useWorkshopScope();
  const [showAll, setShowAll] = useState(false);
  const { relevant, pending } = useScopedRoleIds(roles, workshop);
  // While the roles' grants are still being read nothing is shown (no flash of every app's roles, and no
  // auto-selected role of another app).
  const shownRoles = pending ? [] : relevant && !showAll ? roles.filter((r) => relevant.has(r.id)) : roles;

  useEffect(() => {
    (async () => {
      try {
        await ensureAuth();
        const r = await listRoles();
        setRoles(r);
        // Auto-select first role if none selected
        setSelectedId((cur) => cur ?? r[0]?.id ?? null);
      } catch (e) {
        setErr(e instanceof ApiError ? e.message : t("page.adminRoles.loadError"));
      } finally {
        setLoading(false);
      }
    })();
  }, []);

  function openRole(id: string | null) {
    setSelectedId(id);
    setMatrixDirty(false);
  }

  function handleSelectRole(id: string) {
    if (id === selectedId) return;
    if (matrixDirty) setPendingId(id);
    else openRole(id);
  }

  async function handleCreated(role: RoleSummary) {
    setCreating(false);
    setRoles(await listRoles());
    openRole(role.id);
  }

  async function handleDeleted() {
    const r = await listRoles();
    setRoles(r);
    openRole(r[0]?.id ?? null);
  }

  const selected = shownRoles.find((r) => r.id === selectedId) ?? shownRoles[0] ?? null;

  const waiting = loading || pending;

  return (
    <div className="flex min-h-0 flex-1 flex-col lg:flex-row">
      {/* ── Left pane: role list (split-view) ── */}
      <div className="slds-split-view_container slds-is-open max-h-72 w-full flex-none lg:max-h-none lg:w-72">
        <article className="slds-split-view slds-grid slds-grid_vertical slds-grow min-h-0">
          <header className="slds-split-view__header">
            <div className="slds-grid slds-grid_vertical-align-center slds-m-bottom_xx-small">
              <div className="slds-has-flexi-truncate">
                <div className="slds-media slds-media_center">
                  <div className="slds-media__figure">
                    <Icon icon={ShieldCheck} category="standard" name="user-role" size="small" />
                  </div>
                  <div className="slds-media__body">
                    <h2 className="slds-text-heading_small slds-text-color_default slds-p-right_x-small slds-truncate">
                      {t("page.adminRoles.listTitle")}
                    </h2>
                  </div>
                </div>
              </div>
              <div className="slds-no-flex slds-grid">
                <ButtonIcon icon={Plus} label={t("page.adminRoles.create")} variant="border-filled" onClick={() => setCreating(true)} />
              </div>
            </div>
            {workshop?.entityNames && relevant && (
              <ScopeHiddenNote hidden={roles.length - shownRoles.length} noun="roles" showAll={showAll} onToggle={() => setShowAll((v) => !v)} />
            )}
            <p className="slds-text-body_small slds-text-color_weak">
              {waiting ? " " : fill(t("page.adminRoles.count"), { n: shownRoles.length })}
            </p>
          </header>
          <div className="slds-grid slds-grid_vertical min-h-0 flex-1">
            <div className="slds-split-view__list-header slds-grid">
              <span>{t("page.adminRoles.listHeader")}</span>
            </div>
            {waiting ? (
              <div className="slds-is-relative p-6">
                <Spinner size="small" />
              </div>
            ) : err ? (
              <div className="p-3">
                <ScopedNotification theme="error">{err}</ScopedNotification>
              </div>
            ) : shownRoles.length === 0 ? (
              <p className="px-6 py-3 slds-text-color_weak">{t("page.adminRoles.empty")}</p>
            ) : (
              <RoleOptions roles={shownRoles} selectedId={selected?.id ?? null} onSelect={handleSelectRole} />
            )}
          </div>
        </article>
      </div>

      {/* ── Right pane: role header + ACL matrix ── */}
      <div className="flex min-w-0 flex-1 flex-col gap-3 overflow-y-auto p-4">
        {!selected ? (
          !waiting && (
            <EmptyState
              size="large"
              icon={ShieldCheck}
              title={t("page.adminRoles.pick.title")}
              description={t("page.adminRoles.pick.desc")}
            />
          )
        ) : (
          <>
            <RoleEditor
              key={selected.id}
              roleId={selected.id}
              viewAs={viewAs}
              onDirtyChange={setMatrixDirty}
              onDeleted={() => void handleDeleted()}
              entityScope={showAll ? null : workshop?.entityNames ?? null}
            />
          </>
        )}
      </div>

      <CreateRoleModal open={creating} onClose={() => setCreating(false)} onCreated={(r) => void handleCreated(r)} />

      <Prompt
        open={pendingId !== null}
        onOpenChange={(o) => { if (!o) setPendingId(null); }}
        title={t("page.adminRoles.dirty.title")}
        footer={
          <>
            <Button onClick={() => setPendingId(null)}>{t("page.adminRoles.dirty.stay")}</Button>
            <Button
              variant="brand"
              onClick={() => {
                if (pendingId) openRole(pendingId);
                setPendingId(null);
              }}
            >
              {t("page.adminRoles.dirty.discard")}
            </Button>
          </>
        }
      >
        {t("page.adminRoles.dirty.body")}
      </Prompt>

    </div>
  );
}

/** Split-view listbox: one tab stop (the open role), arrows / Home / End move, Enter / Space opens. */
function RoleOptions({
  roles, selectedId, onSelect,
}: { roles: RoleSummary[]; selectedId: string | null; onSelect: (id: string) => void }) {
  const refs = useRef<(HTMLAnchorElement | null)[]>([]);
  const current = Math.max(0, roles.findIndex((r) => r.id === selectedId));

  const onKeyDown = useCallback((e: KeyboardEvent<HTMLAnchorElement>, i: number, id: string) => {
    const to = { ArrowDown: i + 1, ArrowUp: i - 1, Home: 0, End: roles.length - 1 }[e.key];
    if (to !== undefined) {
      e.preventDefault();
      refs.current[Math.max(0, Math.min(roles.length - 1, to))]?.focus();
    } else if (e.key === " ") {
      e.preventDefault();
      onSelect(id);
    }
  }, [roles.length, onSelect]);

  return (
    <ul className="slds-scrollable_y min-h-0 flex-1" role="listbox" aria-label={t("page.adminRoles.listLabel")}>
      {roles.map((r, i) => (
        <li key={r.id} className="slds-split-view__list-item" role="presentation">
          <a
            ref={(el) => { refs.current[i] = el; }}
            href="#"
            role="option"
            aria-current={r.id === selectedId ? "page" : undefined}
            // An option must state its selection (ARIA listbox); SLDS styles aria-selected=true the same.
            aria-selected={r.id === selectedId}
            tabIndex={i === current ? 0 : -1}
            className="slds-split-view__list-item-action slds-grow slds-has-flexi-truncate"
            onClick={(e) => { e.preventDefault(); onSelect(r.id); }}
            onKeyDown={(e) => onKeyDown(e, i, r.id)}
          >
            <div className="slds-grid slds-wrap">
              <span className="slds-truncate slds-text-body_regular slds-text-color_default" title={roleText(r)}>{roleText(r)}</span>
            </div>
            <div className="slds-grid slds-wrap">
              <span className="slds-truncate" title={r.name}>{r.name}</span>
            </div>
          </a>
        </li>
      ))}
    </ul>
  );
}

// ── Create role ────────────────────────────────────────────────────────────────
function CreateRoleModal({
  open, onClose, onCreated,
}: { open: boolean; onClose: () => void; onCreated: (role: RoleSummary) => void }) {
  const formId = useId();
  const [newName, setNewName] = useState("");
  const [newLabel, setNewLabel] = useState("");
  const [saving, setSaving] = useState(false);
  const { guard, dialog } = useDirectApply();

  function close() {
    if (saving) return;
    setNewName("");
    setNewLabel("");
    onClose();
  }

  function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    if (!newName.trim() || !newLabel.trim()) return;
    void guard(t("direct.what.role"), doCreate);
  }

  async function doCreate() {
    setSaving(true);
    try {
      const role = await createRole({ name: newName.trim(), label: newLabel.trim() });
      toast.success(fill(t("page.adminRoles.created"), { label: role.label ?? role.name }));
      setNewName("");
      setNewLabel("");
      onCreated(role);
    } catch (ex) {
      toast.error(ex instanceof ApiError ? ex.message : t("page.adminRoles.createFailed"));
    } finally {
      setSaving(false);
    }
  }

  return (
    <>
    <Modal
      open={open}
      onOpenChange={(o) => { if (!o) close(); }}
      title={t("page.adminRoles.createTitle")}
      size="small"
      footer={
        <>
          <Button disabled={saving} onClick={close}>{t("page.adminRoles.cancel")}</Button>
          <Button variant="brand" type="submit" form={formId} disabled={saving}>{t("page.adminRoles.createSubmit")}</Button>
        </>
      }
    >
      <form id={formId} onSubmit={handleCreate} className="flex flex-col gap-3">
        <Input
          label={t("page.adminRoles.field.name")}
          required
          value={newName}
          onChange={(e) => setNewName(e.target.value)}
          placeholder={t("page.adminRoles.field.namePlaceholder")}
        />
        <Input
          label={t("page.adminRoles.field.label")}
          required
          value={newLabel}
          onChange={(e) => setNewLabel(e.target.value)}
        />
      </form>
    </Modal>
    {dialog}
    </>
  );
}
