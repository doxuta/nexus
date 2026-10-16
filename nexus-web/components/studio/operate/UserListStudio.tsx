"use client";

// Vận hành › Người dùng in the Studio work surface (specs/078 lát u7-nguoi-dung-danh-sach, FR-020..FR-025).
// "List with an action at the end of each row": lede + action bar, a flat table, and every follow-up (create form,
// lock confirm, reset link, one-time password) opens IN the page — never a dialog. Secrets come from a plain `await`
// in a handler and go straight into component state: never `useMutation` (its variables/data sit in the mutation cache).
import Link from "next/link";
import { useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { Button } from "@/components/slds/Button";
import { DataTable, fill } from "@/components/slds/DataTable";
import { Input } from "@/components/slds/Input";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";
import { Checkbox } from "@/components/slds/Checkbox";
import { toast } from "@/components/ui/Toaster";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { QueryState } from "@/components/studio/QueryState";
import { SecretOnceBlock, type SecretRow } from "@/components/studio/SecretOnceBlock";
import { StatusText } from "@/components/studio/StatusText";
import { useProjectQuery } from "@/components/studio/useProjectQuery";
import { ApiError, createResetLink, createUser, listRoles, listUsers, setUserManager, updateUser, type AdminUser, type CreateUserInput } from "@/lib/admin";
import { t } from "@/lib/i18n";
import { inviteLinkView } from "@/lib/invite";
import { describeUserTier, generatePassword, PASSWORD_MIN_LENGTH, userErrorSentence as sentence, userLede, userRowActions } from "@/lib/userAdmin";
import { ID_RE, studioHref } from "@/lib/studioRoutes";
import { UserDetailStudio } from "./UserDetailStudio";

const FIRST_PAGE = 8;
const displayName = (u: Pick<AdminUser, "fullName" | "email">) => u.fullName || u.email;

interface RowSecret {
  userId: string;
  name: string;
  rows: SecretRow[];
  ttl: number;
  emailQueued: boolean;
}
const EMPTY_FORM: CreateUserInput = { email: "", password: "", fullName: "", roleId: "", isAdmin: false };

/** `?nguoi=<id>` opens one person; anything that is not a plain id is dropped here too (FR-133), not only in the router. */
export function UserListStudio({ slug, person }: { slug: string; person?: string }) {
  if (person !== undefined && ID_RE.test(person)) return <UserDetailStudio slug={slug} id={person} />;
  return <UserListPage slug={slug} />;
}

function UserListPage({ slug }: { slug: string }) {
  const qc = useQueryClient();
  const users = useProjectQuery(slug, ["operate", "users"], listUsers);
  const roles = useProjectQuery(slug, ["operate", "roles"], listRoles);
  const [q, setQ] = useState("");
  const [all, setAll] = useState(false);
  const [formOpen, setFormOpen] = useState(false);
  const [lockFor, setLockFor] = useState<string | null>(null);
  const [ownerFor, setOwnerFor] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [rowErr, setRowErr] = useState<Record<string, string>>({});
  const [reset, setReset] = useState<RowSecret | null>(null);
  const [created, setCreated] = useState<AdminUser | null>(null);
  const [newSecret, setNewSecret] = useState<SecretRow[] | null>(null);
  const lockBtn = useRef<Record<string, HTMLElement | null>>({});
  const ownerBtn = useRef<Record<string, HTMLElement | null>>({});

  const list = useMemo(() => users.data ?? [], [users.data]);
  const roleLabel = (id?: string) => (id ? roles.data?.find((r) => r.id === id)?.label ?? "—" : t("studio.operate.users.noRole"));
  const refresh = () => qc.invalidateQueries({ queryKey: ["project", slug, "operate", "users"] });
  const setErr = (id: string, msg: string | null) =>
    setRowErr((m) => {
      const next = { ...m };
      if (msg) next[id] = msg;
      else delete next[id];
      return next;
    });

  const needle = q.trim().toLowerCase();
  const matches = needle ? list.filter((u) => (u.fullName ?? "").toLowerCase().includes(needle) || u.email.toLowerCase().includes(needle)) : list;
  const shown = needle || all || matches.length <= FIRST_PAGE ? matches : matches.slice(0, FIRST_PAGE);
  const showTier = list.some((u) => u.canManageUsers !== undefined);
  const lede = userLede(list);

  async function doReset(u: AdminUser) {
    setErr(u.id, null);
    setBusy(u.id);
    try {
      const r = await createResetLink(u.id); // plain await: the token must not enter a mutation cache
      const v = inviteLinkView(r);
      setReset({
        userId: u.id,
        name: displayName(u),
        rows: [{ label: t(v.usedTokenFallback ? "studio.operate.users.reset.tokenLabel" : "studio.operate.users.reset.linkLabel"), value: v.copyValue }],
        ttl: r.ttlMinutes,
        emailQueued: r.emailQueued,
      });
    } catch (e) {
      setErr(u.id, sentence(e));
      if (e instanceof ApiError && e.code === "USER_NOT_FOUND") void refresh();
    } finally {
      setBusy(null);
    }
  }

  async function setStatus(u: AdminUser, next: "active" | "inactive") {
    setErr(u.id, null);
    setBusy(u.id);
    try {
      await updateUser(u.id, { status: next });
      setLockFor(null);
      toast.success(fill(t(next === "active" ? "studio.operate.users.unlocked" : "studio.operate.users.locked"), { name: displayName(u) }));
      await refresh();
    } catch (e) {
      setErr(u.id, sentence(e));
      if (e instanceof ApiError && e.code === "USER_NOT_FOUND") {
        setLockFor(null);
        void refresh();
      }
    } finally {
      setBusy(null);
    }
  }

  async function setOwner(u: AdminUser, enabled: boolean) {
    setErr(u.id, null);
    setBusy(u.id);
    try {
      await setUserManager(u.id, enabled);
      setOwnerFor(null);
      toast.success(fill(t(enabled ? "studio.operate.tier.setOn" : "studio.operate.tier.setOff"), { name: displayName(u) }));
      await refresh();
    } catch (e) {
      setErr(u.id, sentence(e));
      if (e instanceof ApiError && e.code === "USER_NOT_FOUND") {
        setOwnerFor(null);
        void refresh();
      }
    } finally {
      setBusy(null);
    }
  }

  const columns = useMemo<ColumnDef<AdminUser>[]>(() => {
    const cols: ColumnDef<AdminUser>[] = [
      {
        id: "person",
        header: t("studio.operate.users.col.person"),
        accessorFn: (u) => displayName(u),
        cell: ({ row: { original: u } }) => (
          <span className="block min-w-0">
            <strong className="slds-truncate block">
              <Link href={studioHref(slug, "van-hanh/nguoi-dung", {}, { nguoi: u.id })}>{u.fullName || t("studio.operate.users.unnamed")}</Link>
            </strong>
            <span className="slds-text-body_small slds-text-color_weak slds-truncate block">{u.email}</span>
          </span>
        ),
      },
      { id: "role", header: t("studio.operate.users.col.role"), accessorFn: (u) => roleLabel(u.roleId) },
    ];
    if (showTier) {
      cols.push({
        id: "tier",
        header: t("studio.operate.users.col.tier"),
        accessorFn: (u) => describeUserTier(u) ?? "",
        cell: ({ row: { original: u } }) => {
          const tier = describeUserTier(u);
          return tier ? <StatusText tone="weak">{t(tier === "owner" ? "studio.operate.users.tier.owner" : "studio.operate.users.tier.admin")}</StatusText> : null;
        },
      });
    }
    cols.push(
      {
        id: "status",
        header: t("studio.operate.users.col.status"),
        accessorFn: (u) => u.status,
        cell: ({ row: { original: u } }) =>
          u.status === "active" ? (
            <StatusText tone="success">{t("studio.operate.users.status.active")}</StatusText>
          ) : (
            <StatusText tone="error">{t("studio.operate.users.status.locked")}</StatusText>
          ),
      },
      {
        id: "actions",
        header: () => <span className="slds-assistive-text">{t("studio.operate.users.col.actions")}</span>,
        enableSorting: false,
        cell: ({ row: { original: u } }) => {
          const name = displayName(u);
          const act = userRowActions(u);
          return (
            <span className="slds-grid slds-wrap slds-grid_align-end">
              <Button
                variant="base"
                className="whitespace-nowrap"
                disabled={busy === u.id}
                aria-label={fill(t("studio.operate.users.row.resetOf"), { name })}
                onClick={() => void doReset(u)}
              >
                {t("studio.operate.users.row.reset")}
              </Button>
              <Button
                variant="base"
                className="slds-m-left_small whitespace-nowrap"
                disabled={busy === u.id}
                aria-label={fill(t(act.lock === "lock" ? "studio.operate.users.row.lockOf" : "studio.operate.users.row.unlockOf"), { name })}
                ref={(el) => { lockBtn.current[u.id] = el; }}
                onClick={() => (act.lock === "lock" ? setLockFor(u.id) : void setStatus(u, "active"))}
              >
                {t(act.lock === "lock" ? "studio.operate.users.row.lock" : "studio.operate.users.row.unlock")}
              </Button>
              {showTier && !u.isAdmin && (
                <Button
                  variant="base"
                  className="slds-m-left_small whitespace-nowrap"
                  disabled={busy === u.id}
                  ref={(el) => { ownerBtn.current[u.id] = el; }}
                  aria-label={fill(t(u.canManageUsers ? "studio.operate.tier.offOf" : "studio.operate.tier.onOf"), { name })}
                  onClick={() => (u.canManageUsers ? void setOwner(u, false) : setOwnerFor(u.id))}
                >
                  {t(u.canManageUsers ? "studio.operate.tier.off" : "studio.operate.tier.rowOn")}
                </Button>
              )}
            </span>
          );
        },
      },
    );
    return cols;
    // doReset/setStatus only close over stable setters; the column set changes with the data it labels.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [showTier, roles.data, busy, slug]);

  const resetBlock = (r: RowSecret) => (
    <SecretOnceBlock key="reset" rows={r.rows} onDone={() => setReset(null)}>
      <p>{fill(t("studio.operate.users.reset.expires"), { n: r.ttl })}</p>
      <p>{fill(t(r.emailQueued ? "studio.operate.users.reset.sent" : "studio.operate.users.reset.manual"), { name: r.name })}</p>
      <p><strong>{fill(t("studio.operate.users.reset.warning"), { name: r.name })}</strong></p>
    </SecretOnceBlock>
  );
  // The person may not be a drawn row (beyond the first 8, filtered out by the search, just created): the secret must
  // still show, so it falls back to a block under the table instead of vanishing with a code nobody can read again.
  const resetOutside = reset && !shown.some((x) => x.id === reset.userId) ? reset : null;

  const rowDetail = (u: AdminUser) => {
    const name = displayName(u);
    const parts = [];
    if (rowErr[u.id]) parts.push(<ScopedNotification key="e" theme="error">{rowErr[u.id]}</ScopedNotification>);
    if (lockFor === u.id) {
      parts.push(
        <InlineConfirm
          key="lock"
          tone="warning"
          message={fill(t("studio.operate.users.lockConfirm"), { name })}
          confirmLabel={t("studio.operate.users.row.lock")}
          busy={busy === u.id}
          onConfirm={() => void setStatus(u, "inactive")}
          onCancel={() => {
            setLockFor(null);
            queueMicrotask(() => lockBtn.current[u.id]?.focus());
          }}
        />,
      );
    }
    if (ownerFor === u.id) {
      parts.push(
        <InlineConfirm
          key="owner"
          tone="warning"
          message={fill(t("studio.operate.tier.confirm"), { name })}
          confirmLabel={t("studio.operate.tier.on")}
          busy={busy === u.id}
          onConfirm={() => void setOwner(u, true)}
          onCancel={() => {
            setOwnerFor(null);
            queueMicrotask(() => ownerBtn.current[u.id]?.focus());
          }}
        />,
      );
    }
    if (reset?.userId === u.id) parts.push(resetBlock(reset));
    return parts.length ? <div className="flex flex-col gap-2 py-2">{parts}</div> : null;
  };

  return (
    <div className="slds-p-around_medium">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="nx-lede">
          {users.data && (
            <>
              <b className="tabular-nums">{fill(t("studio.operate.users.lede.people"), { n: lede.n })}</b>
              {fill(t("studio.operate.users.lede.rest"), { m: lede.m, k: lede.k })}
            </>
          )}
        </p>
        <div className="flex items-center gap-3">
          {list.length > FIRST_PAGE && (
            <Input type="search" label={t("studio.operate.users.search")} labelHidden placeholder={t("studio.operate.users.search")} value={q} onChange={(e) => setQ(e.target.value)} />
          )}
          <Button variant="brand" onClick={() => { setFormOpen(true); setCreated(null); setNewSecret(null); }}>
            {t("studio.operate.users.new")}
          </Button>
        </div>
      </div>

      {(formOpen || created) && (
        <div className="slds-m-top_medium">
          {formOpen ? (
            <NewUserForm
              roles={roles.data ?? []}
              onCancel={() => setFormOpen(false)}
              onCreated={(u, secret) => {
                setFormOpen(false);
                setCreated(u);
                setNewSecret(secret);
                void refresh();
              }}
            />
          ) : created && (
            <div className="flex flex-col gap-2">
              <p>{fill(t("studio.operate.users.created"), { name: displayName(created) })}</p>
              {newSecret && (
                <SecretOnceBlock rows={newSecret} hint={t("studio.operate.users.newSecretHint")} onDone={() => setNewSecret(null)} />
              )}
              <div>
                <Button
                  variant="base"
                  onClick={() => void doReset(created)}
                >
                  {fill(t("studio.operate.users.createdLink"), { name: displayName(created) })}
                </Button>
              </div>
            </div>
          )}
        </div>
      )}

      {resetOutside && <div className="slds-m-top_medium">{resetBlock(resetOutside)}</div>}

      <div className="slds-m-top_medium">
        {users.isError ? (
          <div className="slds-p-around_medium">
            <ScopedNotification theme="error">{t("studio.operate.users.loadError")}</ScopedNotification>
            <div className="slds-m-top_small">
              <Button variant="neutral" onClick={() => void users.refetch()}>{t("studio.query.retry")}</Button>
            </div>
          </div>
        ) : (
        <QueryState
          query={users}
          empty={<p className="slds-p-around_medium slds-text-color_weak">{t("studio.operate.users.empty")}</p>}
        >
          {matches.length === 0 ? (
            <p className="slds-p-around_medium slds-text-color_weak">{fill(t("studio.operate.users.noMatch"), { q: q.trim() })}</p>
          ) : (
            <>
              <DataTable
                label={t("studio.operate.users.table")}
                columns={columns}
                data={shown}
                getRowId={(u) => u.id}
                stacked
                rowClassName={(u) => (lockFor === u.id || ownerFor === u.id || reset?.userId === u.id ? "slds-is-selected" : undefined)}
                rowDetail={rowDetail}
              />
              {!needle && matches.length > FIRST_PAGE && (
                <p className="slds-m-top_small">
                  <Button variant="base" onClick={() => setAll((v) => !v)}>
                    {all ? t("studio.operate.users.collapse") : fill(t("studio.operate.users.showAll"), { n: matches.length })}
                  </Button>
                </p>
              )}
            </>
          )}
        </QueryState>
        )}
      </div>
    </div>
  );
}

function NewUserForm({ roles, onCancel, onCreated }: {
  roles: { id: string; name: string; label?: string }[];
  onCancel: () => void;
  onCreated: (u: AdminUser, generated: SecretRow[] | null) => void;
}) {
  const first = useRef<HTMLInputElement>(null);
  const [form, setForm] = useState<CreateUserInput>(EMPTY_FORM);
  const [generated, setGenerated] = useState(false);
  const [show, setShow] = useState(false);
  const [saving, setSaving] = useState(false);
  const [emailErr, setEmailErr] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  useEffect(() => first.current?.focus(), []);
  const set = <K extends keyof CreateUserInput>(k: K, v: CreateUserInput[K]) => setForm((f) => ({ ...f, [k]: v }));

  async function submit(e: FormEvent) {
    e.preventDefault();
    setSaving(true);
    setEmailErr(null);
    setErr(null);
    try {
      const u = await createUser({ ...form, roleId: form.roleId || undefined, status: "active" }); // plain await: the password is in the arguments
      onCreated(u, generated ? [{ label: t("studio.operate.users.form.email"), value: form.email }, { label: t("studio.operate.users.form.password"), value: form.password }] : null);
    } catch (ex) {
      if (ex instanceof ApiError && ex.code === "USER_ALREADY_EXISTS") setEmailErr(sentence(ex));
      else setErr(sentence(ex));
      setSaving(false);
    }
  }

  return (
    <form
      onSubmit={submit}
      onKeyDown={(e) => { if (e.key === "Escape" && !saving) { e.stopPropagation(); onCancel(); } }}
      className="slds-theme_shade slds-p-around_small flex flex-col gap-3"
    >
      {err && <ScopedNotification theme="error">{err}</ScopedNotification>}
      <div className="grid gap-3 md:grid-cols-2">
        <Input ref={first} label={t("studio.operate.users.form.fullName")} required autoComplete="off" value={form.fullName} onChange={(e) => set("fullName", e.target.value)} />
        <Input label={t("studio.operate.users.form.email")} required type="email" autoComplete="off" value={form.email} error={emailErr ?? undefined} onChange={(e) => set("email", e.target.value)} />
        <div className="flex items-end gap-2">
          <div className="min-w-0 flex-1">
            <Input
              label={t("studio.operate.users.form.password")}
              required
              minLength={PASSWORD_MIN_LENGTH}
              type={show ? "text" : "password"}
              autoComplete="new-password"
              help={t("studio.operate.users.form.passwordHelp")}
              value={form.password}
              onChange={(e) => { setGenerated(false); set("password", e.target.value); }}
            />
          </div>
          <Button variant="base" onClick={() => setShow((s) => !s)}>{t(show ? "studio.operate.users.form.hide" : "studio.operate.users.form.show")}</Button>
          <Button variant="neutral" onClick={() => { set("password", generatePassword()); setGenerated(true); }}>{t("studio.operate.users.form.generate")}</Button>
        </div>
        <Select
          label={t("studio.operate.users.form.role")}
          value={form.roleId ?? ""}
          onChange={(e) => set("roleId", e.target.value)}
          options={roles.map((r) => ({ value: r.id, label: r.label ?? r.name }))}
          placeholder={t("studio.operate.users.noRole")}
        />
      </div>
      <Checkbox label={t("studio.operate.users.form.isAdmin")} checked={!!form.isAdmin} onChange={(e) => set("isAdmin", e.target.checked)} />
      <div className="slds-grid slds-grid_align-end">
        <Button variant="neutral" disabled={saving} onClick={onCancel}>{t("studio.operate.users.form.cancel")}</Button>
        <Button variant="brand" type="submit" className="slds-m-left_x-small" disabled={saving}>{t("studio.operate.users.form.save")}</Button>
      </div>
    </form>
  );
}
