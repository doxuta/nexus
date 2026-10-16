"use client";

// C7/C8 — Teams List + Team Detail + Members
// Verified: GET/POST /admin/teams, GET/PUT/DELETE /admin/teams/:id,
//           GET/POST /admin/teams/:id/members, DELETE /admin/teams/:id/members/:userId
// SLDS (specs/038 T048, RESEARCH §5 /admin/teams): list = ObjectHomePage with the team hierarchy as a
// tree-grid (every level open by default — the tree is the org chart, nothing hidden) + the read-only
// team-set card; create = modals/base; detail = page-headers/record-home + cards: the team's name and
// parent (select), members (lookups to add, data-tables to list/remove); delete behind a prompt.

import { useEffect, useId, useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { ArrowLeft, UserMinus, UserRound, Users } from "lucide-react";
import {
  listTeams, getTeam, createTeam, updateTeam, deleteTeam,
  listTeamMembers, addTeamMember, removeTeamMember,
  listUsers, ensureAuth, ApiError,
  type AdminTeam, type AdminUser,
} from "@/lib/admin";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { toast } from "@/components/ui/Toaster";
import { Button } from "@/components/slds/Button";
import { Card } from "@/components/slds/Card";
import { matches, type ComboboxOption } from "@/components/slds/Combobox";
import { DataTable, fill } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { Input } from "@/components/slds/Input";
import { Lookup } from "@/components/slds/Lookup";
import { MenuItem } from "@/components/slds/Menu";
import { Modal, Prompt } from "@/components/slds/Modal";
import { PageHeader } from "@/components/slds/PageHeader";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";
import { Spinner } from "@/components/slds/Spinner";
import { TreeGrid } from "@/components/slds/TreeGrid";
import { ActionGroup } from "@/components/slds/templates/ActionGroup";
import { ObjectHomePage } from "@/components/slds/templates/ObjectHomePage";
import { TeamSetsPanel } from "@/components/screens/admin/TeamSetsPanel";

// The Studio page says "đội" (vocabulary table S13); the customer's /admin/teams keeps "nhóm". Keys not listed read the same on both.
export const STUDIO_WORDS = new Set(['objectLabel', 'title', 'meta', 'tree', 'col.name', 'root', 'rootOption', 'field.name', 'field.parentOptional', 'field.parent', 'field.parentHelp', 'back', 'detail.objectLabel', 'detail.info', 'nameSaved', 'deleteTeam', 'deleted', 'created', 'createFailed', 'loadError', 'member.remove', 'member.table']);
const txCustomer = (k: I18nKey): string => t(k);
const txStudio = (k: I18nKey): string => t(STUDIO_WORDS.has(k.slice("page.adminTeams.".length)) ? (`studio.build.access.team.x.${k.slice("page.adminTeams.".length)}` as I18nKey) : k);

type TeamNode = AdminTeam & { children: TeamNode[] };

/** Flat teams → forest by parentId. A parent missing from the list makes its child a root. */
function buildTree(teams: AdminTeam[]): TeamNode[] {
  const ids = new Set(teams.map((x) => x.id));
  const byParent = new Map<string, AdminTeam[]>();
  for (const x of teams) {
    const p = x.parentId && ids.has(x.parentId) ? x.parentId : "";
    byParent.set(p, [...(byParent.get(p) ?? []), x]);
  }
  const seen = new Set<string>();
  const grow = (x: AdminTeam): TeamNode => {
    seen.add(x.id);
    return { ...x, children: (byParent.get(x.id) ?? []).filter((c) => !seen.has(c.id)).map(grow) };
  };
  const roots = (byParent.get("") ?? []).map(grow);
  // ponytail: the server refuses a parent loop (422 TEAM_PARENT_CYCLE); should one exist anyway, its
  // teams would have no root to hang from — list them as roots rather than hide them.
  for (const x of teams) if (!seen.has(x.id)) roots.push(grow(x));
  return roots;
}

const userOption = (u: AdminUser): ComboboxOption => ({
  value: u.id,
  label: u.fullName || u.email,
  meta: u.email,
  icon: { glyph: UserRound, name: "user", label: t("page.adminTeams.userObject") },
});

/**
 * `inline` = the Studio page (specs/074 f2-access-team-share): "+ Đội mới" opens a form in the page and deleting a
 * team or removing a member asks in the page, no dialog. Without it (the customer's /admin/teams) the Modal/Prompt stay.
 */
export function TeamsList({ inline = false }: { inline?: boolean } = {}) {
  const tx = inline ? txStudio : txCustomer;
  const [view, setView] = useState<"list" | "detail">("list");
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [teams, setTeams] = useState<AdminTeam[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);
  const [createOpen, setCreateOpen] = useState(false);

  // Load on mount; `reload` bumps the version to fetch again (the tree stays on screen meanwhile).
  // State is only set after the awaits — react-hooks/set-state-in-effect.
  const [version, setVersion] = useState(0);
  const reload = () => setVersion((v) => v + 1);
  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        await ensureAuth();
        const list = await listTeams();
        if (!alive) return;
        setTeams(list);
        setErr(null);
      } catch (e) {
        if (alive) setErr(e instanceof ApiError ? e.message : tx("page.adminTeams.loadError"));
      } finally {
        if (alive) setLoading(false);
      }
    })();
    return () => { alive = false; };
  }, [version, tx]);

  const tree = useMemo(() => buildTree(teams), [teams]);
  const columns = useMemo<ColumnDef<TeamNode>[]>(() => [
    {
      id: "name",
      accessorKey: "name",
      header: tx("page.adminTeams.col.name"),
      // .slds-button is inline-flex, where text-overflow does not apply: capped at the cell width,
      // the name truncates in its own span.
      cell: ({ row }) => (
        <button
          type="button"
          className="slds-button max-w-full"
          onClick={() => { setSelectedId(row.original.id); setView("detail"); }}
        >
          <span className="slds-truncate">{row.original.name}</span>
        </button>
      ),
    },
    {
      id: "parent",
      accessorFn: (n) => teams.find((p) => p.id === n.parentId)?.name ?? "",
      header: tx("page.adminTeams.col.parent"),
      cell: ({ getValue }) => (getValue() as string) || <span className="slds-text-color_weak">{tx("page.adminTeams.root")}</span>,
    },
  ], [teams, tx]);

  if (view === "detail" && selectedId) {
    return (
      <TeamDetail
        teamId={selectedId}
        teams={teams}
        inline={inline}
        onBack={() => { setView("list"); reload(); }}
        onDeleted={() => { setView("list"); reload(); }}
      />
    );
  }

  const createForm = (
    <CreateTeamModal
      open={createOpen}
      inline={inline}
      teams={teams}
      onClose={() => setCreateOpen(false)}
      onCreated={() => { setCreateOpen(false); reload(); }}
    />
  );
  const studioEmpty = inline && !loading && !err && teams.length === 0;

  return (
    // No gap: SLDS itself spaces the team-set card under the list card (.slds-card + .slds-card).
    <div className="p-4">
      {studioEmpty ? (
        <>
          <EmptyState
            plain
            title={t("studio.build.access.team.empty.title")}
            description={t("studio.build.access.team.empty.desc")}
            action={createOpen ? undefined : <Button variant="brand" onClick={() => setCreateOpen(true)}>{t("studio.build.access.team.first")}</Button>}
          />
          {createForm}
        </>
      ) : (
      <>
      {inline && createForm}
      <ObjectHomePage
        icon={{ glyph: Users, name: "groups" }}
        label={tx("page.adminTeams.objectLabel")}
        title={tx("page.adminTeams.title")}
        meta={loading ? undefined : fill(tx("page.adminTeams.meta"), { n: teams.length })}
        actions={[{ label: inline ? t("studio.build.access.team.new") : tx("page.adminTeams.create"), onSelect: () => setCreateOpen(true) }]}
        empty={
          err ? (
            <div className="p-4">
              <ScopedNotification theme="error">{err}</ScopedNotification>
            </div>
          ) : !loading && teams.length === 0 && (
            <EmptyState
              size="large"
              icon={Users}
              title={tx("page.adminTeams.empty.title")}
              description={tx("page.adminTeams.empty.desc")}
            />
          )
        }
      >
        <TreeGrid
          label={tx("page.adminTeams.tree")}
          columns={columns}
          data={tree}
          getSubRows={(n) => (n.children.length ? n.children : undefined)}
          getRowId={(n) => n.id}
          defaultExpanded={true}
          loading={loading}
        />
      </ObjectHomePage>
      </>
      )}

      {/* Team sets (engine-derived, read-only). Not on the Studio page: S13 has no "bộ nhóm" word there. */}
      {!inline && <TeamSetsPanel />}

      {!inline && createForm}
    </div>
  );
}

