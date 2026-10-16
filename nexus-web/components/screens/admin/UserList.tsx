"use client";

// C1 — User List · C2 — Create User · C3 — User Detail/Edit
// State machine: "list" | "detail"; create is a modal over the list.
// Endpoints verified: GET/POST /admin/users, GET/PUT/DELETE /admin/users/:id,
//                     POST /admin/users/:id/reset-link (invite).
// SLDS (specs/038 T048, RESEARCH §5 /admin/users): list = ObjectHomePage (page-headers/object-home
// + data-tables/advanced, row action menu); create + invite = modals; detail = page-headers/record-home
// (highlights) + a card with the record form (form-element stacked, radio-button-group for the status),
// delete behind a prompt.
// specs/065 lát B: the SAME list serves the customer owner at /settings/users (mode="owner", /org-admin/*): no
// add / invite / delete / admin flag / password; rows the owner may not change are read-only; the detail form
// has exactly name, role (assignable ones) and active/locked.

import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import {
  ArrowLeft, Check, Copy, Eye, EyeOff, KeyRound, Link2, MailCheck, SearchX, UserCheck, UserRound, UserX, Users,
} from "lucide-react";
import {
  listUsers, getUser, createUser, updateUser, deleteUser,
  listRoles, createResetLink, listUserSessions, endUserSession, setUserManager,
  ensureAuth, ApiError,
  type AdminUser, type RoleSummary, type CreateUserInput, type UpdateUserInput,
} from "@/lib/admin";
import {
  listOrgUsers, listOrgRoles, getOrgUser, updateOrgUser, createOrgUser, issueOrgInviteLink,
  listOrgUserSessions, endOrgUserSession, type OrgTier, type OrgUser,
} from "@/lib/orgAdmin";
import { useAuth } from "@/lib/auth/AuthProvider";
import { inviteLinkView, type ResetLinkResult } from "@/lib/invite";
import { t } from "@/lib/i18n";
import { toast } from "@/components/ui/Toaster";
import { Avatar } from "@/components/slds/Avatar";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { Card } from "@/components/slds/Card";
import { Checkbox } from "@/components/slds/Checkbox";
import { DataTable, fill } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { Input } from "@/components/slds/Input";
import { MenuItem } from "@/components/slds/Menu";
import { Modal, Prompt } from "@/components/slds/Modal";
import { PageHeader } from "@/components/slds/PageHeader";
import { RadioButtonGroup } from "@/components/slds/RadioGroup";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";
import { Spinner } from "@/components/slds/Spinner";
import { ActionGroup } from "@/components/slds/templates/ActionGroup";
import { ObjectHomePage } from "@/components/slds/templates/ObjectHomePage";
import { ScopeHiddenNote } from "@/components/studio/ScopeHiddenNote";
import { useScopedRoleIds, useWorkshopScope } from "@/lib/workshopScope";
import { UserSessionsCard } from "@/components/screens/admin/UserSessionsCard";
import { useEmbedded } from "@/components/slds/EmbeddedSurface";
import { UserListStudio } from "@/components/studio/operate/UserListStudio";

/** A list row: the admin's AdminUser, or an owner-mode account carrying tier + manageable. */
type UserRow = AdminUser & { tier?: OrgTier; manageable?: boolean };

const displayName = (u: { fullName?: string; email: string }) => u.fullName || u.email;

/** Owner mode has no tenant id and no admin flag on the wire: adapt to the shape the table already renders. */
const fromOrg = (u: OrgUser): UserRow => ({
  id: u.id, tenantId: "", email: u.email, fullName: u.fullName ?? "", status: u.status, roleId: u.roleId,
  isAdmin: u.tier === "admin", canManageUsers: u.tier === "owner", tier: u.tier, manageable: u.manageable,
});

function StatusBadge({ status, owner = false }: { status: string; owner?: boolean }) {
  // Owner mode says "Khoá" (spec 065 FR-037), the admin screen says "Vô hiệu".
  return status === "active"
    ? <Badge theme="success">{t("page.adminUsers.status.active")}</Badge>
    : <Badge>{t(owner ? "settings.users.status.locked" : "page.adminUsers.status.inactive")}</Badge>;
}

const roleOptions = (roles: RoleSummary[]) => roles.map((r) => ({ value: r.id, label: r.label ?? r.name }));

// ── User List (C1) ────────────────────────────────────────────────────────────
/** In the Studio work surface the list is the flat, in-place page of specs/078; everywhere else the screen below. */
export function UserList({ mode = "admin", studioSlug, person }: { mode?: "admin" | "owner"; studioSlug?: string; person?: string }) {
  const embedded = useEmbedded();
  if (embedded && studioSlug && mode === "admin") return <UserListStudio slug={studioSlug} person={person} />;
  return <UserListScreen mode={mode} />;
}

