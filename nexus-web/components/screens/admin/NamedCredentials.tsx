"use client";

// Kho khoá kết nối — quản trị khách cất khoá của đối tác một lần, quy trình chỉ chọn theo tên
// (specs/050-named-credentials S8a; BE: admin_credential_handler.go, adminGated).
//   GET /admin/credentials · GET /:id (popover "Đang dùng") · POST (hmac tự sinh trả `secret` MỘT lần)
//   · DELETE (409 CREDENTIAL_IN_USE kèm details.usages, 503 thiếu khoá chủ).
// Ràng buộc bảo mật hiển thị trung thực:
//   * khoá tự sinh chỉ nằm trong state của modal tạo: đóng modal là mất, không đi qua cache/storage;
//   * bảng chỉ render các trường allowlist của CredentialView — không bao giờ secret.
// S8b: PUT (sửa thông tin, cùng máy chủ) · POST /:id/rotate (xoay khoá; "Nhập khoá" là cùng hộp thoại cho chỗ
// khoá còn trống). Khoá tự sinh khi xoay hiện MỘT lần như khi tạo. Kiểu "khoá trong đường dẫn" (BE S5, #207) chưa có.
//
// SLDS: page-headers/base · card + data-tables/base · modals/base (form → khoá một lần cùng modal) ·
// popover · scoped-notifications · prompt (xoá, ConfirmPrompt dùng chung).

import { useCallback, useEffect, useId, useRef, useState, type FormEvent, type KeyboardEvent, type ReactNode } from "react";
import { Check, ChevronDown, Copy, KeyRound, Plus, Trash2 } from "lucide-react";
import {
  listCredentials, getCredential, createCredential, deleteCredential, updateCredential, rotateCredential,
  ensureAuth, ApiError,
  type NamedCredential, type CredentialAuthType, type CredentialUsage, type CreateCredentialInput,
  type UpdateCredentialInput, type RotateCredentialInput,
} from "@/lib/admin";
import { t, tf } from "@/lib/i18n";
import { toast } from "@/components/ui/Toaster";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Card } from "@/components/slds/Card";
import { Checkbox } from "@/components/slds/Checkbox";
import { EmptyState } from "@/components/slds/EmptyState";
import { ExpandableSection } from "@/components/slds/ExpandableSection";
import { Input } from "@/components/slds/Input";
import { Menu, MenuItem } from "@/components/slds/Menu";
import { Modal } from "@/components/slds/Modal";
import { PageHeader } from "@/components/slds/PageHeader";
import { Popover } from "@/components/slds/Popover";
import { RadioGroup } from "@/components/slds/RadioGroup";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";
import { Spinner } from "@/components/slds/Spinner";
import { Toggle } from "@/components/slds/Toggle";
import { useEmbedded } from "@/components/slds/EmbeddedSurface";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { SecretOnceBlock } from "@/components/studio/SecretOnceBlock";
import type { MassOpGuard } from "@/lib/api";
import { ConfirmPrompt } from "@/components/studio/workspaceShared";

const STATUS_THEME = {
  ready: "success", needs_secret: "warning", grace: "warning", key_unavailable: "error",
} as const;

const AUTH_TYPES: CredentialAuthType[] = ["header", "basic", "hmac", "url_token"];

function fmtTime(iso: string | null | undefined): string {
  if (!iso) return "—";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? "—" : d.toLocaleString("vi-VN");
}

// Dialog on the customer's own face; a plain section in the page inside the Studio frame (specs/078 FR-077: no dialog to
// create, edit, rotate or enter a key). `locked` = Esc must not close it (a request is running or a secret is on screen).
function Pane({ open = true, onOpenChange, title, tagline, footer, locked, children }: {
  open?: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  tagline?: ReactNode;
  footer?: ReactNode;
  locked?: boolean;
  children: ReactNode;
}) {
  const embedded = useEmbedded();
  const id = useId();
  const box = useRef<HTMLElement>(null);
  const shown = embedded && open;
  useEffect(() => {
    if (!shown) return;
    const from = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    box.current?.focus();
    return () => { if (from?.isConnected) from.focus(); };
  }, [shown]);
  if (!embedded) {
    return <Modal open={open} onOpenChange={onOpenChange} title={title} tagline={tagline} footer={footer}>{children}</Modal>;
  }
  if (!open) return null;
  const onKeyDown = (e: KeyboardEvent<HTMLElement>) => {
    if (e.key === "Escape" && !locked) { e.stopPropagation(); onOpenChange(false); }
  };
  return (
    <section ref={box} tabIndex={-1} aria-labelledby={id} onKeyDown={onKeyDown} className="slds-m-bottom_large">
      <h2 id={id} className="slds-text-heading_medium">{title}</h2>
      {tagline && <p className="slds-text-color_weak slds-m-top_xx-small">{tagline}</p>}
      <div className="slds-m-top_small">{children}</div>
      {footer && <div className="slds-m-top_medium flex flex-wrap justify-end gap-2">{footer}</div>}
    </section>
  );
}

// The key shown once: SecretOnceBlock in the Studio (the tick gates "Xong"), the modal's own block on the customer's face.
function Reveal({ value, onDone }: { value: string; onDone: () => void }) {
  return useEmbedded()
    ? <SecretOnceBlock rows={[{ label: t("page.credentials.secret.label"), value }]} onDone={onDone} />
    : <OnceSecret value={value} />;
}