// ── Create Team ───────────────────────────────────────────────────────────────
function CreateTeamModal({
  open, inline, teams, onClose, onCreated,
}: { open: boolean; inline: boolean; teams: AdminTeam[]; onClose: () => void; onCreated: () => void }) {
  const tx = inline ? txStudio : txCustomer;
  const formId = useId();
  const [name, setName] = useState("");
  const [parentId, setParentId] = useState("");
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  function close() {
    if (saving) return;
    setName("");
    setParentId("");
    setErr(null);
    onClose();
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setSaving(true);
    setErr(null);
    try {
      await createTeam(name.trim(), parentId || undefined);
      toast.success(tx("page.adminTeams.created"));
      setName("");
      setParentId("");
      onCreated();
    } catch (ex) {
      setErr(ex instanceof ApiError ? ex.message : tx("page.adminTeams.createFailed"));
    } finally {
      setSaving(false);
    }
  }

  const fields = (
    <>
      {err && <ScopedNotification theme="error">{err}</ScopedNotification>}
      <Input
        label={tx("page.adminTeams.field.name")}
        required
        autoFocus={inline}
        value={name}
        onChange={(e) => setName(e.target.value)}
        placeholder={tx("page.adminTeams.field.namePlaceholder")}
      />
      <Select
        label={tx("page.adminTeams.field.parentOptional")}
        value={parentId}
        onChange={(e) => setParentId(e.target.value)}
        options={teams.map((x) => ({ value: x.id, label: x.name }))}
        placeholder={tx("page.adminTeams.rootOption")}
      />
    </>
  );

  if (inline) {
    if (!open) return null;
    return (
      <form
        id={formId}
        aria-label={t("studio.build.access.team.new")}
        className="slds-theme_shade slds-p-around_medium slds-m-bottom_small"
        onKeyDown={(e) => { if (e.key === "Escape") { e.stopPropagation(); close(); } }}
        onSubmit={handleSubmit}
      >
        <div className="flex max-w-sm flex-col gap-3">{fields}</div>
        <div className="slds-m-top_small flex gap-2">
          <Button variant="brand" type="submit" disabled={saving || !name.trim()}>{t("studio.build.access.team.submit")}</Button>
          <Button disabled={saving} onClick={close}>{tx("page.adminTeams.cancel")}</Button>
        </div>
      </form>
    );
  }

  return (
    <Modal
      open={open}
      onOpenChange={(o) => { if (!o) close(); }}
      title={tx("page.adminTeams.createTitle")}
      size="small"
      footer={
        <>
          <Button disabled={saving} onClick={close}>{tx("page.adminTeams.cancel")}</Button>
          <Button variant="brand" type="submit" form={formId} disabled={saving}>{tx("page.adminTeams.create")}</Button>
        </>
      }
    >
      <form id={formId} onSubmit={handleSubmit} className="flex flex-col gap-3">
        {fields}
      </form>
    </Modal>
  );
}