function UserListScreen({ mode = "admin" }: { mode?: "admin" | "owner" }) {
  const owner = mode === "owner";
  const [view, setView] = useState<"list" | "detail">("list");
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [users, setUsers] = useState<UserRow[]>([]);
  const [roles, setRoles] = useState<RoleSummary[]>([]);
  // Owner mode: ids of roles the owner may NOT give (the reserved owner role) — listed, never offered.
  const [lockedRoles, setLockedRoles] = useState<Set<string>>(new Set());
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  const [inviteUser, setInviteUser] = useState<AdminUser | null>(null);
  // Owner mode: the invite dialog just opened by "Thêm người dùng" (a failed link then says "created, no link yet").
  const [justCreated, setJustCreated] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);
  const issue = owner ? issueOrgInviteLink : createResetLink;
  // The row menu item unmounts before the invite dialog mounts, so the dialog sees <body> as its
  // opener: remember the row's ▾ button and return there on close (SLDS modals: Esc goes back to
  // whatever opened the dialog).
  const inviteFrom = useRef<HTMLElement | null>(null);

  // Load on mount; `reload` bumps the version to fetch again (the list stays on screen meanwhile).
  // State is only set after the awaits — react-hooks/set-state-in-effect.
  const [version, setVersion] = useState(0);
  const reload = () => setVersion((v) => v + 1);
  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        await ensureAuth();
        if (owner) {
          const [u, r] = await Promise.all([listOrgUsers(), listOrgRoles()]);
          if (!alive) return;
          setUsers(u.map(fromOrg));
          setRoles(r.map((x) => ({ id: x.id, name: x.label, label: x.label })));
          setLockedRoles(new Set(r.filter((x) => !x.assignable).map((x) => x.id)));
        } else {
          const [u, r] = await Promise.all([listUsers(), listRoles()]);
          if (!alive) return;
          setUsers(u);
          setRoles(r);
        }
        setErr(null);
      } catch (e) {
        if (alive) setErr(e instanceof ApiError ? e.message : t("page.adminUsers.loadError"));
      } finally {
        if (alive) setLoading(false);
      }
    })();
    return () => { alive = false; };
  }, [version, owner]);

  // Inside an app workshop (#302): the people whose role reaches this app (no role or admin stay: they
  // reach everything), and the role pickers offer only those roles — "Xem tất cả" lifts both.
  const workshop = useWorkshopScope();
  const [showAll, setShowAll] = useState(false);
  const { relevant, pending } = useScopedRoleIds(roles, workshop);
  const scoped = !!relevant && !showAll;
  const inApp = (u: AdminUser) => !scoped || u.isAdmin || !u.roleId || relevant!.has(u.roleId);
  const pickRoles = (scoped ? roles.filter((r) => relevant!.has(r.id)) : roles).filter((r) => !lockedRoles.has(r.id));
  const appUsers = pending ? [] : users.filter(inApp);
  const waiting = loading || pending;

  const filtered = appUsers.filter((u) => {
    if (!search.trim()) return true;
    const q = search.toLowerCase();
    // fullName can be absent on the wire (an account never named) — it crashed the search before.
    return (u.fullName ?? "").toLowerCase().includes(q) || u.email.toLowerCase().includes(q);
  });

  const roleLabel = useCallback((id?: string) => roles.find((r) => r.id === id)?.label ?? id ?? "—", [roles]);

  const columns = useMemo<ColumnDef<UserRow>[]>(() => [
    {
      id: "fullName",
      accessorFn: (u) => u.fullName || t("page.adminUsers.unnamed"),
      header: t("page.adminUsers.col.name"),
      cell: ({ row }) => {
        const u = row.original;
        return (
          <span className="flex min-w-0 items-center gap-2">
            <Avatar name={displayName(u)} shape="circle" size="x-small" className="shrink-0" />
            {/* .slds-button is inline-flex, where text-overflow does not apply: the label truncates
                in its own span (blueprint: the text sits in the truncating element). */}
            <button
              type="button"
              className="slds-button min-w-0"
              title={u.fullName || t("page.adminUsers.unnamed")}
              onClick={() => { setSelectedId(u.id); setView("detail"); }}
            >
              <span className="slds-truncate">{u.fullName || t("page.adminUsers.unnamed")}</span>
            </button>
          </span>
        );
      },
    },
    { id: "email", accessorKey: "email", header: t("page.adminUsers.col.email") },
    {
      // The admin flag rides with the role (Salesforce's "profile" column), not after the name: on a
      // narrow table the badge would squeeze the name to nothing.
      id: "role",
      accessorFn: (u) => roleLabel(u.roleId),
      header: t("page.adminUsers.col.role"),
      cell: ({ row, getValue }) => {
        const u = row.original;
        // The owner capability shows next to the role (specs/065); an owner-mode row the owner may not
        // change says whose it is instead of offering an edit.
        const badge = u.isAdmin
          ? <Badge variant="lightest">{t("page.adminUsers.admin")}</Badge>
          : u.canManageUsers
            ? <Badge variant="lightest">{t("settings.users.ownerBadge")}</Badge>
            : null;
        const byBuilder = owner && u.manageable === false && !u.isAdmin && !u.canManageUsers;
        if (!badge && !byBuilder) return getValue() as string;
        return (
          <span className="flex min-w-0 items-center gap-2">
            {u.roleId && <span className="slds-truncate">{getValue() as string}</span>}
            {badge}
            {byBuilder && <Badge>{t("settings.users.byBuilder")}</Badge>}
          </span>
        );
      },
    },
    {
      id: "status",
      accessorKey: "status",
      header: t("page.adminUsers.col.status"),
      cell: ({ row }) => <StatusBadge status={row.original.status} owner={owner} />,
    },
  ], [roleLabel, owner]);

  if (view === "detail" && selectedId && owner) {
    return (
      <OwnerUserDetail
        userId={selectedId}
        roles={roles}
        lockedRoles={lockedRoles}
        onBack={() => { setView("list"); reload(); }}
      />
    );
  }
  if (view === "detail" && selectedId) {
    return (
      <UserDetail
        userId={selectedId}
        roles={roles}
        onInvite={setInviteUser}
        onBack={() => { setView("list"); reload(); }}
        onDeleted={() => { setView("list"); reload(); }}
        invite={<InviteDialog user={inviteUser} issue={issue} onClose={() => setInviteUser(null)} />}
      />
    );
  }

  return (
    <div className="p-4">
      <ObjectHomePage
        icon={{ glyph: Users, name: "user" }}
        label={t(owner ? "settings.users.objectLabel" : "page.adminUsers.objectLabel")}
        title={t("page.adminUsers.title")}
        meta={waiting ? undefined : fill(t(owner ? "settings.users.meta" : scoped ? "page.adminUsers.metaApp" : "page.adminUsers.meta"), { n: appUsers.length })}
        actions={[{ label: t("page.adminUsers.add"), onSelect: () => setCreateOpen(true) }]}
        search={{ value: search, onChange: setSearch, placeholder: t("page.adminUsers.search") }}
        empty={
          err ? (
            <div className="p-4">
              <ScopedNotification theme="error">{err}</ScopedNotification>
            </div>
          ) : !waiting && filtered.length === 0 && (
            <EmptyState icon={SearchX} title={t("page.adminUsers.empty")} />
          )
        }
      >
        {/* Four columns + actions do not fit a phone: below 40rem the table scrolls sideways
            instead of clipping names and badges mid-word (same as the permission matrix). */}
        {workshop?.entityNames && relevant && (
          <div className="px-4">
            <ScopeHiddenNote hidden={users.length - appUsers.length} noun="users" showAll={showAll} onToggle={() => setShowAll((v) => !v)} />
          </div>
        )}
        <div className="overflow-x-auto">
          <div className="min-w-[40rem]">
            <DataTable
              label={t("page.adminUsers.table")}
              columns={columns}
              data={filtered}
              getRowId={(u) => u.id}
              loading={waiting}
              sortable
              rowActions={(u) => (
                <>
                  <MenuItem label={t("page.adminUsers.action.open")} onSelect={() => { setSelectedId(u.id); setView("detail"); }} />
                  {(!owner || u.manageable) && <MenuItem
                    label={t(owner ? "settings.users.action.invite" : "page.adminUsers.action.invite")}
                    icon={KeyRound}
                    onSelect={(e) => {
                      // The menu is not portaled: the item sits inside its trigger's wrapper.
                      inviteFrom.current =
                        (e.target as HTMLElement).closest(".slds-dropdown-trigger")?.querySelector<HTMLElement>("button") ?? null;
                      setJustCreated(false);
                      setInviteUser(u);
                    }}
                  />}
                </>
              )}
            />
          </div>
        </div>
      </ObjectHomePage>

      {!owner && <CreateUserModal
        open={createOpen}
        roles={pickRoles}
        onClose={() => setCreateOpen(false)}
        onCreated={() => { setCreateOpen(false); reload(); }}
      />}
      {owner && <OwnerCreateModal
        open={createOpen}
        roles={pickRoles}
        onClose={() => setCreateOpen(false)}
        onCreated={(u) => { setCreateOpen(false); reload(); inviteFrom.current = null; setJustCreated(true); setInviteUser(fromOrg(u)); }}
      />}
      <InviteDialog
        user={inviteUser}
        issue={issue}
        created={justCreated}
        onClose={() => { setInviteUser(null); setJustCreated(false); }}
        returnFocus={() => inviteFrom.current}
      />
    </div>
  );
}