const usageLine = (u: CredentialUsage) => tf("page.credentials.usage.item", { name: u.workflowName, n: u.actionIndex + 1 });

/** `guard`: set by the Studio page (components/studio/operate/CredentialsPage); the customer's own face needs none. */
export function NamedCredentials({ guard }: { guard?: MassOpGuard } = {}) {
  const embedded = useEmbedded();
  // FR-131: the token may already belong to another customer; every write asks again, right before it is sent.
  const scopeOk = () => guard?.assertScope?.() ?? true;
  const scopeRef = useRef(scopeOk);
  useEffect(() => { scopeRef.current = scopeOk; });
  // A show-once key is on screen (create / rotate / enter): nothing else may open, or the new key is lost unseen.
  const [revealing, setRevealing] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);
  const [rows, setRows] = useState<NamedCredential[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<{ message: string; notConfigured: boolean } | null>(null);
  const [createOpen, setCreateOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<NamedCredential | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [inUse, setInUse] = useState<CredentialUsage[] | null>(null);
  const [editTarget, setEditTarget] = useState<NamedCredential | null>(null);
  // `baseUrl` seeds the rotate dialog when the edit dialog sent the operator here for a host change.
  const [rotateTarget, setRotateTarget] = useState<{ cred: NamedCredential; baseUrl?: string } | null>(null);

  const load = useCallback(async () => {
    try {
      await ensureAuth();
      if (!scopeRef.current()) return; // the token moved to another customer: draw nothing of theirs
      setRows(await listCredentials());
      setErr(null);
    } catch (e) {
      setErr(e instanceof ApiError && e.status === 503
        ? { message: t("page.credentials.notConfigured"), notConfigured: true }
        : { message: e instanceof ApiError ? e.message : t("page.credentials.loadError"), notConfigured: false });
    } finally {
      setLoading(false);
    }
  }, []);

  const reload = () => {
    setLoading(true);
    load();
  };

  useEffect(() => {
    (async () => { await load(); })();
  }, [load]);

  async function handleDelete(c: NamedCredential) {
    setNotice(null);
    if (!scopeOk()) {
      setDeleteTarget(null);
      setNotice(t("studio.operate.credentials.scope"));
      return;
    }
    setDeleting(true);
    try {
      await deleteCredential(c.id);
      toast.success(t("page.credentials.delete.done"));
      setDeleteTarget(null);
      reload();
    } catch (e) {
      setDeleteTarget(null);
      if (e instanceof ApiError && e.code === "CREDENTIAL_IN_USE") setInUse(e.details.usages ?? []);
      else toast.error(e instanceof ApiError ? e.message : t("page.credentials.delete.error"));
    } finally {
      setDeleting(false);
    }
  }

  let content;
  if (loading) {
    content = (
      <div className="relative min-h-32">
        <Spinner />
      </div>
    );
  } else if (err) {
    content = <ScopedNotification theme={err.notConfigured ? "warning" : "error"}>{err.message}</ScopedNotification>;
  } else if (rows.length === 0) {
    content = (
      <Card title={t("page.credentials.list")} icon={{ glyph: KeyRound, name: "password" }}>
        <EmptyState icon={KeyRound} title={t("page.credentials.empty")} description={t("page.credentials.emptyDesc")} />
      </Card>
    );
  } else {
    content = (
      <Card
        title={t("page.credentials.list")}
        icon={{ glyph: KeyRound, name: "password" }}
        count={rows.length}
        padded={false}
        footer={<p className="slds-text-align_left slds-text-body_small slds-text-color_weak">{t("page.credentials.footnote")}</p>}
      >
        <div className="overflow-x-auto">
          <table className="slds-table slds-table_cell-buffer slds-table_bordered" aria-label={t("page.credentials.grid")}>
            <thead>
              <tr className="slds-line-height_reset">
                {(["name", "baseUrl", "type", "status", "hint", "rotated", "usage"] as const).map((c) => (
                  <th key={c} scope="col">
                    <div className="slds-truncate" title={t(`page.credentials.col.${c}`)}>{t(`page.credentials.col.${c}`)}</div>
                  </th>
                ))}
                <th scope="col" className="slds-cell-shrink">
                  <span className="slds-assistive-text">{t("page.credentials.col.actions")}</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {rows.map((c) => (
                <tr key={c.id} className="slds-hint-parent">
                  <th scope="row" data-label={t("page.credentials.col.name")}>
                    <div className="slds-truncate max-w-xs" title={c.label}><strong>{c.label}</strong></div>
                    <div className="slds-truncate slds-text-body_small slds-text-color_weak slds-text-font_monospace">{c.name}</div>
                  </th>
                  <td data-label={t("page.credentials.col.baseUrl")}>
                    <div className="slds-truncate max-w-xs" title={c.baseUrl}>{c.baseUrl}</div>
                  </td>
                  <td data-label={t("page.credentials.col.type")}><Badge>{t(`page.credentials.type.${c.authType}`)}</Badge></td>
                  <td data-label={t("page.credentials.col.status")}>
                    <Badge theme={STATUS_THEME[c.status]}>
                      {c.status === "grace"
                        ? tf("page.credentials.status.grace", { time: fmtTime(c.previousExpiresAt) })
                        : t(`page.credentials.status.${c.status}`)}
                    </Badge>
                  </td>
                  <td data-label={t("page.credentials.col.hint")} className="slds-text-font_monospace">{c.secretHint ? `…${c.secretHint}` : "—"}</td>
                  <td data-label={t("page.credentials.col.rotated")} className="tabular-nums">{fmtTime(c.rotatedAt)}</td>
                  <td data-label={t("page.credentials.col.usage")}><UsageCell cred={c} scopeOk={scopeOk} /></td>
                  <td>
                    <Menu
                      align="right"
                      trigger={
                        <ButtonIcon
                          icon={ChevronDown}
                          variant="border-filled"
                          size="small"
                          label={tf("page.credentials.rowMenu", { name: c.label })}
                          disabled={revealing}
                        />
                      }
                    >
                      <MenuItem label={t("page.credentials.edit")} onSelect={() => setEditTarget(c)} />
                      <MenuItem
                        label={t(c.status === "needs_secret" ? "page.credentials.enter" : "page.credentials.rotate")}
                        onSelect={() => setRotateTarget({ cred: c })}
                      />
                      <MenuItem label={t("page.credentials.delete.confirm")} onSelect={() => setDeleteTarget(c)} />
                    </Menu>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Card>
    );
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4">
      <PageHeader
        icon={{ glyph: KeyRound, name: "password" }}
        title={t("page.credentials.title")}
        meta={tf("page.credentials.count", { n: rows.length })}
        actions={[
          <Button key="create" variant="brand" iconLeft={Plus} onClick={() => setCreateOpen(true)} disabled={err?.notConfigured || revealing}>
            {t("page.credentials.create")}
          </Button>,
        ]}
      />

      {notice && <ScopedNotification theme="warning">{notice}</ScopedNotification>}

      <CreateCredential
        scopeOk={scopeOk}
        onReveal={setRevealing}
        open={createOpen}
        onClose={(created) => {
          setCreateOpen(false);
          if (created) reload();
        }}
      />

      {editTarget && (
        <EditCredential
          scopeOk={scopeOk}
          key={editTarget.id}
          cred={editTarget}
          onClose={(saved) => {
            setEditTarget(null);
            if (saved) reload();
          }}
          onRotateInstead={(baseUrl) => {
            setRotateTarget({ cred: editTarget, baseUrl });
            setEditTarget(null);
          }}
        />
      )}
      {rotateTarget && (
        <RotateCredential
          scopeOk={scopeOk}
          onReveal={setRevealing}
          key={rotateTarget.cred.id}
          cred={rotateTarget.cred}
          initialBaseUrl={rotateTarget.baseUrl}
          onClose={(rotated) => {
            setRotateTarget(null);
            if (rotated) reload();
          }}
        />
      )}

      {embedded && deleteTarget && (
        <div className="slds-m-bottom_medium">
          <InlineConfirm
            tone="error"
            message={tf("page.credentials.delete.title", { name: deleteTarget.label })}
            confirmLabel={t("page.credentials.delete.confirm")}
            busy={deleting}
            onConfirm={() => handleDelete(deleteTarget)}
            onCancel={() => setDeleteTarget(null)}
          >
            <p className="slds-text-body_small slds-text-color_weak slds-m-top_x-small">{t("page.credentials.delete.desc")}</p>
          </InlineConfirm>
        </div>
      )}
      <ConfirmPrompt
        open={deleteTarget !== null && !embedded}
        onOpenChange={(o) => { if (!o) setDeleteTarget(null); }}
        title={deleteTarget ? tf("page.credentials.delete.title", { name: deleteTarget.label }) : ""}
        description={t("page.credentials.delete.desc")}
        danger
        confirmLabel={t("page.credentials.delete.confirm")}
        loading={deleting}
        onConfirm={() => deleteTarget && handleDelete(deleteTarget)}
      />

      <Pane
        open={inUse !== null}
        onOpenChange={(o) => { if (!o) setInUse(null); }}
        title={t("page.credentials.inUse.title")}
        footer={<Button variant="brand" onClick={() => setInUse(null)}>{t("page.credentials.inUse.close")}</Button>}
      >
        <p className="slds-m-bottom_small">{t("page.credentials.inUse.desc")}</p>
        <ul className="slds-list_dotted">
          {(inUse ?? []).map((u) => <li key={`${u.workflowId}-${u.actionIndex}`}>{usageLine(u)}</li>)}
        </ul>
      </Pane>

      {content}
    </div>
  );
}

// "Đang dùng N quy trình": the list is fetched when the popover opens (GET /:id), not for every row.
function UsageCell({ cred, scopeOk }: { cred: NamedCredential; scopeOk: () => boolean }) {
  const [usages, setUsages] = useState<CredentialUsage[] | null>(null);
  const [failed, setFailed] = useState(false);
  if (cred.usageCount === 0) return <span className="slds-text-color_weak">{t("page.credentials.usage.none")}</span>;
  return (
    <Popover
      title={t("page.credentials.usage.title")}
      side="bottom"
      align="start"
      onOpenChange={(o) => {
        if (!o || usages) return;
        // FR-131: even a read of one key waits for the project guard (the list may be stale after a project switch).
        if (!scopeOk()) { setFailed(true); return; }
        setFailed(false);
        getCredential(cred.id).then((r) => setUsages(r.usages), () => setFailed(true));
      }}
      trigger={<Button variant="base">{tf("page.credentials.usage.count", { n: cred.usageCount })}</Button>}
    >
      {failed ? (
        <p>{t("page.credentials.usage.error")}</p>
      ) : usages ? (
        <ul className="slds-list_dotted">
          {usages.map((u) => <li key={`${u.workflowId}-${u.actionIndex}`}>{usageLine(u)}</li>)}
        </ul>
      ) : (
        <div className="relative min-h-12"><Spinner /></div>
      )}
    </Popover>
  );
}

// ── Tạo khoá: form → khoá tự sinh một-lần, cùng một modal ───────────────────────────────────────
type HeaderRow = { id: number; name: string; value: string };

function CreateCredential({ open, onClose, scopeOk, onReveal }: {
  open: boolean;
  onReveal: (on: boolean) => void;
  scopeOk: () => boolean;
  /** `created`: a key was stored (the list must reload) — also true when the reveal is closed. */
  onClose: (created: boolean) => void;
}) {
  const embedded = useEmbedded();
  const [name, setName] = useState("");
  const [label, setLabel] = useState("");
  const [baseUrl, setBaseUrl] = useState("");
  const [authType, setAuthType] = useState<CredentialAuthType>("header");
  const [headerName, setHeaderName] = useState("");
  // null = untouched, so the server default ("Bearer " on Authorization) applies; "" = explicitly none.
  const [headerPrefix, setHeaderPrefix] = useState<string | null>(null);
  const [username, setUsername] = useState("");
  const [secret, setSecret] = useState("");
  const [showSecret, setShowSecret] = useState(false);
  const [generate, setGenerate] = useState(false);
  const [extra, setExtra] = useState<HeaderRow[]>([]);
  const nextRow = useRef(1);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [fieldErr, setFieldErr] = useState<Record<string, string>>({});
  const [confirmLeave, setConfirmLeave] = useState(false);
  const [created, setCreated] = useState<{ credential: NamedCredential; secret: string } | null>(null);
  const hasSecret = created !== null;
  useEffect(() => { onReveal(hasSecret); return () => onReveal(false); }, [hasSecret, onReveal]);

  // A fresh form every time the modal opens; closing drops the generated key with the rest of the state.
  const [wasOpen, setWasOpen] = useState(open);
  if (open !== wasOpen) {
    setWasOpen(open);
    setName(""); setLabel(""); setBaseUrl(""); setAuthType("header"); setHeaderName(""); setHeaderPrefix(null);
    setUsername(""); setSecret(""); setShowSecret(false); setGenerate(false); setExtra([]);
    setErr(null); setFieldErr({}); setCreated(null); setConfirmLeave(false);
  }

  const isAuthzHeader = headerName.trim() === "" || headerName.trim().toLowerCase() === "authorization";
  const prefixShown = headerPrefix ?? (isAuthzHeader ? "Bearer " : "");

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const input: CreateCredentialInput = { name: name.trim(), baseUrl: baseUrl.trim(), authType };
    if (label.trim()) input.label = label.trim();
    if (authType === "header") {
      if (headerName.trim()) input.headerName = headerName.trim();
      input.headerPrefix = prefixShown; // always send what the field shows (server defaults would differ silently)
    }
    if (authType === "basic" && username.trim()) input.username = username.trim();
    if (authType === "hmac" && generate) input.generateSecret = true;
    else if (secret) input.secret = secret;
    const headers = extra.filter((r) => r.name.trim());
    if (headers.length) input.extraHeaders = Object.fromEntries(headers.map((r) => [r.name.trim(), r.value]));

    if (!scopeOk()) {
      setErr(t("studio.operate.credentials.scope"));
      return;
    }
    setSaving(true);
    setErr(null);
    setFieldErr({});
    try {
      const r = await createCredential(input);
      setSecret("");
      if (r.secret) setCreated({ credential: r.credential, secret: r.secret });
      else onClose(true);
    } catch (ex) {
      const msg = ex instanceof ApiError ? ex.message : t("page.credentials.form.err.generic");
      const field = ex instanceof ApiError ? ex.details.field : undefined;
      const hasField = authType === "header" ? ["headerName", "headerPrefix"] : authType === "basic" ? ["username"] : [];
      const shown = ["name", "label", "baseUrl", "authType", ...hasField, ...(generate ? [] : ["secret"])];
      if (field && shown.includes(field)) setFieldErr({ [field]: msg });
      else if (field) setErr(msg); // no visible input owns this field (e.g. generateSecret, collapsed extraHeaders)
      else setErr(ex instanceof ApiError && ex.status === 503 ? t("page.credentials.notConfigured") : msg);
    } finally {
      setSaving(false);
    }
  }

  const close = () => onClose(created !== null);
  const setRow = (id: number, patch: Partial<HeaderRow>) => setExtra((rs) => rs.map((r) => (r.id === id ? { ...r, ...patch } : r)));

  return (
    <Pane
      open={open}
      onOpenChange={(o) => {
        if (o || saving) return;
        if (created) setConfirmLeave(true);
        else close();
      }}
      locked={saving || created !== null}
      title={created ? tf("page.credentials.secret.title", { name: created.credential.label }) : t("page.credentials.form.title")}
      tagline={t(created ? "page.credentials.secret.tagline" : "page.credentials.form.tagline")}
      footer={
        created ? (
          embedded ? null : <Button variant="brand" onClick={close}>{t("page.credentials.secret.done")}</Button>
        ) : (
          <>
            <Button onClick={close} disabled={saving}>{t("action.cancel")}</Button>
            <Button type="submit" form="credential-create-form" variant="brand" disabled={saving}>
              {saving ? t("action.saving") : t("page.credentials.create")}
            </Button>
          </>
        )
      }
    >
      {created ? (
        <Reveal value={created.secret} onDone={close} />
      ) : (
        <form id="credential-create-form" onSubmit={handleSubmit} noValidate className="slds-form flex flex-col gap-3">
          {err && <ScopedNotification theme="error">{err}</ScopedNotification>}
          <Input
            label={t("page.credentials.form.name")}
            required
            maxLength={64}
            value={name}
            onChange={(e) => setName(e.target.value)}
            help={t("page.credentials.form.nameHelp")}
            error={fieldErr.name}
          />
          <Input
            label={t("page.credentials.form.label")}
            maxLength={120}
            value={label}
            onChange={(e) => setLabel(e.target.value)}
            error={fieldErr.label}
          />
          <Input
            label={t("page.credentials.form.baseUrl")}
            required
            type="url"
            value={baseUrl}
            onChange={(e) => setBaseUrl(e.target.value)}
            placeholder={authType === "url_token" ? t("page.credentials.form.baseUrlTokenPlaceholder") : "https://"}
            help={t(authType === "url_token" ? "page.credentials.form.baseUrlTokenHelp" : "page.credentials.form.baseUrlHelp")}
            error={fieldErr.baseUrl}
          />
          <RadioGroup
            label={t("page.credentials.form.authType")}
            value={authType}
            onValueChange={(v) => { setAuthType(v as CredentialAuthType); setGenerate(false); setSecret(""); }}
            options={AUTH_TYPES.map((v) => ({ value: v, label: t(`page.credentials.type.${v}`) }))}
            error={fieldErr.authType}
          />
          {authType === "header" && (
            <>
              <Input
                label={t("page.credentials.form.headerName")}
                value={headerName}
                onChange={(e) => setHeaderName(e.target.value)}
                help={t("page.credentials.form.headerNameHelp")}
                error={fieldErr.headerName}
              />
              <Input
                label={t("page.credentials.form.headerPrefix")}
                value={prefixShown}
                onChange={(e) => setHeaderPrefix(e.target.value)}
                help={t("page.credentials.form.headerPrefixHelp")}
                error={fieldErr.headerPrefix}
              />
            </>
          )}
          {authType === "basic" && (
            <Input
              label={t("page.credentials.form.username")}
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              error={fieldErr.username}
            />
          )}
          {authType === "hmac" && (
            <Toggle
              label={t("page.credentials.form.generate")}
              help={t("page.credentials.form.generateHelp")}
              checked={generate}
              onChange={(e) => setGenerate(e.target.checked)}
            />
          )}
          {!generate && (
            <>
              <Input
                label={t("page.credentials.form.secret")}
                type={showSecret ? "text" : "password"}
                autoComplete="new-password"
                value={secret}
                onChange={(e) => setSecret(e.target.value)}
                help={t("page.credentials.form.secretHelp")}
                error={fieldErr.secret}
              />
              <Checkbox
                label={t("page.credentials.form.showSecret")}
                checked={showSecret}
                onChange={(e) => setShowSecret(e.target.checked)}
              />
            </>
          )}
          <ExpandableSection title={t("page.credentials.form.extra")} defaultOpen={false}>
            <div className="flex flex-col gap-2">
              {fieldErr.extraHeaders && <ScopedNotification theme="error">{fieldErr.extraHeaders}</ScopedNotification>}
              {extra.map((r, i) => (
                <div key={r.id} className="flex items-end gap-2">
                  <div className="min-w-0 flex-1">
                    <Input label={t("page.credentials.form.extraName")} value={r.name} onChange={(e) => setRow(r.id, { name: e.target.value })} />
                  </div>
                  <div className="min-w-0 flex-1">
                    <Input label={t("page.credentials.form.extraValue")} type="password" autoComplete="new-password" value={r.value} onChange={(e) => setRow(r.id, { value: e.target.value })} />
                  </div>
                  <ButtonIcon
                    variant="border-filled"
                    icon={Trash2}
                    label={tf("page.credentials.form.extraRemove", { n: i + 1 })}
                    onClick={() => setExtra((rs) => rs.filter((x) => x.id !== r.id))}
                  />
                </div>
              ))}
              <div>
                <Button iconLeft={Plus} onClick={() => setExtra((rs) => [...rs, { id: nextRow.current++, name: "", value: "" }])}>
                  {t("page.credentials.form.extraAdd")}
                </Button>
              </div>
            </div>
          </ExpandableSection>
        </form>
      )}
      <ConfirmPrompt
        open={confirmLeave}
        onOpenChange={setConfirmLeave}
        title={t("page.credentials.leave.title")}
        description={t("page.credentials.leave.desc")}
        danger
        confirmLabel={t("page.credentials.leave.confirm")}
        onConfirm={close}
      />
    </Pane>
  );
}

// Khoá tự sinh: chỉ nằm trong state của hộp thoại đang mở (không cache, không storage).
function OnceSecret({ value }: { value: string }) {
  const [copied, setCopied] = useState(false);
  async function copy() {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      toast.success(t("page.credentials.secret.copiedToast"));
      setTimeout(() => setCopied(false), 2000);
    } catch {
      toast.error(t("page.credentials.secret.copyError"));
    }
  }
  return (
    <div className="flex flex-col gap-3">
      <ScopedNotification theme="warning">
        <strong>{t("page.credentials.secret.once")}</strong>
      </ScopedNotification>
      <div className="flex items-start gap-2">
        <div className="min-w-0 flex-1">
          <Input
            label={t("page.credentials.secret.label")}
            readOnly
            value={value}
            className="slds-input_borders slds-text-font_monospace"
            data-testid="credential-secret"
            onFocus={(e) => e.target.select()}
          />
        </div>
        <ButtonIcon
          className="mt-6"
          variant="border-filled"
          icon={copied ? Check : Copy}
          label={t(copied ? "page.credentials.secret.copied" : "page.credentials.secret.copy")}
          onClick={copy}
        />
      </div>
    </div>
  );
}

// Map a 422 onto the visible input that owns `details.field`; anything else goes to the banner.
function placeError(ex: unknown, shown: string[]): { field?: Record<string, string>; banner?: string } {
  if (!(ex instanceof ApiError)) return { banner: t("page.credentials.form.err.generic") };
  const field = ex.details.field;
  if (field && shown.includes(field)) return { field: { [field]: ex.message } };
  if (ex.status === 503) return { banner: t("page.credentials.notConfigured") };
  return { banner: ex.message };
}

// ── Sửa thông tin: không bao giờ chạm bí mật; đổi sang máy chủ khác phải đi đường Xoay khoá ─────
function EditCredential({ cred, onClose, onRotateInstead, scopeOk }: {
  scopeOk: () => boolean;
  cred: NamedCredential;
  onClose: (saved: boolean) => void;
  onRotateInstead: (baseUrl: string) => void;
}) {
  const [label, setLabel] = useState(cred.label);
  const [baseUrl, setBaseUrl] = useState(cred.baseUrl);
  const [headerName, setHeaderName] = useState(cred.headerName);
  const [headerPrefix, setHeaderPrefix] = useState(cred.headerPrefix);
  const [username, setUsername] = useState(cred.username);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [fieldErr, setFieldErr] = useState<Record<string, string>>({});
  const [needsRotate, setNeedsRotate] = useState(false);

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const input: UpdateCredentialInput = { label: label.trim(), baseUrl: baseUrl.trim() };
    if (cred.authType === "header") {
      input.headerName = headerName.trim();
      input.headerPrefix = headerPrefix;
    }
    if (cred.authType === "basic") input.username = username.trim();
    if (!scopeOk()) {
      setErr(t("studio.operate.credentials.scope"));
      return;
    }
    setSaving(true);
    setErr(null);
    setFieldErr({});
    setNeedsRotate(false);
    try {
      await updateCredential(cred.id, input);
      toast.success(t("page.credentials.edit.done"));
      onClose(true);
    } catch (ex) {
      const hasField = cred.authType === "header" ? ["headerName", "headerPrefix"] : cred.authType === "basic" ? ["username"] : [];
      const placed = placeError(ex, ["label", "baseUrl", ...hasField]);
      if (placed.field) setFieldErr(placed.field);
      if (placed.banner) setErr(placed.banner);
      setNeedsRotate(ex instanceof ApiError && ex.code === "CREDENTIAL_ORIGIN_CHANGE_REQUIRES_ROTATE");
    } finally {
      setSaving(false);
    }
  }

  return (
    <Pane
      open
      locked={saving}
      onOpenChange={(o) => { if (!o && !saving) onClose(false); }}
      title={tf("page.credentials.edit.title", { name: cred.label })}
      tagline={t("page.credentials.edit.tagline")}
      footer={
        <>
          <Button onClick={() => onClose(false)} disabled={saving}>{t("action.cancel")}</Button>
          <Button type="submit" form="credential-edit-form" variant="brand" disabled={saving}>
            {saving ? t("action.saving") : t("page.credentials.edit.save")}
          </Button>
        </>
      }
    >
      <form id="credential-edit-form" onSubmit={handleSubmit} noValidate className="slds-form flex flex-col gap-3">
        {err && <ScopedNotification theme="error">{err}</ScopedNotification>}
        <Input label={t("page.credentials.form.name")} value={cred.name} readOnly disabled help={t("page.credentials.edit.nameFixed")} />
        <Input label={t("page.credentials.form.label")} maxLength={120} value={label} onChange={(e) => setLabel(e.target.value)} error={fieldErr.label} />
        <Input
          label={t("page.credentials.form.baseUrl")}
          required
          type="url"
          value={baseUrl}
          onChange={(e) => setBaseUrl(e.target.value)}
          help={t(cred.authType === "url_token" ? "page.credentials.edit.baseUrlTokenHelp" : "page.credentials.edit.baseUrlHelp")}
          error={fieldErr.baseUrl}
        />
        {needsRotate && (
          <div>
            <Button onClick={() => onRotateInstead(baseUrl.trim())}>{t("page.credentials.edit.toRotate")}</Button>
          </div>
        )}
        {cred.authType === "header" && (
          <>
            <Input
              label={t("page.credentials.form.headerName")}
              value={headerName}
              onChange={(e) => setHeaderName(e.target.value)}
              help={t("page.credentials.form.headerNameHelp")}
              error={fieldErr.headerName}
            />
            <Input
              label={t("page.credentials.form.headerPrefix")}
              value={headerPrefix}
              onChange={(e) => setHeaderPrefix(e.target.value)}
              error={fieldErr.headerPrefix}
            />
          </>
        )}
        {cred.authType === "basic" && (
          <Input label={t("page.credentials.form.username")} value={username} onChange={(e) => setUsername(e.target.value)} error={fieldErr.username} />
        )}
        <p className="slds-text-body_small slds-text-color_weak">{t("page.credentials.edit.secretNote")}</p>
      </form>
    </Pane>
  );
}

// ── Xoay khoá / Nhập khoá: cùng một hộp thoại. Khoá cũ không bao giờ hiện lại; khoá tự sinh hiện một lần ──
const GRACE_HOURS = [0, 1, 24, 72, 168] as const;

function sameOrigin(a: string, b: string): boolean {
  try {
    return new URL(a).origin === new URL(b).origin;
  } catch {
    return true; // unparsable input: let the server judge instead of guessing here
  }
}

function RotateCredential({ cred, initialBaseUrl, onClose, scopeOk, onReveal }: {
  onReveal: (on: boolean) => void;
  scopeOk: () => boolean;
  cred: NamedCredential;
  initialBaseUrl?: string;
  onClose: (rotated: boolean) => void;
}) {
  const embedded = useEmbedded();
  const entering = cred.status === "needs_secret"; // an empty slot: nothing old to keep, no grace
  const [baseUrl, setBaseUrl] = useState(initialBaseUrl ?? cred.baseUrl);
  const [username, setUsername] = useState(cred.username);
  const [secret, setSecret] = useState("");
  const [showSecret, setShowSecret] = useState(false);
  const [generate, setGenerate] = useState(false);
  // Master key mismatch: the old key cannot be opened, so BE only accepts grace 0 + an explicit extraHeaders.
  const lost = cred.status === "key_unavailable";
  const [grace, setGrace] = useState(lost ? "0" : "24");
  const [replaceExtra, setReplaceExtra] = useState(lost);
  const [extra, setExtra] = useState<HeaderRow[]>([]);
  const nextRow = useRef(1);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [fieldErr, setFieldErr] = useState<Record<string, string>>({});
  const [revealed, setRevealed] = useState<string | null>(null);
  const hasSecret = revealed !== null;
  useEffect(() => { onReveal(hasSecret); return () => onReveal(false); }, [hasSecret, onReveal]);
  const [confirmLeave, setConfirmLeave] = useState(false);

  const moved = !sameOrigin(cred.baseUrl, baseUrl.trim());
  const hasNewKey = generate || secret !== "";
  const setRow = (id: number, patch: Partial<HeaderRow>) => setExtra((rs) => rs.map((r) => (r.id === id ? { ...r, ...patch } : r)));

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const input: RotateCredentialInput = {};
    if (generate) input.generateSecret = true;
    else if (secret) input.secret = secret;
    if (cred.authType === "hmac" && !entering && !moved && hasNewKey) input.graceHours = Number(grace);
    if (baseUrl.trim() !== cred.baseUrl) input.baseUrl = baseUrl.trim();
    if (cred.authType === "basic" && username.trim() !== cred.username) input.username = username.trim();
    if (replaceExtra) {
      input.extraHeaders = Object.fromEntries(extra.filter((r) => r.name.trim()).map((r) => [r.name.trim(), r.value]));
    }
    if (!scopeOk()) {
      setErr(t("studio.operate.credentials.scope"));
      return;
    }
    setSaving(true);
    setErr(null);
    setFieldErr({});
    try {
      const r = await rotateCredential(cred.id, input);
      setSecret("");
      if (r.secret) setRevealed(r.secret);
      else {
        toast.success(t(entering ? "page.credentials.enter.done" : "page.credentials.rotate.done"));
        onClose(true);
      }
    } catch (ex) {
      const shown = ["baseUrl", "graceHours", ...(cred.authType === "basic" ? ["username"] : []), ...(generate ? [] : ["secret"])];
      const placed = placeError(ex, shown);
      if (placed.field) setFieldErr(placed.field);
      if (placed.banner) setErr(placed.banner);
    } finally {
      setSaving(false);
    }
  }

  const close = () => onClose(revealed !== null);
  const title = revealed
    ? tf("page.credentials.rotate.revealTitle", { name: cred.label })
    : tf(entering ? "page.credentials.enter.title" : "page.credentials.rotate.title", { name: cred.label });

  return (
    <Pane
      open
      onOpenChange={(o) => {
        if (o || saving) return;
        if (revealed) setConfirmLeave(true);
        else close();
      }}
      locked={saving || revealed !== null}
      title={title}
      tagline={t(revealed ? "page.credentials.secret.tagline" : "page.credentials.rotate.tagline")}
      footer={
        revealed ? (
          embedded ? null : <Button variant="brand" onClick={close}>{t("page.credentials.secret.done")}</Button>
        ) : (
          <>
            <Button onClick={close} disabled={saving}>{t("action.cancel")}</Button>
            <Button type="submit" form="credential-rotate-form" variant="brand" disabled={saving}>
              {saving ? t("action.saving") : t(entering ? "page.credentials.enter.submit" : "page.credentials.rotate")}
            </Button>
          </>
        )
      }
    >
      {revealed ? (
        <Reveal value={revealed} onDone={close} />
      ) : (
        <form id="credential-rotate-form" onSubmit={handleSubmit} noValidate className="slds-form flex flex-col gap-3">
          {err && <ScopedNotification theme="error">{err}</ScopedNotification>}
          {!entering && cred.authType !== "hmac" && (
            <ScopedNotification theme="info">{t("page.credentials.rotate.immediate")}</ScopedNotification>
          )}
          <Input
            label={t("page.credentials.form.baseUrl")}
            type="url"
            value={baseUrl}
            onChange={(e) => setBaseUrl(e.target.value)}
            help={t(cred.authType === "url_token" ? "page.credentials.rotate.baseUrlTokenHelp" : "page.credentials.rotate.baseUrlHelp")}
            error={fieldErr.baseUrl}
          />
          {moved && <ScopedNotification theme="warning">{t("page.credentials.rotate.moved")}</ScopedNotification>}
          {cred.authType === "basic" && (
            <Input label={t("page.credentials.form.username")} value={username} onChange={(e) => setUsername(e.target.value)} error={fieldErr.username} />
          )}
          {cred.authType === "hmac" && (
            <Toggle
              label={t("page.credentials.form.generate")}
              help={t("page.credentials.form.generateHelp")}
              checked={generate}
              onChange={(e) => { setGenerate(e.target.checked); setSecret(""); }}
            />
          )}
          {!generate && (
            <>
              <Input
                label={t("page.credentials.rotate.secret")}
                type={showSecret ? "text" : "password"}
                autoComplete="new-password"
                value={secret}
                onChange={(e) => setSecret(e.target.value)}
                help={t(moved || entering ? "page.credentials.rotate.secretRequired" : "page.credentials.rotate.secretHelp")}
                error={fieldErr.secret}
              />
              <Checkbox label={t("page.credentials.form.showSecret")} checked={showSecret} onChange={(e) => setShowSecret(e.target.checked)} />
            </>
          )}
          {cred.authType === "hmac" && !entering && !moved && (
            <Select
              label={t("page.credentials.rotate.grace")}
              value={grace}
              onChange={(e) => setGrace(e.target.value)}
              placeholder={false}
              options={GRACE_HOURS.map((h) => ({ value: String(h), label: t(`page.credentials.rotate.grace.${h}`) }))}
              help={t("page.credentials.rotate.graceHelp")}
              error={fieldErr.graceHours}
              disabled={!hasNewKey}
            />
          )}
          {!moved && cred.extraHeaderNames.length > 0 && (
            <div>
              <p className="slds-text-title slds-m-bottom_xx-small">{t("page.credentials.rotate.extraNow")}</p>
              <ul className="slds-list_dotted slds-text-font_monospace">
                {cred.extraHeaderNames.map((n) => <li key={n}>{n}</li>)}
              </ul>
              <p className="slds-text-body_small slds-text-color_weak">{t("page.credentials.rotate.extraReplaceNote")}</p>
            </div>
          )}
          <Checkbox
            label={t("page.credentials.rotate.extraReplace")}
            checked={replaceExtra}
            onChange={(e) => setReplaceExtra(e.target.checked)}
          />
          {replaceExtra && (
            <div className="flex flex-col gap-2">
              {extra.map((r, i) => (
                <div key={r.id} className="flex items-end gap-2">
                  <div className="min-w-0 flex-1">
                    <Input label={t("page.credentials.form.extraName")} value={r.name} onChange={(e) => setRow(r.id, { name: e.target.value })} />
                  </div>
                  <div className="min-w-0 flex-1">
                    <Input label={t("page.credentials.form.extraValue")} type="password" autoComplete="new-password" value={r.value} onChange={(e) => setRow(r.id, { value: e.target.value })} />
                  </div>
                  <ButtonIcon
                    variant="border-filled"
                    icon={Trash2}
                    label={tf("page.credentials.form.extraRemove", { n: i + 1 })}
                    onClick={() => setExtra((rs) => rs.filter((x) => x.id !== r.id))}
                  />
                </div>
              ))}
              <div>
                <Button iconLeft={Plus} onClick={() => setExtra((rs) => [...rs, { id: nextRow.current++, name: "", value: "" }])}>
                  {t("page.credentials.form.extraAdd")}
                </Button>
              </div>
            </div>
          )}
        </form>
      )}
      <ConfirmPrompt
        open={confirmLeave}
        onOpenChange={setConfirmLeave}
        title={t("page.credentials.leave.title")}
        description={t("page.credentials.leave.desc")}
        danger
        confirmLabel={t("page.credentials.leave.confirm")}
        onConfirm={close}
      />
    </Pane>
  );
}
