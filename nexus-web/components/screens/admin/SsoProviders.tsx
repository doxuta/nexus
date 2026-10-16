"use client";

// SSO / Đăng nhập một lần — admin CRUD cho sso_providers (ENG-AUTH2 #8, api-conventions §3c.4).
// BE contract (nexus-platform/internal/handlers/sso_handler.go — đã đối chiếu, KHÔNG đổi BE):
//   GET    /admin/sso/providers     → { data: SsoProvider[] } — không bao giờ lộ clientSecret.
//   POST   /admin/sso/providers     → 201; enabled mặc định true, trustIdpMfa mặc định false
//                                     (trustIdpMfa KHÔNG settable lúc tạo — quyết định thứ hai).
//   PATCH  /admin/sso/providers/:id → {enabled?, trustIdpMfa?} — bề mặt sửa DUY NHẤT;
//                                     đổi issuer/clientId/secret = xoá + tạo lại.
//   DELETE /admin/sso/providers/:id → 204; CASCADE thu hồi mọi identity đã link qua provider.
// Trạng thái trung thực — 503 SSO_NOT_CONFIGURED (server thiếu SSO_SECRET_ENC_KEY): ĐÃ VERIFY
// SỐNG trên :8090 (2026-07-08) — GET list vẫn trả 200 kể cả khi SSO chưa cấu hình (chỉ
// CreateProvider + /start//callback gate 503, xem sso_service.go). Vì vậy banner "chưa cấu hình"
// ở nhánh list là DEFENSIVE (nếu BE sau này gate cả list); đường phát hiện thật là lúc bấm tạo —
// create form map 503 thành lỗi nói thẳng, empty-state cũng ghi chú điều kiện server. Cảnh báo
// trust_idp_mfa hiển thị THƯỜNG TRỰC ở mỗi hàng (không giấu trong tooltip) + bước xác nhận riêng
// khi bật — đây là setting bỏ qua MFA cục bộ.
//
// SLDS (specs/038 T048): page-headers/base · card + data-tables/base (one row per provider, the two
// switches are checkbox-toggle cells) · modals/base + form-element for create · prompt for the
// trust confirm (warning) and the cascading delete (error).

import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { Eye, EyeOff, Globe, KeyRound, Plus, Trash2, TriangleAlert } from "lucide-react";
import {
  listSsoProviders, createSsoProvider, updateSsoProvider, deleteSsoProvider,
  ensureAuth, ApiError, type SsoProvider,
} from "@/lib/admin";
import { t, tf } from "@/lib/i18n";
import vi, { type I18nKey } from "@/lib/i18n/vi";
import { toast } from "@/components/ui/Toaster";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Card } from "@/components/slds/Card";
import { Checkbox } from "@/components/slds/Checkbox";
import { EmptyState } from "@/components/slds/EmptyState";
import { Icon } from "@/components/slds/Icon";
import { Input } from "@/components/slds/Input";
import { Modal, Prompt } from "@/components/slds/Modal";
import { PageHeader } from "@/components/slds/PageHeader";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { Toggle } from "@/components/slds/Toggle";
import { ConfirmPrompt } from "@/components/studio/workspaceShared";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { useEmbedded } from "@/components/slds/EmbeddedSurface";

// Cảnh báo bảo mật BẮT BUỘC (SSO-MFA-POLICY §3c.8) — copy này được test pin, đừng đổi lời mà
// không cập nhật test: bật cờ này nghĩa là đăng nhập qua provider bỏ qua bước MFA cục bộ.
export const TRUST_IDP_MFA_WARNING = t("page.sso.trustWarning");

/** Validate issuer client-side (mirror SHAPE-check của BE): URL https tuyệt đối. */
export function validateIssuer(v: string): string | null {
  const s = v.trim();
  if (!s) return t("page.sso.issuer.empty");
  let u: URL;
  try {
    u = new URL(s);
  } catch {
    return t("page.sso.issuer.url");
  }
  if (u.protocol !== "https:") return t("page.sso.issuer.https");
  return null;
}

function issuerHost(issuer: string): string {
  try {
    return new URL(issuer).host;
  } catch {
    return issuer;
  }
}