// ── Invite / reset-password link (W7 §11.3) ──────────────────────────────────────
// Mints a live reset link the admin hands to the user like an invite. When SMTP is unwired the
// backend still returns a link/token (honest) — badge says "Đưa link thủ công".
type IssueLink = (userId: string) => Promise<ResetLinkResult>;

function InviteDialog({
  user, issue, created = false, onClose, returnFocus,
}: { user: AdminUser | null; issue: IssueLink; created?: boolean; onClose: () => void; returnFocus?: () => HTMLElement | null }) {
  return (
    <Modal
      open={!!user}
      onOpenChange={(o) => { if (!o) onClose(); }}
      returnFocus={returnFocus}
      title={t("page.adminUsers.invite.title")}
      tagline={user ? fill(t("page.adminUsers.invite.tagline"), { name: displayName(user) }) : undefined}
      footer={<Button onClick={onClose}>{t("page.adminUsers.close")}</Button>}
    >
      {/* Keyed: every opening mints a fresh link with fresh state (nothing to reset by hand). */}
      {user && <InviteLink key={user.id} userId={user.id} issue={issue} created={created} />}
    </Modal>
  );
}

function InviteLink({ userId, issue, created }: { userId: string; issue: IssueLink; created: boolean }) {
  const [loading, setLoading] = useState(true);
  const [result, setResult] = useState<ResetLinkResult | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  // "Gửi link mời" after a failed first try just bumps the attempt (state is set only in callbacks).
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    let alive = true;
    issue(userId)
      .then((r) => { if (alive) { setResult(r); setErr(null); } })
      .catch((e) => {
        if (!alive) return;
        if (created) setErr(t("settings.users.invite.createdNoLink"));
        else if (e instanceof ApiError && e.code === "USER_NOT_ACTIVE") setErr(t("page.adminUsers.invite.inactive"));
        else if (e instanceof ApiError && e.code === "USER_NOT_FOUND") setErr(t("page.adminUsers.invite.notFound"));
        else setErr(e instanceof ApiError ? e.message : t("page.adminUsers.invite.failed"));
      })
      .finally(() => { if (alive) setLoading(false); });
    return () => { alive = false; };
  }, [userId, issue, created, attempt]);

  const view = result ? inviteLinkView(result) : null;

  const handleCopy = async () => {
    if (!view) return;
    try {
      await navigator.clipboard.writeText(view.copyValue);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      toast.error(t("page.adminUsers.invite.copyFailed"));
    }
  };

  return loading ? (
    <div className="slds-is-relative p-6">
      <Spinner size="small" label={t("page.adminUsers.invite.loading")} />
    </div>
  ) : err ? (
    <div className="flex flex-col gap-3">
      <ScopedNotification theme="error">{err}</ScopedNotification>
      {created && (
        <div>
          <Button onClick={() => { setLoading(true); setAttempt((a) => a + 1); }}>{t("settings.users.action.invite")}</Button>
        </div>
      )}
    </div>
  ) : view ? (
    <div className="flex flex-col gap-3">
      <div className="flex items-end gap-2">
        <div className="min-w-0 flex-1">
          <Input
            label={view.usedTokenFallback ? t("page.adminUsers.invite.tokenLabel") : t("page.adminUsers.invite.linkLabel")}
            readOnly
            value={view.copyValue}
            onFocus={(e) => e.currentTarget.select()}
          />
        </div>
        <Button iconLeft={copied ? Check : Copy} onClick={handleCopy}>
          {copied ? t("page.adminUsers.invite.copied") : t("page.adminUsers.invite.copy")}
        </Button>
      </div>
      <div className="flex flex-wrap items-center gap-3">
        {view.emailBadge === "sent" ? (
          <Badge theme="success" iconLeft={MailCheck}>{t("page.adminUsers.invite.sent")}</Badge>
        ) : (
          <Badge theme="warning" iconLeft={Link2}>{t("page.adminUsers.invite.manual")}</Badge>
        )}
        <span className="slds-text-body_small slds-text-color_weak">
          {fill(t("page.adminUsers.invite.expires"), { n: result?.ttlMinutes ?? 30 })}
        </span>
      </div>
      <p className="slds-text-body_small slds-text-color_weak">{t("page.adminUsers.invite.note")}</p>
    </div>
  ) : null;
}

