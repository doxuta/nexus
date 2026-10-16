"use client";

// Vận hành › Việc tự động › Khoá truy cập (specs/078 lát u7-khoa-truy-cap, FR-078, FR-006).
// BE contract (nexus-platform/internal/handlers/admin_apikey_handler.go):
//   GET    /admin/api-keys     → { data: { apiKeys } } — metadata only, never a secret/hash.
//   POST   /admin/api-keys     → 201 { data: { apiKey, secret } } — the secret is in this answer ONLY, once.
//   DELETE /admin/api-keys/:id → 204 soft revoke (row kept for audit), idempotent.
// "List + result of one action": a flat table, a create form that opens IN the page and then turns into the one-time
// block, and a revoke that asks in the row. The old /admin/api-keys route (customer face) is gone, so this screen has one
// home and no Modal branch. The secret comes from a plain `await` in the submit handler and goes straight into state:
// never `useMutation` (its data would sit in the mutation cache), never a query result, URL or storage.
import { useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { Button } from "@/components/slds/Button";
import { DataTable, fill } from "@/components/slds/DataTable";
import { Input } from "@/components/slds/Input";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";
import { Spinner } from "@/components/slds/Spinner";
import { toast } from "@/components/ui/Toaster";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { QueryState } from "@/components/studio/QueryState";
import { SecretOnceBlock } from "@/components/studio/SecretOnceBlock";
import { StatusText } from "@/components/studio/StatusText";
import { OperateSection } from "@/components/studio/operate/OperateSection";
import { useScopeGate } from "@/components/studio/operate/useScopeGate";
import { useProjectQuery } from "@/components/studio/useProjectQuery";
import { ApiError, createApiKey, listApiKeys, listRoles, revokeApiKey, type ApiKey, type RoleSummary } from "@/lib/admin";
import { formatDate, formatDateTime } from "@/lib/format";
import { t } from "@/lib/i18n";
import { useStudioIdentity } from "@/lib/studioContext";

/** Trạng thái hiển thị của một khoá — thu hồi thắng hết hạn (hàng đã revoke là hàng chết). */
export function keyStatus(k: Pick<ApiKey, "revokedAt" | "expiresAt">, now = new Date()): "active" | "revoked" | "expired" {
  if (k.revokedAt) return "revoked";
  if (k.expiresAt && new Date(k.expiresAt) <= now) return "expired";
  return "active";
}

const prefixText = (k: Pick<ApiKey, "keyPrefix">) => fill(t("studio.operate.apiKeys.prefix"), { prefix: k.keyPrefix });
const STATUS = {
  active: { tone: "success", key: "studio.operate.apiKeys.status.active" },
  revoked: { tone: "weak", key: "studio.operate.apiKeys.status.revoked" },
  expired: { tone: "warn", key: "studio.operate.apiKeys.status.expired" },
} as const;
const LIST_KEY = (slug: string) => ["project", slug, "operate", "apikeys"] as const;

/** Mount gate (same as the key vault page): while the token in hand is not this project's, nothing is fetched or drawn;
 *  the key on `slug` throws away the form, the one-time block and the list on a switch. */
export function ApiKeys({ slug }: { slug: string }) {
  const identity = useStudioIdentity();
  if (!identity.matches(slug)) {
    return (
      <div className="slds-is-relative slds-p-around_large">
        <Spinner size="small" />
      </div>
    );
  }
  return <ApiKeysPage key={slug} slug={slug} />;
}

function ApiKeysPage({ slug }: { slug: string }) {
  const qc = useQueryClient();
  const guard = useScopeGate(true);
  const keys = useProjectQuery(slug, ["operate", "apikeys"], listApiKeys);
  const roles = useProjectQuery(slug, ["operate", "roles"], listRoles);
  const [formOpen, setFormOpen] = useState(false);
  const [made, setMade] = useState<{ apiKey: ApiKey; secret: string } | null>(null);
  const [revokeFor, setRevokeFor] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [rowErr, setRowErr] = useState<Record<string, string>>({});
  const newBtn = useRef<HTMLButtonElement>(null);

  const list = useMemo(() => keys.data ?? [], [keys.data]);
  const roleLabel = (id: string) => {
    const r = roles.data?.find((x) => x.id === id);
    return r ? r.label?.trim() || r.name : "—";
  };
  const refresh = () => qc.invalidateQueries({ queryKey: LIST_KEY(slug) });

  async function revoke(k: ApiKey) {
    setBusy(k.id);
    setRowErr((m) => ({ ...m, [k.id]: "" }));
    try {
      if (!guard.assertScope?.()) {
        setRowErr((m) => ({ ...m, [k.id]: t("studio.operate.apiKeys.revoke.scope") }));
        return;
      }
      await revokeApiKey(k.id);
      setRevokeFor(null);
      toast.success(fill(t("studio.operate.apiKeys.revoke.done"), { name: k.name }));
      await refresh();
    } catch {
      setRowErr((m) => ({ ...m, [k.id]: t("studio.operate.apiKeys.revoke.error") }));
    } finally {
      setBusy(null);
    }
  }

  const columns = useMemo<ColumnDef<ApiKey>[]>(
    () => [
      { id: "name", header: t("studio.operate.apiKeys.col.name"), accessorFn: (k) => k.name, cell: ({ row: { original: k } }) => <strong className="slds-truncate block">{k.name}</strong> },
      { id: "role", header: t("studio.operate.apiKeys.col.role"), accessorFn: (k) => roleLabel(k.roleId) },
      { id: "prefix", header: t("studio.operate.apiKeys.col.prefix"), accessorFn: (k) => prefixText(k), cell: ({ row: { original: k } }) => <span className="slds-text-font_monospace">{prefixText(k)}</span> },
      { id: "expires", header: t("studio.operate.apiKeys.col.expires"), accessorFn: (k) => k.expiresAt ?? "", cell: ({ row: { original: k } }) => (k.expiresAt ? formatDateTime(k.expiresAt) : t("studio.operate.apiKeys.never")) },
      {
        id: "status",
        header: t("studio.operate.apiKeys.col.status"),
        accessorFn: (k) => keyStatus(k),
        cell: ({ row: { original: k } }) => {
          const s = STATUS[keyStatus(k)];
          return (
            <>
              <StatusText tone={s.tone}>{t(s.key)}</StatusText>
              {k.revokedAt && <span className="slds-text-body_small slds-text-color_weak block">{fill(t("studio.operate.apiKeys.revokedAt"), { time: formatDate(k.revokedAt) })}</span>}
            </>
          );
        },
      },
      {
        id: "actions",
        header: () => <span className="slds-assistive-text">{t("studio.operate.apiKeys.col.actions")}</span>,
        enableSorting: false,
        cell: ({ row: { original: k } }) =>
          k.revokedAt ? null : (
            <span className="slds-grid slds-grid_align-end">
              <Button variant="base" className="whitespace-nowrap" disabled={busy === k.id} aria-label={fill(t("studio.operate.apiKeys.revoke.rowOf"), { name: k.name })} onClick={() => setRevokeFor(k.id)}>
                {t("studio.operate.apiKeys.revoke.row")}
              </Button>
            </span>
          ),
      },
    ],
    // roleLabel closes over the roles answer; busy gates the row button.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [roles.data, busy],
  );

  const rowDetail = (k: ApiKey) => {
    const parts = [];
    if (rowErr[k.id]) parts.push(<ScopedNotification key="e" theme="error">{rowErr[k.id]}</ScopedNotification>);
    if (revokeFor === k.id) {
      parts.push(
        <InlineConfirm
          key="revoke"
          tone="error"
          message={fill(t("studio.operate.apiKeys.revoke.ask"), { prefix: prefixText(k) })}
          confirmLabel={t("studio.operate.apiKeys.revoke.confirm")}
          busy={busy === k.id}
          onConfirm={() => void revoke(k)}
          onCancel={() => setRevokeFor(null)}
        />,
      );
    }
    return parts.length ? <div className="flex flex-col gap-2 py-2">{parts}</div> : null;
  };

  return (
    <div className="slds-p-horizontal_medium slds-p-bottom_medium">
      <OperateSection
        title={t("studio.route.operKeys")}
        actions={
          <Button ref={newBtn} variant="brand" disabled={formOpen || made !== null} onClick={() => setFormOpen(true)}>
            {t("studio.operate.apiKeys.new")}
          </Button>
        }
      >
      <p className="nx-lede max-w-3xl">{t("studio.operate.apiKeys.lede")}</p>

      {formOpen && (
        <div className="slds-m-top_medium">
          <NewKeyForm
            roles={roles.data ?? []}
            rolesFailed={roles.isError}
            guard={guard}
            onCancel={() => {
              setFormOpen(false);
              queueMicrotask(() => newBtn.current?.focus());
            }}
            onCreated={(r) => {
              setFormOpen(false);
              setMade(r);
              void refresh();
            }}
          />
        </div>
      )}

      {made && (
        <div className="slds-m-top_medium flex flex-col gap-2">
          <p>{fill(t("studio.operate.apiKeys.created"), { name: made.apiKey.name, prefix: made.apiKey.keyPrefix })}</p>
          <SecretOnceBlock rows={[{ label: t("studio.operate.apiKeys.secret.label"), value: made.secret }]} hint={t("studio.operate.apiKeys.secret.hint")} onDone={() => { setMade(null); queueMicrotask(() => newBtn.current?.focus()); }}>
            {t("studio.operate.apiKeys.secret.warning")}
          </SecretOnceBlock>
        </div>
      )}

      <div className="slds-m-top_medium">
        <QueryState query={keys} empty={<p className="slds-p-around_medium slds-text-color_weak">{t("studio.operate.apiKeys.empty")}</p>}>
          <DataTable
            label={t("studio.operate.apiKeys.table")}
            columns={columns}
            data={list}
            getRowId={(k) => k.id}
            stacked
            rowClassName={(k) => (revokeFor === k.id ? "slds-is-selected" : undefined)}
            rowDetail={rowDetail}
          />
        </QueryState>
      </div>
      </OperateSection>
    </div>
  );
}

function NewKeyForm({ roles, rolesFailed, guard, onCancel, onCreated }: {
  roles: RoleSummary[];
  rolesFailed: boolean;
  guard: ReturnType<typeof useScopeGate>;
  onCancel: () => void;
  onCreated: (r: { apiKey: ApiKey; secret: string }) => void;
}) {
  const nameRef = useRef<HTMLInputElement>(null);
  const roleRef = useRef<HTMLSelectElement>(null);
  const expiryRef = useRef<HTMLInputElement>(null);
  const [name, setName] = useState("");
  const [roleId, setRoleId] = useState("");
  const [expiresLocal, setExpiresLocal] = useState(""); // datetime-local, optional
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [nameErr, setNameErr] = useState<string | null>(null);
  const [roleErr, setRoleErr] = useState<string | null>(null);
  const [expiryErr, setExpiryErr] = useState<string | null>(null);
  useEffect(() => nameRef.current?.focus(), []);

  async function submit(e: FormEvent) {
    e.preventDefault();
    const ne = name.trim() ? null : t("studio.operate.apiKeys.form.err.name");
    const re = roleId ? null : t("studio.operate.apiKeys.form.err.role");
    setNameErr(ne);
    setRoleErr(re);
    setExpiryErr(null);
    if (ne || re) {
      (ne ? nameRef : roleRef).current?.focus();
      return;
    }
    setSaving(true);
    setErr(null);
    try {
      if (!guard.assertScope?.()) {
        setErr(t("studio.operate.apiKeys.form.err.scope"));
        return;
      }
      // plain await: the secret is in the answer and must not enter a mutation cache
      onCreated(await createApiKey({ name: name.trim(), roleId, expiresAt: expiresLocal ? new Date(expiresLocal).toISOString() : undefined }));
    } catch (ex) {
      if (ex instanceof ApiError && ex.code === "APIKEY_ROLE_INVALID") {
        setRoleErr(t("studio.operate.apiKeys.form.err.roleInvalid"));
        roleRef.current?.focus();
      } else if (ex instanceof ApiError && ex.code === "APIKEY_EXPIRY_INVALID") {
        setExpiryErr(t("studio.operate.apiKeys.form.err.expiry"));
        expiryRef.current?.focus();
      } else {
        setErr(t("studio.operate.apiKeys.form.err.generic"));
      }
    } finally {
      setSaving(false);
    }
  }

  return (
    <form
      onSubmit={submit}
      noValidate
      onKeyDown={(e) => { if (e.key === "Escape" && !saving) { e.stopPropagation(); onCancel(); } }}
      className="slds-theme_shade slds-p-around_small flex flex-col gap-3"
    >
      {err && <ScopedNotification theme="error">{err}</ScopedNotification>}
      {rolesFailed && <ScopedNotification theme="error">{t("studio.operate.apiKeys.roles.error")}</ScopedNotification>}
      <div className="grid gap-3 md:grid-cols-2">
        <Input
          ref={nameRef}
          label={t("studio.operate.apiKeys.form.name")}
          required
          maxLength={100}
          autoComplete="off"
          value={name}
          placeholder={t("studio.operate.apiKeys.form.namePh")}
          error={nameErr ?? undefined}
          onChange={(e) => { setName(e.target.value); setNameErr(null); }}
        />
        <Select
          ref={roleRef}
          label={t("studio.operate.apiKeys.form.role")}
          required
          value={roleId}
          placeholder={t("studio.operate.apiKeys.form.rolePh")}
          options={roles.map((r) => ({ value: r.id, label: r.label?.trim() || r.name }))}
          help={t("studio.operate.apiKeys.form.roleHelp")}
          error={roleErr ?? undefined}
          onChange={(e) => { setRoleId(e.target.value); setRoleErr(null); }}
        />
        <Input
          ref={expiryRef}
          label={t("studio.operate.apiKeys.form.expiry")}
          type="datetime-local"
          value={expiresLocal}
          help={t("studio.operate.apiKeys.form.expiryHelp")}
          error={expiryErr ?? undefined}
          onChange={(e) => { setExpiresLocal(e.target.value); setExpiryErr(null); }}
        />
      </div>
      <div className="flex gap-2">
        <Button type="submit" variant="brand" disabled={saving}>{saving ? t("action.saving") : t("studio.operate.apiKeys.form.save")}</Button>
        <Button variant="neutral" onClick={onCancel} disabled={saving}>{t("action.cancel")}</Button>
      </div>
    </form>
  );
}