// ── Team Detail + Members (C8) ────────────────────────────────────────────────
function TeamDetail({ teamId, teams, inline, onBack, onDeleted }: { teamId: string; teams: AdminTeam[]; inline: boolean; onBack: () => void; onDeleted: () => void }) {
  const tx = inline ? txStudio : txCustomer;
  const [team, setTeam] = useState<AdminTeam | null>(null);
  const [members, setMembers] = useState<AdminUser[]>([]);
  const [allUsers, setAllUsers] = useState<AdminUser[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);
  const [editName, setEditName] = useState("");
  const [saving, setSaving] = useState(false);
  // Parent ("cấp trên") editor: "" = root. The server refuses a parent that would loop the tree
  // (422 TEAM_PARENT_CYCLE) — its message is shown inline, not only as a toast.
  const [parentId, setParentId] = useState("");
  const [parentErr, setParentErr] = useState<string | null>(null);
  const [savingParent, setSavingParent] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [removing, setRemoving] = useState<AdminUser | null>(null);
  const [confirmBusy, setConfirmBusy] = useState(false); // one DELETE per confirm click
  const [addUser, setAddUser] = useState<ComboboxOption | null>(null);

  // Only the first load shows the spinner: a reload after a save keeps the page (and the lookup) in
  // place. State is only set after the awaits — react-hooks/set-state-in-effect.
  const [version, setVersion] = useState(0);
  const reload = () => setVersion((v) => v + 1);
  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        await ensureAuth();
        const [tm, m, u] = await Promise.all([getTeam(teamId), listTeamMembers(teamId), listUsers()]);
        if (!alive) return;
        setTeam(tm);
        setEditName(tm.name);
        setParentId(tm.parentId ?? "");
        setParentErr(null);
        setMembers(m);
        setAllUsers(u);
      } catch (e) {
        if (alive) setErr(e instanceof ApiError ? e.message : tx("page.adminTeams.loadError"));
      } finally {
        if (alive) setLoading(false);
      }
    })();
    return () => { alive = false; };
  }, [teamId, version, tx]);

  async function handleSaveName(e: React.FormEvent) {
    e.preventDefault();
    if (!editName.trim()) return;
    setSaving(true);
    try {
      await updateTeam(teamId, { name: editName.trim() });
      toast.success(tx("page.adminTeams.nameSaved"));
      reload();
    } catch (ex) {
      toast.error(ex instanceof ApiError ? ex.message : tx("page.adminTeams.saveFailed"));
    } finally {
      setSaving(false);
    }
  }

  async function handleSaveParent(e: React.FormEvent) {
    e.preventDefault();
    setSavingParent(true);
    setParentErr(null);
    try {
      await updateTeam(teamId, { parentId });
      toast.success(parentId ? tx("page.adminTeams.parentSet") : tx("page.adminTeams.parentCleared"));
      reload();
    } catch (ex) {
      const msg = ex instanceof ApiError ? ex.message : tx("page.adminTeams.parentFailed");
      setParentErr(msg);
      toast.error(msg);
    } finally {
      setSavingParent(false);
    }
  }

  async function handleDeleteTeam() {
    setConfirmBusy(true);
    try {
      await deleteTeam(teamId);
      toast.success(tx("page.adminTeams.deleted"));
      onDeleted();
    } catch (ex) {
      toast.error(ex instanceof ApiError ? ex.message : tx("page.adminTeams.deleteFailed"));
      setConfirmDelete(false);
      setConfirmBusy(false);
    }
  }

  async function handleAddMember() {
    if (!addUser) return;
    try {
      await addTeamMember(teamId, addUser.value);
      toast.success(tx("page.adminTeams.memberAdded"));
      setAddUser(null);
      reload();
    } catch (ex) {
      toast.error(ex instanceof ApiError ? ex.message : tx("page.adminTeams.memberAddFailed"));
    }
  }

  async function handleRemoveMember(userId: string) {
    setConfirmBusy(true);
    try {
      await removeTeamMember(teamId, userId);
      toast.success(tx("page.adminTeams.memberRemoved"));
      setRemoving(null);
      reload();
    } catch (ex) {
      toast.error(ex instanceof ApiError ? ex.message : tx("page.adminTeams.memberRemoveFailed"));
    } finally {
      setConfirmBusy(false);
    }
  }

  const memberIds = new Set(members.map((m) => m.id));
  const availableUsers = allUsers.filter((u) => !memberIds.has(u.id));
  const searchUsers = async (term: string) =>
    availableUsers.filter((u) => matches(u.fullName || "", term) || matches(u.email, term)).map(userOption);
  const parentName = teams.find((x) => x.id === team?.parentId)?.name;

  const memberColumns = useMemo<ColumnDef<AdminUser>[]>(() => [
    { id: "name", accessorFn: (m) => m.fullName || m.email, header: tx("page.adminTeams.member.name") },
    { id: "email", accessorKey: "email", header: tx("page.adminTeams.member.email") },
  ], [tx]);

  return (
    <div className="flex flex-col gap-3 p-4">
      <div>
        <Button variant="base" iconLeft={ArrowLeft} onClick={onBack}>{tx("page.adminTeams.back")}</Button>
      </div>
      {loading ? (
        <div className="slds-is-relative p-6">
          <Spinner size="small" />
        </div>
      ) : (
        <>
          <PageHeader
            variant="record-home"
            icon={{ glyph: Users, name: "groups" }}
            label={tx("page.adminTeams.detail.objectLabel")}
            title={team?.name ?? ""}
            actions={team ? [
              <ActionGroup
                key="actions"
                maxVisible={1}
                actions={[{ label: tx("page.adminTeams.deleteTeam"), onSelect: () => setConfirmDelete(true) }]}
              />,
            ] : undefined}
            details={team ? [
              { label: tx("page.adminTeams.col.parent"), value: parentName ?? tx("page.adminTeams.root") },
              { label: tx("page.adminTeams.detail.members"), value: String(members.length) },
            ] : undefined}
          />
          {err && <ScopedNotification theme="error">{err}</ScopedNotification>}
          {inline && confirmDelete && (
            <InlineConfirm
              tone="error"
              message={tf("studio.build.access.team.delete.title", { name: team?.name ?? "" })}
              confirmLabel={t("studio.build.access.team.delete.yes")}
              busy={confirmBusy}
              onConfirm={() => void handleDeleteTeam()}
              onCancel={() => setConfirmDelete(false)}
            >
              {t("studio.build.access.team.delete.body")}
            </InlineConfirm>
          )}
          {inline && removing && (
            <InlineConfirm
              tone="warning"
              message={tf("studio.build.access.team.member.removeTitle", { name: removing.fullName || removing.email, team: team?.name ?? "" })}
              confirmLabel={t("studio.build.access.team.member.removeYes")}
              busy={confirmBusy}
              onConfirm={() => void handleRemoveMember(removing.id)}
              onCancel={() => setRemoving(null)}
            />
          )}

          {/* Each card in its own cell: SLDS spaces adjacent cards (.slds-card + .slds-card), which
              would push the second column down. */}
          <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
            <div>
              <Card title={tx("page.adminTeams.detail.info")}>
                <div className="flex flex-col gap-4">
                  {/* Name */}
                  <form onSubmit={handleSaveName} className="flex items-end gap-2">
                    <div className="min-w-0 flex-1">
                      <Input
                        label={tx("page.adminTeams.field.name")}
                        value={editName}
                        onChange={(e) => setEditName(e.target.value)}
                      />
                    </div>
                    <Button type="submit" disabled={saving || editName === team?.name}>{tx("page.adminTeams.save")}</Button>
                  </form>
  
                  {/* Parent team (cấp trên) — a member of the parent sees every record of the teams below it.
                      items-start + mt-6 (the label's height): the help / error line under the select must
                      not push the button down. */}
                  <form onSubmit={handleSaveParent} className="flex items-start gap-2">
                    <div className="min-w-0 flex-1">
                      <Select
                        label={tx("page.adminTeams.field.parent")}
                        value={parentId}
                        onChange={(e) => { setParentId(e.target.value); setParentErr(null); }}
                        options={teams.filter((x) => x.id !== teamId).map((x) => ({ value: x.id, label: x.name }))}
                        placeholder={tx("page.adminTeams.rootOption")}
                        help={tx("page.adminTeams.field.parentHelp")}
                        error={parentErr ? <span role="alert">{parentErr}</span> : undefined}
                      />
                    </div>
                    <Button type="submit" className="mt-6" disabled={savingParent || parentId === (team?.parentId ?? "")}>
                      {tx("page.adminTeams.save")}
                    </Button>
                  </form>
                </div>
              </Card>
            </div>

            <div>
              <Card title={tx("page.adminTeams.detail.members")} count={members.length} padded={false}>
                <div className="flex items-end gap-2 px-4 pb-3">
                  <Lookup
                    className="min-w-0 flex-1"
                    label={tx("page.adminTeams.member.add")}
                    placeholder={tx("page.adminTeams.member.search")}
                    search={searchUsers}
                    value={addUser}
                    onChange={setAddUser}
                  />
                  <Button disabled={!addUser} onClick={() => void handleAddMember()}>{tx("page.adminTeams.member.addButton")}</Button>
                </div>
                <DataTable
                  label={tx("page.adminTeams.member.table")}
                  columns={memberColumns}
                  data={members}
                  getRowId={(m) => m.id}
                  empty={<p className="p-4 slds-text-align_center slds-text-color_weak">{tx("page.adminTeams.member.empty")}</p>}
                  rowActions={(m) => (
                    <MenuItem label={tx("page.adminTeams.member.remove")} icon={UserMinus} onSelect={() => (inline ? setRemoving(m) : void handleRemoveMember(m.id))} />
                  )}
                />
              </Card>
            </div>
          </div>
        </>
      )}

      {!inline && <Prompt
        open={confirmDelete}
        onOpenChange={setConfirmDelete}
        theme="error"
        title={fill(tx("page.adminTeams.delete.title"), { name: team?.name ?? "" })}
        footer={
          <>
            <Button onClick={() => setConfirmDelete(false)}>{tx("page.adminTeams.cancel")}</Button>
            <Button variant="destructive" onClick={() => void handleDeleteTeam()}>{tx("page.adminTeams.delete.confirm")}</Button>
          </>
        }
      >
        {tx("page.adminTeams.delete.body")}
      </Prompt>}
    </div>
  );
}