// ── Create User (C2) ──────────────────────────────────────────────────────────
const EMPTY_USER: CreateUserInput = { email: "", password: "", fullName: "", status: "active", isAdmin: false, roleId: "" };

function CreateUserModal({
  open, roles, onClose, onCreated,
}: { open: boolean; roles: RoleSummary[]; onClose: () => void; onCreated: () => void }) {
  const formId = useId();
  const [form, setForm] = useState<CreateUserInput>(EMPTY_USER);
  const [showPw, setShowPw] = useState(false);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const set = (k: keyof CreateUserInput, v: unknown) => setForm((f) => ({ ...f, [k]: v }));

  function close() {
    if (saving) return;
    setForm(EMPTY_USER);
    setErr(null);
    setShowPw(false);
    onClose();
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setSaving(true);
    setErr(null);
    try {
      await createUser({ ...form, roleId: form.roleId || undefined });
      toast.success(t("page.adminUsers.create.done"));
      setForm(EMPTY_USER);
      setShowPw(false);
      onCreated();
    } catch (ex) {
      setErr(ex instanceof ApiError ? ex.message : t("page.adminUsers.create.failed"));
    } finally {
      setSaving(false);
    }
  }

  return (
    <Modal
      open={open}
      onOpenChange={(o) => { if (!o) close(); }}
      title={t("page.adminUsers.create.title")}
      tagline={t("page.adminUsers.create.tagline")}
      footer={
        <>
          <Button disabled={saving} onClick={close}>{t("page.adminUsers.cancel")}</Button>
          <Button variant="brand" type="submit" form={formId} disabled={saving}>{t("page.adminUsers.create.submit")}</Button>
        </>
      }
    >
      <form id={formId} onSubmit={handleSubmit} className="flex flex-col gap-3">
        {err && <ScopedNotification theme="error">{err}</ScopedNotification>}
        <Input
          label={t("page.adminUsers.field.fullName")}
          required
          value={form.fullName}
          onChange={(e) => set("fullName", e.target.value)}
          placeholder={t("page.adminUsers.field.fullNamePlaceholder")}
        />
        <Input
          label={t("page.adminUsers.field.email")}
          required
          type="email"
          value={form.email}
          onChange={(e) => set("email", e.target.value)}
          placeholder={t("page.adminUsers.field.emailPlaceholder")}
        />
        <div className="flex items-end gap-2">
          <div className="min-w-0 flex-1">
            <Input
              label={t("page.adminUsers.field.password")}
              required
              // Native mirror of the server's shared policy (RM-17, 422 PASSWORD_TOO_WEAK);
              // the server remains the source of truth (whitespace-only / 72-byte rules).
              minLength={8}
              type={showPw ? "text" : "password"}
              value={form.password}
              onChange={(e) => set("password", e.target.value)}
              placeholder={t("page.adminUsers.field.passwordPlaceholder")}
            />
          </div>
          <Button variant="base" iconLeft={showPw ? EyeOff : Eye} onClick={() => setShowPw((s) => !s)}>
            {showPw ? t("page.auth.hidePassword") : t("page.auth.showPassword")}
          </Button>
        </div>
        <Select
          label={t("page.adminUsers.field.role")}
          value={form.roleId ?? ""}
          onChange={(e) => set("roleId", e.target.value)}
          options={roleOptions(roles)}
          placeholder={t("page.adminUsers.field.noRole")}
        />
        <Select
          label={t("page.adminUsers.field.status")}
          value={form.status}
          onChange={(e) => set("status", e.target.value as "active" | "inactive")}
          options={[
            { value: "active", label: t("page.adminUsers.status.active") },
            { value: "inactive", label: t("page.adminUsers.status.inactive") },
          ]}
          placeholder={false}
        />
        <Checkbox
          label={t("page.adminUsers.field.isAdmin")}
          checked={form.isAdmin}
          onChange={(e) => set("isAdmin", e.target.checked)}
        />
      </form>
    </Modal>
  );
}