// Embedded (Studio) face reads business wording from studio.operate.sso.*; the guest face keeps page.sso.*.
const ssKey = (embedded: boolean, k: string) => {
  const sk = `studio.operate.sso.${k}`;
  return (embedded && sk in vi ? sk : `page.sso.${k}`) as I18nKey;
};
const ss = (embedded: boolean, k: string) => t(ssKey(embedded, k));
const ssf = (embedded: boolean, k: string, vars: Record<string, string | number>) => tf(ssKey(embedded, k), vars);

const providerName = (p: SsoProvider) => p.displayName?.trim() || issuerHost(p.issuer);

type Busy = { id: string; which: "enabled" | "trustIdpMfa" | "delete" } | null;

export function SsoProviders() {
  // Studio work surface (specs/078 T047): forms and confirms sit in the page, never in a dialog.
  const embedded = useEmbedded();
  const [providers, setProviders] = useState<SsoProvider[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);
  // 503 SSO_NOT_CONFIGURED — trạng thái riêng, khác lỗi thường: SSO tắt ở tầng server.
  const [notConfigured, setNotConfigured] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);
  const [busy, setBusy] = useState<Busy>(null);
  // Bật trust_idp_mfa là hạ cấp bảo mật có chủ đích → cần bước xác nhận riêng. Tắt thì không
  // (tắt = tăng an toàn, áp lại MFA cục bộ ngay).
  const [confirmTrust, setConfirmTrust] = useState<SsoProvider | null>(null);
  const [confirmDelete, setConfirmDelete] = useState<SsoProvider | null>(null);

  // Every state write in load() happens after its first await and the mount effect runs it as an
  // async callback, so nothing sets state synchronously in the effect body
  // (react-hooks/set-state-in-effect); reload() raises the spinner for a refetch after a write.
  const load = useCallback(async () => {
    try {
      await ensureAuth();
      setProviders(await listSsoProviders());
      setErr(null);
      setNotConfigured(false);
    } catch (e) {
      if (e instanceof ApiError && e.code === "SSO_NOT_CONFIGURED") {
        setNotConfigured(true);
      } else {
        setErr(e instanceof ApiError ? e.message : t("page.sso.loadError"));
      }
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

  async function patch(p: SsoProvider, body: { enabled?: boolean; trustIdpMfa?: boolean }, which: "enabled" | "trustIdpMfa") {
    setBusy({ id: p.id, which });
    try {
      await updateSsoProvider(p.id, body);
      toast.success(
        which === "enabled"
          ? t(body.enabled ? "page.sso.toast.enabledOn" : "page.sso.toast.enabledOff")
          : t(body.trustIdpMfa ? "page.sso.toast.trustOn" : "page.sso.toast.trustOff"),
      );
      reload();
    } catch (e) {
      toast.error(e instanceof ApiError ? e.message : t("page.sso.updateError"));
    } finally {
      setBusy(null);
      setConfirmTrust(null);
    }
  }

  async function handleDelete(p: SsoProvider) {
    setBusy({ id: p.id, which: "delete" });
    try {
      await deleteSsoProvider(p.id);
      toast.success(t("page.sso.delete.done"));
      setConfirmDelete(null);
      reload();
    } catch (e) {
      toast.error(e instanceof ApiError ? e.message : t("page.sso.delete.error"));
      setConfirmDelete(null);
    } finally {
      setBusy(null);
    }
  }

  let content;
  if (loading) {
    content = (
      <div className="relative min-h-32">
        <Spinner />
      </div>
    );
  } else if (notConfigured) {
    // Trạng thái trung thực: server chưa bật SSO — không phải "chưa có provider".
    content = (
      <ScopedNotification theme="warning">
        <strong>{t("page.sso.notConfigured.title")}</strong>
        <br />
        {t("page.sso.notConfigured.body")}
      </ScopedNotification>
    );
  } else if (err) {
    content = <ScopedNotification theme="error">{err}</ScopedNotification>;
  } else if (providers.length === 0) {
    content = (
      <Card title={ss(embedded, "list")} icon={{ glyph: Globe, name: "connected-apps" }}>
        <EmptyState icon={KeyRound} title={ss(embedded, "empty")} description={ss(embedded, "emptyDesc")} />
      </Card>
    );
  } else {
    content = (
      <Card
        title={ss(embedded, "list")}
        icon={{ glyph: Globe, name: "connected-apps" }}
        count={providers.length}
        padded={false}
        footer={
          <div className="slds-text-align_left slds-text-body_small slds-text-color_weak">
            <p>{t("page.sso.enabledHint")}</p>
            <p>{t("page.sso.immutable")}</p>
          </div>
        }
      >
        <div className="overflow-x-auto">
          <table className="slds-table slds-table_cell-buffer slds-table_bordered" aria-label={t("page.sso.grid")}>
            <thead>
              <tr className="slds-line-height_reset">
                {(["provider", "status", "enabled", "trust"] as const).map((c) => (
                  <th key={c} scope="col">
                    <div className="slds-truncate" title={t(`page.sso.col.${c}`)}>{t(`page.sso.col.${c}`)}</div>
                  </th>
                ))}
                <th scope="col" className="slds-cell-shrink">
                  <span className="slds-assistive-text">{t("page.sso.col.actions")}</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {providers.map((p) => {
                const name = providerName(p);
                const rowBusy = busy?.id === p.id;
                return (
                  <tr key={p.id} className="slds-hint-parent">
                    <th scope="row" data-label={ss(embedded, "col.provider")}>
                      {/* max-w: a long client id truncates (full text on hover) instead of squeezing the other columns. */}
                      <div className="slds-truncate max-w-md" title={name}><strong>{name}</strong></div>
                      <div
                        className="slds-truncate slds-text-body_small slds-text-color_weak slds-text-font_monospace max-w-md"
                        title={tf("page.sso.client", { issuer: p.issuer, clientId: p.clientId })}
                      >
                        {tf("page.sso.client", { issuer: p.issuer, clientId: p.clientId })}
                      </div>
                    </th>
                    <td data-label={t("page.sso.col.status")}>
                      <div className="flex gap-1">
                        <Badge theme={p.enabled ? "success" : undefined}>
                          {t(p.enabled ? "page.sso.badge.enabled" : "page.sso.badge.disabled")}
                        </Badge>
                        {p.jitProvisioning && <Badge variant="lightest">{t("page.sso.badge.jit")}</Badge>}
                        {p.trustIdpMfa && <Badge theme="warning">{ss(embedded, "badge.trust")}</Badge>}
                      </div>
                    </td>
                    <td data-label={t("page.sso.col.enabled")}>
                      <Toggle
                        label={<span className="slds-assistive-text">{tf("page.sso.toggle.enabled", { name })}</span>}
                        checked={p.enabled}
                        disabled={rowBusy}
                        onChange={(e) => patch(p, { enabled: e.target.checked }, "enabled")}
                      />
                    </td>
                    <td data-label={ss(embedded, "col.trust")} className="slds-cell-wrap">
                      <Toggle
                        label={<span className="slds-assistive-text">{tf("page.sso.toggle.trust", { name })}</span>}
                        checked={p.trustIdpMfa}
                        disabled={rowBusy}
                        onChange={(e) => {
                          if (e.target.checked) setConfirmTrust(p); // bật → xác nhận trước, KHÔNG patch ngay
                          else patch(p, { trustIdpMfa: false }, "trustIdpMfa"); // tắt = an toàn hơn, áp lại ngay
                        }}
                      />
                      {/* Cảnh báo THƯỜNG TRỰC (không giấu sau hover/confirm) — setting này bỏ qua MFA cục bộ. */}
                      <p className="slds-text-body_small slds-m-top_xx-small flex gap-1">
                        <Icon icon={TriangleAlert} size="xx-small" tone="warning" />
                        <span>{TRUST_IDP_MFA_WARNING} {t("page.sso.trustBlanket")}</span>
                      </p>
                    </td>
                    <td>
                      <ButtonIcon
                        icon={Trash2}
                        variant="border-filled"
                        size="small"
                        label={ssf(embedded, "delete", { name })}
                        disabled={rowBusy}
                        onClick={() => setConfirmDelete(p)}
                      />
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </Card>
    );
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4">
      <PageHeader
        icon={{ glyph: Globe, name: "connected-apps" }}
        title={ss(embedded, "title")}
        meta={notConfigured ? t("page.sso.notConfiguredMeta") : ssf(embedded, "count", { n: providers.length })}
        actions={[
          <Button
            key="add"
            variant="brand"
            iconLeft={Plus}
            onClick={() => setCreateOpen(true)}
            disabled={notConfigured}
            title={notConfigured ? t("page.sso.addDisabled") : undefined}
          >
            {ss(embedded, "add")}
          </Button>,
        ]}
      />

      {content}

      <CreateProvider
        inline={embedded}
        open={createOpen}
        onClose={(created) => {
          setCreateOpen(false);
          if (created) reload();
        }}
      />

      {embedded && confirmTrust && (
        <InlineConfirm
          tone="warning"
          message={`${TRUST_IDP_MFA_WARNING} ${ssf(embedded, "trustConfirm.body", { name: providerName(confirmTrust) })}`}
          confirmLabel={t("page.sso.trustConfirm.ok")}
          busy={busy?.which === "trustIdpMfa"}
          onCancel={() => setConfirmTrust(null)}
          onConfirm={() => patch(confirmTrust, { trustIdpMfa: true }, "trustIdpMfa")}
        />
      )}
      {embedded && confirmDelete && (
        <InlineConfirm
          tone="error"
          message={`${ssf(embedded, "delete.title", { name: providerName(confirmDelete) })} ${tf("page.sso.delete.desc", { name: providerName(confirmDelete) })}`}
          confirmLabel={t("page.sso.delete.confirm")}
          busy={busy?.which === "delete"}
          onCancel={() => setConfirmDelete(null)}
          onConfirm={() => handleDelete(confirmDelete)}
        />
      )}

      <Prompt
        open={!embedded && confirmTrust !== null}
        onOpenChange={(o) => { if (!o && busy?.which !== "trustIdpMfa") setConfirmTrust(null); }}
        theme="warning"
        title={ss(embedded, "trustConfirm.title")}
        footer={
          <>
            <Button onClick={() => setConfirmTrust(null)} disabled={busy?.which === "trustIdpMfa"}>{t("action.cancel")}</Button>
            <Button
              variant="brand"
              disabled={busy?.which === "trustIdpMfa"}
              onClick={() => confirmTrust && patch(confirmTrust, { trustIdpMfa: true }, "trustIdpMfa")}
            >
              {t("page.sso.trustConfirm.ok")}
            </Button>
          </>
        }
      >
        <p>
          <strong>{TRUST_IDP_MFA_WARNING}</strong>{" "}
          {confirmTrust && ssf(embedded, "trustConfirm.body", { name: providerName(confirmTrust) })}
        </p>
      </Prompt>

      <ConfirmPrompt
        open={!embedded && confirmDelete !== null}
        onOpenChange={(o) => { if (!o) setConfirmDelete(null); }}
        title={confirmDelete ? ssf(embedded, "delete.title", { name: providerName(confirmDelete) }) : ""}
        description={confirmDelete ? tf("page.sso.delete.desc", { name: providerName(confirmDelete) }) : undefined}
        danger
        confirmLabel={t("page.sso.delete.confirm")}
        loading={busy?.which === "delete"}
        onConfirm={() => confirmDelete && handleDelete(confirmDelete)}
      />
    </div>
  );
}

// ── Tạo provider (modals/base + form-element) ──────────────────────────────────────────────────
function CreateProvider({ open, inline, onClose }: { open: boolean; inline?: boolean; onClose: (created: boolean) => void }) {
  const [issuer, setIssuer] = useState("");
  const [clientId, setClientId] = useState("");
  const [clientSecret, setClientSecret] = useState("");
  const [showSecret, setShowSecret] = useState(false);
  const [displayName, setDisplayName] = useState("");
  const [jit, setJit] = useState(false);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  // Client-side checks land on the field itself (form-element error); `err` is for server errors only.
  const [issuerErr, setIssuerErr] = useState<string | null>(null);
  const [clientIdErr, setClientIdErr] = useState<string | null>(null);
  const [secretErr, setSecretErr] = useState<string | null>(null);
  const issuerRef = useRef<HTMLInputElement>(null);
  const clientIdRef = useRef<HTMLInputElement>(null);
  const secretRef = useRef<HTMLInputElement>(null);

  // A fresh form every time the modal opens (state reset during render, not in an effect).
  const [wasOpen, setWasOpen] = useState(open);
  if (open !== wasOpen) {
    setWasOpen(open);
    if (open) {
      setIssuer(""); setClientId(""); setClientSecret(""); setShowSecret(false);
      setDisplayName(""); setJit(false); setErr(null); setIssuerErr(null); setClientIdErr(null); setSecretErr(null);
    }
  }

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const ie = validateIssuer(issuer);
    const ce = clientId.trim() ? null : t("page.sso.create.err.clientId");
    const se = clientSecret ? null : t("page.sso.create.err.secret");
    setIssuerErr(ie);
    setClientIdErr(ce);
    setSecretErr(se);
    if (ie || ce || se) {
      (ie ? issuerRef : ce ? clientIdRef : secretRef).current?.focus();
      return;
    }
    setSaving(true);
    setErr(null);
    try {
      await createSsoProvider({
        issuer: issuer.trim(),
        clientId: clientId.trim(),
        clientSecret,
        displayName: displayName.trim() || undefined,
        jitProvisioning: jit,
      });
      toast.success(ss(!!inline, "create.done"));
      onClose(true);
    } catch (ex) {
      if (ex instanceof ApiError && ex.code === "SSO_PROVIDER_ALREADY_EXISTS") {
        setErr(t("page.sso.create.err.exists"));
      } else if (ex instanceof ApiError && ex.code === "SSO_NOT_CONFIGURED") {
        setErr(t("page.sso.create.err.notConfigured"));
      } else {
        setErr(ex instanceof ApiError ? ex.message : t("page.sso.create.err.generic"));
      }
    } finally {
      setSaving(false);
    }
  }

  const form = (
      <form id="sso-create-form" onSubmit={handleSubmit} noValidate className="slds-form flex flex-col gap-3">
        {err && <ScopedNotification theme="error">{err}</ScopedNotification>}
        <Input
          ref={issuerRef}
          label={ss(!!inline, "create.issuer")}
          required
          value={issuer}
          onChange={(e) => { setIssuer(e.target.value); if (issuerErr) setIssuerErr(null); }}
          placeholder={t("page.sso.create.issuerPh")}
          inputMode="url"
          error={issuerErr ?? undefined}
          help={ss(!!inline, "create.issuerHelp")}
        />
        <Input
          ref={clientIdRef}
          label={t("page.sso.create.clientId")}
          required
          value={clientId}
          onChange={(e) => { setClientId(e.target.value); setClientIdErr(null); }}
          placeholder={t("page.sso.create.clientIdPh")}
          error={clientIdErr ?? undefined}
        />
        <div className="flex items-start gap-2">
          <div className="min-w-0 flex-1">
            <Input
              ref={secretRef}
              label={t("page.sso.create.secret")}
              required
              type={showSecret ? "text" : "password"}
              value={clientSecret}
              onChange={(e) => { setClientSecret(e.target.value); setSecretErr(null); }}
              autoComplete="new-password"
              error={secretErr ?? undefined}
              help={ss(!!inline, "create.secretHelp")}
            />
          </div>
          <ButtonIcon
            className="mt-6"
            variant="border-filled"
            icon={showSecret ? EyeOff : Eye}
            label={t(showSecret ? "page.sso.create.hideSecret" : "page.sso.create.showSecret")}
            onClick={() => setShowSecret((s) => !s)}
          />
        </div>
        <Input
          label={t("page.sso.create.displayName")}
          value={displayName}
          onChange={(e) => setDisplayName(e.target.value)}
          placeholder={t("page.sso.create.displayNamePh")}
        />
        <Checkbox
          label={t("page.sso.create.jit")}
          help={ss(!!inline, "create.jitHelp")}
          checked={jit}
          onChange={(e) => setJit(e.target.checked)}
        />
        <ScopedNotification>{ss(!!inline, "create.trustNote")}</ScopedNotification>
      </form>
  );
  const buttons = (
    <>
          <Button onClick={() => onClose(false)} disabled={saving}>{t("action.cancel")}</Button>
          <Button type="submit" form="sso-create-form" variant="brand" disabled={saving}>
            {saving ? t("action.saving") : ss(!!inline, "create.submit")}
          </Button>
    </>
  );
  if (inline) {
    if (!open) return null;
    return (
      <div className="slds-theme_shade slds-p-around_medium">
        <h2 className="slds-text-heading_small slds-m-bottom_x-small">{ss(!!inline, "create.title")}</h2>
        {form}
        <div className="slds-m-top_small slds-grid slds-grid_align-end">{buttons}</div>
      </div>
    );
  }
  return (
    <Modal
      open={open}
      onOpenChange={(o) => { if (!o && !saving) onClose(false); }}
      title={ss(!!inline, "create.title")}
      tagline={ss(!!inline, "create.tagline")}
      footer={buttons}
    >
      {form}
    </Modal>
  );
}