// ── Owner: add staff (specs/065 lát C) ──────────────────────────────────────────
// Three fields: name, email, role. No password — the person sets their own through the invite link,
// which opens right after the account is created.
function OwnerCreateModal({
  open, roles, onClose, onCreated,
}: { open: boolean; roles: RoleSummary[]; onClose: () => void; onCreated: (u: OrgUser) => void }) {
  const formId = useId();
  const [form, setForm] = useState({ fullName: "", email: "", roleId: "" });
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  function close() {
    if (saving) return;
    setForm({ fullName: "", email: "", roleId: "" });
    setErr(null);
    onClose();
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setSaving(true);
    setErr(null);
    try {
      const u = await createOrgUser({ email: form.email, fullName: form.fullName, roleId: form.roleId || undefined });
      setForm({ fullName: "", email: "", roleId: "" });
      onCreated(u);
    } catch (ex) {
      if (ex instanceof ApiError && ex.code === "USER_ALREADY_EXISTS") setErr(t("settings.users.create.duplicate"));
      else setErr(ex instanceof ApiError ? ex.message : t("page.adminUsers.create.failed"));
    } finally {
      setSaving(false);
    }
  }

  return (
    <Modal
      open={open}
      onOpenChange={(o) => { if (!o) close(); }}
      title={t("page.adminUsers.create.title")}
      tagline={t("settings.users.create.tagline")}
      footer={
        <>
          <Button disabled={saving} onClick={close}>{t("page.adminUsers.cancel")}</Button>
          <Button variant="brand" type="submit" form={formId} disabled={saving}>{t("page.adminUsers.create.submit")}</Button>
        </>
      }
    >
      <form id={formId} onSubmit={handleSubmit} className="flex flex-col gap-3">
        {err && <ScopedNotification theme="error">{err}</ScopedNotification>}
        <Input
          label={t("page.adminUsers.field.fullName")}
          value={form.fullName}
          onChange={(e) => setForm((f) => ({ ...f, fullName: e.target.value }))}
          placeholder={t("page.adminUsers.field.fullNamePlaceholder")}
        />
        <Input
          label={t("page.adminUsers.field.email")}
          required
          type="email"
          value={form.email}
          onChange={(e) => setForm((f) => ({ ...f, email: e.target.value }))}
          placeholder={t("page.adminUsers.field.emailPlaceholder")}
        />
        <Select
          label={t("page.adminUsers.field.role")}
          value={form.roleId}
          onChange={(e) => setForm((f) => ({ ...f, roleId: e.target.value }))}
          options={roleOptions(roles)}
          placeholder={t("page.adminUsers.field.noRole")}
        />
      </form>
    </Modal>
  );
}

// ── User Detail/Edit (C3) ─────────────────────────────────────────────────────
function UserDetail({
  userId,
  roles,
  onInvite,
  onBack,
  onDeleted,
  invite,
}: {
  userId: string;
  roles: RoleSummary[];
  onInvite: (u: AdminUser) => void;
  onBack: () => void;
  onDeleted: () => void;
  /** The invite dialog, owned by the list so the same state serves both views. */
  invite: React.ReactNode;
}) {
  const formId = useId();
  const [user, setUser] = useState<AdminUser | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [form, setForm] = useState<UpdateUserInput>({});
  const [confirmDelete, setConfirmDelete] = useState(false);
  // specs/065: only the builder (operator) sees the switch, and never on an admin account (the two tiers exclude each other).
  const { user: me } = useAuth();
  const [switching, setSwitching] = useState(false);

  useEffect(() => {
    (async () => {
      try {
        await ensureAuth();
        const u = await getUser(userId);
        setUser(u);
        setForm({ fullName: u.fullName, status: u.status as "active" | "inactive", isAdmin: u.isAdmin, roleId: u.roleId });
      } catch (e) {
        setErr(e instanceof ApiError ? e.message : t("page.adminUsers.loadOneError"));
      } finally {
        setLoading(false);
      }
    })();
  }, [userId]);

  const set = (k: keyof UpdateUserInput, v: unknown) => setForm((f) => ({ ...f, [k]: v }));
  const loadSessions = useCallback(() => listUserSessions(userId), [userId]);
  const endSession = useCallback((sid: string) => endUserSession(userId, sid), [userId]);

  async function handleSave(e: React.FormEvent) {
    e.preventDefault();
    if (!user) return;
    // Send only what the admin changed (specs/065 FR-003). The old form resent role/isAdmin/status
    // on every save, and "Không vai" became roleId: undefined — JSON.stringify drops it, so the
    // role never cleared. "" is the API's explicit clear-to-NULL.
    const patch: UpdateUserInput = {};
    if ((form.fullName ?? "") !== (user.fullName ?? "")) patch.fullName = form.fullName ?? "";
    if (form.status !== user.status) patch.status = form.status;
    if ((form.isAdmin ?? false) !== user.isAdmin) patch.isAdmin = form.isAdmin ?? false;
    if ((form.roleId ?? "") !== (user.roleId ?? "")) patch.roleId = form.roleId ?? "";
    if (Object.keys(patch).length === 0) {
      onBack();
      return;
    }
    setSaving(true);
    setErr(null);
    try {
      await updateUser(userId, patch);
      toast.success(t("page.adminUsers.saved"));
      onBack();
    } catch (ex) {
      setErr(ex instanceof ApiError ? ex.message : t("page.adminUsers.saveFailed"));
    } finally {
      setSaving(false);
    }
  }

  async function handleOwnerSwitch(enabled: boolean) {
    setSwitching(true);
    try {
      const u = await setUserManager(userId, enabled);
      setUser((cur) => (cur ? { ...cur, canManageUsers: u.canManageUsers } : cur));
      toast.success(t(enabled ? "page.adminUsers.owner.on" : "page.adminUsers.owner.off"));
    } catch (ex) {
      toast.error(ex instanceof ApiError ? ex.message : t("page.adminUsers.owner.failed"));
    } finally {
      setSwitching(false);
    }
  }

  async function handleDelete() {
    try {
      await deleteUser(userId);
      toast.success(t("page.adminUsers.deleted"));
      onDeleted();
    } catch (ex) {
      const msg = ex instanceof ApiError ? ex.message : t("page.adminUsers.deleteFailed");
      // Anti-lock-out: 422 LAST_ADMIN or SELF_DELETE_FORBIDDEN
      if (ex instanceof ApiError && (ex.code === "LAST_ADMIN" || ex.code === "SELF_DELETE_FORBIDDEN")) {
        toast.error(msg);
      } else {
        setErr(msg);
      }
      setConfirmDelete(false);
    }
  }

  const roleLabel = roles.find((r) => r.id === user?.roleId)?.label ?? user?.roleId ?? "—";

  return (
    <div className="flex flex-col gap-3 p-4">
      <div>
        <Button variant="base" iconLeft={ArrowLeft} onClick={onBack}>{t("page.adminUsers.back")}</Button>
      </div>
      {loading ? (
        <div className="slds-is-relative p-6">
          <Spinner size="small" />
        </div>
      ) : (
        <>
          <PageHeader
            variant="record-home"
            icon={{ glyph: UserRound, name: "user" }}
            label={t("page.adminUsers.detail.objectLabel")}
            title={user ? displayName(user) : t("page.adminUsers.detail.objectLabel")}
            actions={user ? [
              <ActionGroup
                key="actions"
                maxVisible={2}
                actions={[
                  { label: t("page.adminUsers.action.invite"), onSelect: () => onInvite(user) },
                  { label: t("page.adminUsers.action.delete"), onSelect: () => setConfirmDelete(true) },
                ]}
              />,
            ] : undefined}
            details={user ? [
              { label: t("page.adminUsers.col.email"), value: user.email },
              { label: t("page.adminUsers.col.role"), value: roleLabel },
              { label: t("page.adminUsers.col.status"), value: <StatusBadge status={user.status} /> },
              {
                label: t("page.adminUsers.detail.adminCol"),
                value: user.isAdmin ? t("page.adminUsers.admin") : t("page.adminUsers.detail.notAdmin"),
              },
            ] : undefined}
          />

          <Card
            title={t("page.adminUsers.detail.section")}
            footer={user && (
              <div className="flex justify-end gap-2">
                <Button onClick={onBack}>{t("page.adminUsers.cancel")}</Button>
                <Button variant="brand" type="submit" form={formId} disabled={saving}>{t("page.adminUsers.save")}</Button>
              </div>
            )}
          >
            {err && <ScopedNotification theme="error" className="mb-3">{err}</ScopedNotification>}
            {user && (
              <form id={formId} onSubmit={handleSave} className="slds-form" role="list">
                <div className="slds-form__row grid grid-cols-1 md:grid-cols-2">
                  <div className="slds-form__item min-w-0" role="listitem">
                    <Input
                      layout="stacked"
                      label={t("page.adminUsers.field.fullName")}
                      value={form.fullName ?? ""}
                      onChange={(e) => set("fullName", e.target.value)}
                    />
                  </div>
                  <div className="slds-form__item min-w-0" role="listitem">
                    <Select
                      layout="stacked"
                      label={t("page.adminUsers.field.role")}
                      value={form.roleId ?? ""}
                      onChange={(e) => set("roleId", e.target.value)}
                      options={roleOptions(roles)}
                      placeholder={t("page.adminUsers.field.noRoleShort")}
                    />
                  </div>
                </div>
                <div className="slds-form__row grid grid-cols-1 md:grid-cols-2">
                  <div className="slds-form__item min-w-0" role="listitem">
                    <RadioButtonGroup
                      layout="stacked"
                      label={t("page.adminUsers.field.status")}
                      value={form.status}
                      onValueChange={(v) => set("status", v)}
                      options={[
                        { value: "active", label: t("page.adminUsers.status.active"), icon: UserCheck },
                        { value: "inactive", label: t("page.adminUsers.status.inactive"), icon: UserX },
                      ]}
                    />
                  </div>
                  <div className="slds-form__item min-w-0" role="listitem">
                    <Checkbox
                      layout="stacked"
                      variant="standalone"
                      label={t("page.adminUsers.field.isAdmin")}
                      checked={form.isAdmin ?? false}
                      onChange={(e) => set("isAdmin", e.target.checked)}
                    />
                  </div>
                </div>
              </form>
            )}
            {user && me?.isPlatformOperator && !user.isAdmin && (
              <div className="mt-3">
                <Checkbox
                  layout="stacked"
                  variant="standalone"
                  label={t("page.adminUsers.owner.label")}
                  checked={user.canManageUsers ?? false}
                  disabled={switching}
                  onChange={(e) => void handleOwnerSwitch(e.target.checked)}
                />
                <p className="slds-text-body_small slds-text-color_weak mt-1">{t("page.adminUsers.owner.help")}</p>
                <p className="slds-text-body_small slds-text-color_weak">{t("page.adminUsers.owner.offHelp")}</p>
              </div>
            )}
          </Card>

          {/* #313: this person's live logins, endable one by one (no "log out everywhere" — that
              is the owner's own Profile button). Same card as /profile. */}
          {user && (
            <UserSessionsCard
              load={loadSessions}
              end={endSession}
              description={t("page.adminUsers.sessions.desc")}
            />
          )}
        </>
      )}

      {invite}
      <Prompt
        open={confirmDelete}
        onOpenChange={setConfirmDelete}
        theme="error"
        title={t("page.adminUsers.delete.title")}
        footer={
          <>
            <Button onClick={() => setConfirmDelete(false)}>{t("page.adminUsers.cancel")}</Button>
            <Button variant="destructive" onClick={() => void handleDelete()}>{t("page.adminUsers.delete.confirm")}</Button>
          </>
        }
      >
        {fill(t("page.adminUsers.delete.body"), { name: user ? displayName(user) : "" })}
      </Prompt>
    </div>
  );
}

// ── Owner detail (specs/065 lát B) ───────────────────────────────────────────
// Three fields and nothing else: name, role (only roles the owner may give), active/locked. A row the owner
// may not change (admin, another owner, the builder's account, the owner themself) is read-only.
function OwnerUserDetail({
  userId, roles, lockedRoles, onBack,
}: { userId: string; roles: RoleSummary[]; lockedRoles: Set<string>; onBack: () => void }) {
  const formId = useId();
  const [user, setUser] = useState<OrgUser | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [form, setForm] = useState<{ fullName: string; status: string; roleId: string }>({ fullName: "", status: "active", roleId: "" });

  useEffect(() => {
    (async () => {
      try {
        await ensureAuth();
        const u = await getOrgUser(userId);
        setUser(u);
        setForm({ fullName: u.fullName ?? "", status: u.status, roleId: u.roleId ?? "" });
      } catch (e) {
        setErr(e instanceof ApiError ? e.message : t("page.adminUsers.loadOneError"));
      } finally {
        setLoading(false);
      }
    })();
  }, [userId]);

  const loadSessions = useCallback(() => listOrgUserSessions(userId), [userId]);
  const endSession = useCallback((sid: string) => endOrgUserSession(userId, sid), [userId]);

  // The picker offers assignable roles, plus the role the person already has (shown, not re-offered otherwise).
  const options = roles
    .filter((r) => !lockedRoles.has(r.id) || r.id === user?.roleId)
    .map((r) => ({ value: r.id, label: r.label ?? r.name }));
  const roleLabel = roles.find((r) => r.id === user?.roleId)?.label ?? "—";

  async function handleSave(e: React.FormEvent) {
    e.preventDefault();
    if (!user) return;
    // Only what changed: a bare rename must not touch the role (and so must not sign the person out).
    const patch: { fullName?: string; status?: "active" | "inactive"; roleId?: string } = {};
    if (form.fullName !== (user.fullName ?? "")) patch.fullName = form.fullName;
    if (form.status !== user.status) patch.status = form.status as "active" | "inactive";
    if (form.roleId !== (user.roleId ?? "")) patch.roleId = form.roleId;
    if (Object.keys(patch).length === 0) {
      onBack();
      return;
    }
    setSaving(true);
    setErr(null);
    try {
      await updateOrgUser(userId, patch);
      toast.success(t("page.adminUsers.saved"));
      onBack();
    } catch (ex) {
      setErr(ex instanceof ApiError ? ex.message : t("page.adminUsers.saveFailed"));
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="flex flex-col gap-3 p-4">
      <div>
        <Button variant="base" iconLeft={ArrowLeft} onClick={onBack}>{t("page.adminUsers.back")}</Button>
      </div>
      {loading ? (
        <div className="slds-is-relative p-6"><Spinner size="small" /></div>
      ) : (
        <>
          <PageHeader
            variant="record-home"
            icon={{ glyph: UserRound, name: "user" }}
            label={t("page.adminUsers.detail.objectLabel")}
            title={user ? displayName(user) : t("page.adminUsers.detail.objectLabel")}
            details={user ? [
              { label: t("page.adminUsers.col.email"), value: user.email },
              { label: t("page.adminUsers.col.role"), value: roleLabel },
              { label: t("page.adminUsers.col.status"), value: <StatusBadge status={user.status} owner /> },
            ] : undefined}
          />
          <Card
            title={t("page.adminUsers.detail.section")}
            footer={user?.manageable && (
              <div className="flex justify-end gap-2">
                <Button onClick={onBack}>{t("page.adminUsers.cancel")}</Button>
                <Button variant="brand" type="submit" form={formId} disabled={saving}>{t("page.adminUsers.save")}</Button>
              </div>
            )}
          >
            {err && <ScopedNotification theme="error" className="mb-3">{err}</ScopedNotification>}
            {user && !user.manageable && (
              <ScopedNotification theme="info">{t("settings.users.readOnly")}</ScopedNotification>
            )}
            {user?.manageable && (
              <form id={formId} onSubmit={handleSave} className="slds-form" role="list">
                <div className="slds-form__row grid grid-cols-1 md:grid-cols-2">
                  <div className="slds-form__item min-w-0" role="listitem">
                    <Input
                      layout="stacked"
                      label={t("page.adminUsers.field.fullName")}
                      value={form.fullName}
                      onChange={(e) => setForm((f) => ({ ...f, fullName: e.target.value }))}
                    />
                  </div>
                  <div className="slds-form__item min-w-0" role="listitem">
                    <Select
                      layout="stacked"
                      label={t("page.adminUsers.field.role")}
                      value={form.roleId}
                      onChange={(e) => setForm((f) => ({ ...f, roleId: e.target.value }))}
                      options={options}
                      placeholder={t("page.adminUsers.field.noRoleShort")}
                    />
                  </div>
                </div>
                <div className="slds-form__row grid grid-cols-1 md:grid-cols-2">
                  <div className="slds-form__item min-w-0" role="listitem">
                    <RadioButtonGroup
                      layout="stacked"
                      label={t("page.adminUsers.field.status")}
                      value={form.status}
                      onValueChange={(v) => setForm((f) => ({ ...f, status: v }))}
                      options={[
                        { value: "active", label: t("page.adminUsers.status.active"), icon: UserCheck },
                        { value: "inactive", label: t("settings.users.status.locked"), icon: UserX },
                      ]}
                    />
                  </div>
                </div>
              </form>
            )}
          </Card>
          {user?.manageable && (
            <UserSessionsCard
              load={loadSessions}
              end={endSession}
              description={t("page.adminUsers.sessions.desc")}
            />
          )}
        </>
      )}
    </div>
  );
}
