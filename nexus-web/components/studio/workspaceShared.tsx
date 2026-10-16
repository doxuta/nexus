"use client";

// workspaceShared — pieces shared by VerticalLibrary ("Cài mẫu & Bàn giao") and the
// HandoverWizard ("Bàn giao app này", EASE Nước 4). Extracted VERBATIM from VerticalLibrary.tsx
// so the wizard reuses — never forks — the plan renderer, the install-result view, the
// create-tenant form (MS-B), the show-once credentials reveal and the install/delete confirm.
// One owner per widget.
// SLDS (specs/038 T045): data-tables/base (plan, install result) · badges · scoped-notifications ·
// cards + input/checkbox (create workspace, credentials) · prompt (confirm, type-the-name).

import { useLayoutEffect, useMemo, useRef, useState, type FormEvent, type ReactNode } from "react";
import Link from "next/link";
import type { ColumnDef } from "@tanstack/react-table";
import { ArrowRight, Building2, Check, Copy, Plus, ShieldAlert } from "lucide-react";
import clsx from "clsx";
import { slugifyAppName } from "@/lib/apps";
import { toast } from "@/components/ui/Toaster";
import { provisionTenant, ApiError, type ProvisionedTenant } from "@/lib/api";
import {
  planToDiffSections, partitionConflicts, planIsClean, installResultSummary,
  type InstallPlan, type InstallResult, type DiffTone, type SummaryLine,
} from "@/lib/verticalPlan";
import { errorMessage } from "@/lib/errorMessages";
import { t } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { workspaceTenantName } from "@/lib/workspace";
import { Badge } from "@/components/slds/Badge";
import { Button, buttonClassName } from "@/components/slds/Button";
import { Card } from "@/components/slds/Card";
import { Checkbox } from "@/components/slds/Checkbox";
import { DataTable, fill } from "@/components/slds/DataTable";
import { FormElement } from "@/components/slds/FormElement";
import { Icon } from "@/components/slds/Icon";
import { useEmbedded } from "@/components/slds/EmbeddedSurface";
import { Input } from "@/components/slds/Input";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { Prompt } from "@/components/slds/Modal";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";

// Soft client-side mirror of the BE's authoritative tenant-slug pattern
// (nexus-platform/internal/services/tenant_provision_service.go TenantSlugPattern) — used ONLY
// for an inline hint on the editable slug field; the server re-validates and is the source of truth.
export const TENANT_SLUG_RE = /^[a-z0-9][a-z0-9_-]{0,63}$/;

/**
 * Shown where the handover wizard would open while the tab builds INSIDE the customer's workspace
 * (specs/062 L2, FR-017): exporting X's pack to install it back into X is meaningless — packs only add.
 */
export function WorkspaceHandoverNote() {
  return <ScopedNotification>{fill(t("workspace.handover.note"), { name: workspaceTenantName() })}</ScopedNotification>;
}

/** Inline spinner + message for a region that is still loading. */
export function Loading({ children }: { children: string }) {
  return (
    <div className="flex items-center gap-2">
      <Spinner size="x-small" inline />
      <span className="slds-text-color_weak">{children}</span>
    </div>
  );
}

// ── Dry-run plan renderer ────────────────────────────────────────────────────────

const TONE: Record<DiffTone, { key: I18nKey; theme?: "success" }> = {
  create: { key: "page.packInstall.tone.create", theme: "success" },
  skip: { key: "page.packInstall.tone.skip" },
  info: { key: "page.packInstall.tone.info" },
};

interface PlanRow { id: string; group: string; label: string; tone: DiffTone; note?: string }

export function PlanView({ plan }: { plan: InstallPlan }) {
  const { hard, soft } = partitionConflicts(plan.conflicts);
  const clean = planIsClean(plan);
  const rows: PlanRow[] = planToDiffSections(plan).flatMap((sec) =>
    sec.rows.map((r, i) => ({ id: `${sec.key}:${i}`, group: sec.title, label: r.label, tone: r.tone, note: r.note })));
  const columns = useMemo<ColumnDef<PlanRow>[]>(() => [
    { id: "label", header: t("page.packInstall.plan.item"), accessorKey: "label" },
    { id: "group", header: t("page.packInstall.plan.group"), accessorKey: "group" },
    {
      id: "tone",
      header: t("page.packInstall.plan.change"),
      accessorFn: (r) => t(TONE[r.tone].key),
      cell: ({ row }) => <Badge theme={TONE[row.original.tone].theme}>{t(TONE[row.original.tone].key)}</Badge>,
    },
    { id: "note", header: t("page.packInstall.plan.note"), accessorFn: (r) => r.note ?? "" },
  ], []);

  const stats: [I18nKey, number][] = [
    ["page.packInstall.stat.create", plan.entitiesToCreate.length],
    ["page.packInstall.stat.skip", plan.entitiesToSkip.length],
    ["page.packInstall.stat.fields", plan.fieldsToAdd.length],
    ["page.packInstall.stat.layouts", plan.layoutsToInstall.length],
    ["page.packInstall.stat.samples", plan.samplesCount],
  ];

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap gap-2">
        {stats.map(([key, n]) => (
          <Badge key={key} variant="lightest">
            <span className="tabular-nums">{fill(t(key), { n })}</span>
          </Badge>
        ))}
      </div>

      {clean ? (
        <ScopedNotification theme="success">
          <strong>{t("page.packInstall.clean")}</strong> — {t("page.packInstall.cleanHint")}
        </ScopedNotification>
      ) : (
        <div className="flex flex-col gap-2">
          <ScopedNotification theme={hard.length ? "error" : "warning"}>
            <strong>{fill(t("page.packInstall.conflicts"), { n: plan.conflicts.length })}</strong>
          </ScopedNotification>
          <ul className="slds-has-dividers_bottom-space">
            {[...hard, ...soft].map((c, i) => <ConflictRow key={i} kind={c.kind} detail={c.detail} entity={c.entity} field={c.field} />)}
          </ul>
        </div>
      )}

      {/* Install notes the dry run predicts (queue #104) — the same sentences the install result
          will print, shown BEFORE the operator clicks Cài. They never gate the confirm. */}
      {plan.warnings && plan.warnings.length > 0 && (
        <div className="flex flex-col gap-1" data-testid="plan-warnings">
          <ScopedNotification theme="warning">
            <strong>{t("page.packInstall.result.warnings")}</strong>
          </ScopedNotification>
          <ul className="slds-list_dotted">
            {plan.warnings.map((w) => <li key={w}>{w}</li>)}
          </ul>
        </div>
      )}

      {rows.length > 0 && (
        <DataTable label={t("page.packInstall.plan.label")} columns={columns} data={rows} getRowId={(r) => r.id} maxHeight="20rem" bordered />
      )}
    </div>
  );
}

function ConflictRow({ kind, detail, entity, field }: { kind: string; detail: string; entity?: string; field?: string }) {
  const hard = kind === "unique_slot_held";
  const label = t(hard ? "page.packInstall.conflict.hard" : kind === "layout_overwrite" ? "page.packInstall.conflict.layout" : kind === "upgrade_available" ? "page.packInstall.conflict.upgrade" : "page.packInstall.conflict.other");
  return (
    <li className="slds-item">
      <div className="slds-media slds-media_small">
        <div className="slds-media__figure">
          <Icon icon={ShieldAlert} size="x-small" tone={hard ? "error" : "warning"} />
        </div>
        <div className="slds-media__body">
          <p className={hard ? "slds-text-color_error" : undefined}>
            <strong>{label}</strong>
            {(entity || field) && <span className="slds-text-color_weak"> {entity}{field ? `.${field}` : ""}</span>}
          </p>
          <p className="slds-text-body_small slds-text-color_weak">{detail}</p>
        </div>
      </div>
    </li>
  );
}

/** One line of a confirm's "what this touches" list — tone marks how risky the line is. */
export interface ImpactRow {
  label: string;
  tone?: "neutral" | "warn" | "danger";
}

/** Build the confirm impact rows from a plan's conflicts (heads-up list before install). */
export function conflictImpactRows(plan: InstallPlan): ImpactRow[] {
  return plan.conflicts.map((c): ImpactRow => {
    const hard = c.kind === "unique_slot_held";
    const where = c.entity ? ` (${c.entity}${c.field ? `.${c.field}` : ""})` : "";
    return {
      label: fill(t(hard ? "page.packInstall.impact.unique" : "page.packInstall.impact.layout"), { where }),
      tone: hard ? "danger" : "warn",
    };
  });
}

// ── Confirm (SLDS prompt) — simple, or type-the-name when `confirmName` is set ─────────────────
// Replaces the Nexus-Be ConfirmDialog on these screens. The typed name resets every time the
// prompt opens, so a previous unlock never carries over.
type ConfirmPromptProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description?: ReactNode;
  impact?: ImpactRow[];
  /** Must be typed exactly (case-sensitive) before the confirm button unlocks. */
  confirmName?: string;
  danger?: boolean;
  confirmLabel: string;
  loading?: boolean;
  onConfirm: () => void;
  /** Focus target on close when the confirmed action removed the button that opened the prompt. */
  returnFocus?: () => HTMLElement | null | undefined;
};

// The typed name resets every time the prompt opens, so a previous unlock never carries over.
function useTypedName(open: boolean, confirmName?: string) {
  const [typed, setTyped] = useState("");
  const [wasOpen, setWasOpen] = useState(open);
  if (open !== wasOpen) {
    setWasOpen(open);
    if (open) setTyped("");
  }
  return { typed, setTyped, locked: !!confirmName && typed !== confirmName };
}

function ConfirmBody({ description, impact, confirmName, typed, setTyped, loading }: Pick<ConfirmPromptProps, "description" | "impact" | "confirmName" | "loading"> & { typed: string; setTyped: (v: string) => void }) {
  return (
    <div className="flex flex-col gap-3">
      {description && <div>{description}</div>}
      {impact && impact.length > 0 && (
        <ul className="slds-list_dotted">
          {impact.map((row, i) => (
            <li key={i} className={clsx(row.tone === "danger" && "slds-text-color_error")}>
              {row.tone === "warn" ? <strong>{row.label}</strong> : row.label}
            </li>
          ))}
        </ul>
      )}
      {confirmName && (
        <Input
          label={fill(t("page.packInstall.confirmType"), { name: confirmName })}
          value={typed}
          placeholder={confirmName}
          autoComplete="off"
          spellCheck={false}
          disabled={loading}
          onChange={(e) => setTyped(e.target.value)}
        />
      )}
    </div>
  );
}

// Inside the Studio frame (D-UI-28) the question is drawn in the page where it was asked, never as a dialog.
function EmbeddedConfirm({ open, onOpenChange, title, description, impact, confirmName, danger = false, confirmLabel, loading = false, onConfirm, returnFocus }: ConfirmPromptProps) {
  const { typed, setTyped, locked } = useTypedName(open, confirmName);
  const box = useRef<HTMLDivElement>(null);
  const opener = useRef<HTMLElement | null>(null);
  // Layout effect: runs before InlineConfirm's passive effect moves focus onto its safe button.
  useLayoutEffect(() => {
    if (open) {
      opener.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
      box.current?.scrollIntoView({ block: "nearest" });
      return;
    }
    const target = returnFocus?.() ?? opener.current;
    if (target?.isConnected) target.focus();
    opener.current = null;
    // returnFocus is a fresh closure each render; only the open edge matters.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);
  if (!open) return null;
  return (
    <div ref={box}>
      <InlineConfirm
        tone={danger ? "error" : "warning"}
        message={title}
        confirmLabel={confirmLabel}
        busy={loading}
        confirmDisabled={locked}
        onConfirm={onConfirm}
        onCancel={() => onOpenChange(false)}
      >
        <ConfirmBody description={description} impact={impact} confirmName={confirmName} typed={typed} setTyped={setTyped} loading={loading} />
      </InlineConfirm>
    </div>
  );
}

function ConfirmPromptModal({ open, onOpenChange, title, description, impact, confirmName, danger = false, confirmLabel, loading = false, onConfirm, returnFocus }: ConfirmPromptProps) {
  const { typed, setTyped, locked } = useTypedName(open, confirmName);
  const close = (o: boolean) => { if (!loading) onOpenChange(o); };
  return (
    <Prompt
      open={open}
      onOpenChange={close}
      returnFocus={returnFocus}
      theme={danger ? "error" : "info"}
      title={title}
      footer={
        <>
          <Button onClick={() => close(false)} disabled={loading}>{t("action.cancel")}</Button>
          <Button variant={danger ? "destructive" : "brand"} onClick={onConfirm} disabled={loading || locked}>
            {confirmLabel}
          </Button>
        </>
      }
    >
      <ConfirmBody description={description} impact={impact} confirmName={confirmName} typed={typed} setTyped={setTyped} loading={loading} />
    </Prompt>
  );
}

export function ConfirmPrompt(props: ConfirmPromptProps) {
  return useEmbedded() ? <EmbeddedConfirm {...props} /> : <ConfirmPromptModal {...props} />;
}

// ── Install result (ported from the wizard's StepDone) ─────────────────────────────
// `subtitle` / `actions` are optional overrides for the handover wizard: a cross-tenant apply
// must name the TARGET workspace and must NOT offer "Xem như khách" (that link opens the
// CURRENT tenant's app face — a lie after installing elsewhere). Defaults keep the
// VerticalLibrary same-tenant behavior byte-identical.
export function InstallResultView({
  result, onClose, subtitle, actions, closeDisabled, closeDisabledHint,
}: {
  result: InstallResult;
  onClose: () => void;
  subtitle?: string;
  actions?: ReactNode;
  /* MỘT XƯỞNG RỔ-4 P1: the handover wizard blocks Đóng until the one-time credentials are
     acknowledged as saved. Default undefined = VerticalLibrary behaviour byte-identical. */
  closeDisabled?: boolean;
  closeDisabledHint?: string;
}) {
  const summary = installResultSummary(result);
  const columns = useMemo<ColumnDef<SummaryLine>[]>(() => [
    { id: "label", header: t("page.packInstall.result.part"), accessorKey: "label" },
    {
      id: "count",
      header: t("page.packInstall.result.count"),
      accessorFn: (l) => l.count.toLocaleString("vi-VN"),
      meta: { numeric: true },
    },
  ], []);
  return (
    <div className="flex flex-col gap-3">
      <ScopedNotification theme="success">
        <strong>{t("page.packInstall.done")}</strong> — {subtitle ?? t("page.packInstall.doneHere")}
      </ScopedNotification>

      {/* Fail-open install notes (e.g. provenance skipped, no default business calendar) — printed
          verbatim, never silent (queue #106: the install screen keeps showing result.warnings). */}
      {result.warnings && result.warnings.length > 0 && (
        <div className="flex flex-col gap-1" data-testid="install-warnings">
          <ScopedNotification theme="warning">
            <strong>{t("page.packInstall.result.warnings")}</strong>
          </ScopedNotification>
          <ul className="slds-list_dotted">
            {result.warnings.map((w) => <li key={w}>{w}</li>)}
          </ul>
        </div>
      )}

      {summary.length > 0 ? (
        <DataTable label={t("page.packInstall.result.label")} columns={columns} data={summary} getRowId={(l) => l.label} bordered />
      ) : (
        <ScopedNotification>{t("page.packInstall.result.none")}</ScopedNotification>
      )}

      <div className="flex flex-wrap items-center gap-2">
        {actions ?? (
          /* The pack may have minted a sidebar app — the customer face shows it live. */
          <Link href="/home" className={buttonClassName("brand")}>
            {t("page.packInstall.viewAsCustomer")}
            <ArrowRight aria-hidden="true" className="slds-button__icon slds-button__icon_right nx-lucide" />
          </Link>
        )}
        <Button onClick={onClose} disabled={closeDisabled} title={closeDisabled ? closeDisabledHint : undefined}>
          {t("action.close")}
        </Button>
      </div>
      {closeDisabled && closeDisabledHint && (
        <p className="slds-text-color_error slds-text-body_small">{closeDisabledHint}</p>
      )}
    </div>
  );
}

// ── Create tenant form — inline, name auto-derives an editable slug (CreateAppDialog recipe). ──
export function CreateTenantForm({
  onCancel, onCreated,
}: {
  onCancel: () => void;
  onCreated: (res: ProvisionedTenant) => void;
}) {
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [slugTouched, setSlugTouched] = useState(false);
  const [adminEmail, setAdminEmail] = useState("");
  const [adminFullName, setAdminFullName] = useState("");
  const [generatePw, setGeneratePw] = useState(true);
  const [adminPassword, setAdminPassword] = useState("");
  const [saving, setSaving] = useState(false);
  const [fieldErr, setFieldErr] = useState<{ slug?: string; adminEmail?: string; name?: string; adminPassword?: string }>({});
  const [formErr, setFormErr] = useState<string | null>(null);

  function onNameChange(v: string) {
    setName(v);
    if (!slugTouched) setSlug(slugifyAppName(v));
    setFieldErr((e) => ({ ...e, name: undefined }));
  }

  const slugValid = slug !== "" && TENANT_SLUG_RE.test(slug);
  const canSubmit =
    name.trim() !== "" && slugValid && adminEmail.trim() !== "" &&
    (generatePw || adminPassword !== "") && !saving;

  function mapError(e: unknown) {
    if (e instanceof ApiError) {
      switch (e.code) {
        case "TENANT_SLUG_INVALID":
          setFieldErr((f) => ({ ...f, slug: e.message }));
          return;
        case "TENANT_FIELD_REQUIRED": {
          const field = e.details.field;
          if (field === "slug") setFieldErr((f) => ({ ...f, slug: t("page.packInstall.tenant.required") }));
          else if (field === "adminEmail") setFieldErr((f) => ({ ...f, adminEmail: t("page.packInstall.tenant.required") }));
          else setFormErr(e.message);
          return;
        }
        case "TENANT_ALREADY_EXISTS":
          setFieldErr((f) => ({ ...f, slug: t("page.packInstall.tenant.slugTaken") }));
          return;
        case "TENANT_NAME_TOO_LONG":
          setFieldErr((f) => ({ ...f, name: e.message }));
          return;
        case "TENANT_PASSWORD_CONFLICT":
          setFormErr(t("page.packInstall.tenant.passwordConflict"));
          return;
        case "PASSWORD_TOO_WEAK":
          setFieldErr((f) => ({ ...f, adminPassword: e.message }));
          return;
        default:
          setFormErr(errorMessage(e));
          return;
      }
    }
    setFormErr(errorMessage(e));
  }

  async function submit(ev: FormEvent) {
    ev.preventDefault();
    if (!canSubmit) return;
    setSaving(true);
    setFormErr(null);
    try {
      const res = await provisionTenant({
        slug,
        name: name.trim(),
        adminEmail: adminEmail.trim(),
        ...(adminFullName.trim() ? { adminFullName: adminFullName.trim() } : {}),
        ...(generatePw ? { generatePassword: true } : { adminPassword }),
      });
      onCreated(res);
    } catch (e) {
      mapError(e);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Card title={t("page.packInstall.tenant.title")} icon={{ glyph: Building2, name: "customer-workspace" }} boundary>
      <form onSubmit={submit} className="flex max-w-md flex-col gap-3" noValidate>
        {formErr && <ScopedNotification theme="error">{formErr}</ScopedNotification>}
        <Input
          label={t("page.packInstall.tenant.name")}
          required
          value={name}
          onChange={(e) => onNameChange(e.target.value)}
          disabled={saving}
          maxLength={150}
          placeholder={t("page.packInstall.tenant.namePh")}
          error={fieldErr.name}
        />
        <Input
          label={t("page.packInstall.tenant.slug")}
          required
          value={slug}
          onChange={(e) => { setSlugTouched(true); setSlug(e.target.value.toLowerCase()); setFieldErr((f) => ({ ...f, slug: undefined })); }}
          disabled={saving}
          spellCheck={false}
          autoComplete="off"
          placeholder={t("page.packInstall.tenant.slugPh")}
          error={fieldErr.slug ?? (slug !== "" && !slugValid ? t("page.packInstall.tenant.slugInvalid") : undefined)}
          help={t("page.packInstall.tenant.slugHelp")}
        />
        <Input
          label={t("page.packInstall.tenant.email")}
          required
          type="email"
          value={adminEmail}
          onChange={(e) => { setAdminEmail(e.target.value); setFieldErr((f) => ({ ...f, adminEmail: undefined })); }}
          disabled={saving}
          placeholder={t("page.packInstall.tenant.emailPh")}
          error={fieldErr.adminEmail}
        />
        <Input
          label={t("page.packInstall.tenant.fullName")}
          value={adminFullName}
          onChange={(e) => setAdminFullName(e.target.value)}
          disabled={saving}
          maxLength={150}
          placeholder={t("page.packInstall.tenant.fullNamePh")}
        />
        <Checkbox
          label={t("page.packInstall.tenant.generate")}
          help={t("page.packInstall.tenant.generateHelp")}
          checked={generatePw}
          onChange={(e) => setGeneratePw(e.target.checked)}
          disabled={saving}
        />
        {!generatePw && (
          <Input
            label={t("page.packInstall.tenant.password")}
            required
            type="password"
            value={adminPassword}
            onChange={(e) => { setAdminPassword(e.target.value); setFieldErr((f) => ({ ...f, adminPassword: undefined })); }}
            disabled={saving}
            autoComplete="new-password"
            error={fieldErr.adminPassword}
            help={t("page.packInstall.tenant.passwordHelp")}
          />
        )}
        <div className="flex gap-2">
          <Button onClick={onCancel} disabled={saving}>{t("action.cancel")}</Button>
          <Button variant="brand" type="submit" iconLeft={Plus} disabled={!canSubmit}>
            {saving ? t("page.packInstall.tenant.creating") : t("page.packInstall.tenant.create")}
          </Button>
        </div>
      </form>
    </Card>
  );
}

// ── Show-once credentials handover — mirrors ApiKeys.tsx's SecretReveal pattern exactly:
// copy per line + a block-copy button + a bold non-recoverable warning + a mandatory
// acknowledgement checkbox gating "Xong". Plaintext lives ONLY in this component's React state —
// never sessionStorage/localStorage — so leaving this screen is genuinely, not just cosmetically, gone.
export function TenantCredentialsReveal({ result, onDone }: { result: ProvisionedTenant; onDone: () => void }) {
  const [ackChecked, setAckChecked] = useState(false);
  const [copiedKey, setCopiedKey] = useState<string | null>(null);
  const [copiedBlock, setCopiedBlock] = useState(false);
  const password = result.oneTimeCredentials?.password ?? "";
  const loginUrl = typeof window !== "undefined" ? `${window.location.origin}/login` : "/login";

  const rows: Array<{ key: string; label: string; value: string }> = [
    { key: "url", label: t("page.packInstall.creds.url"), value: loginUrl },
    { key: "slug", label: t("page.packInstall.creds.tenant"), value: result.tenant.slug },
    { key: "email", label: t("page.packInstall.creds.email"), value: result.admin.email },
    { key: "password", label: t("page.packInstall.creds.password"), value: password },
  ];
  const block = rows.map((r) => `${r.label}: ${r.value}`).join("\n");

  async function copyOne(key: string, value: string) {
    try {
      await navigator.clipboard.writeText(value);
      setCopiedKey(key);
      setTimeout(() => setCopiedKey((k) => (k === key ? null : k)), 2000);
    } catch {
      toast.error(t("page.packInstall.creds.copyFailOne"));
    }
  }
  async function copyBlock() {
    try {
      await navigator.clipboard.writeText(block);
      setCopiedBlock(true);
      setTimeout(() => setCopiedBlock(false), 2000);
    } catch {
      toast.error(t("page.packInstall.creds.copyFailBlock"));
    }
  }

  return (
    <Card title={t("page.packInstall.creds.title")} icon={{ glyph: ShieldAlert, name: "password" }} boundary>
      <div className="flex flex-col gap-3">
        <ScopedNotification theme="warning">
          <strong>{t("page.packInstall.creds.once")}</strong>
        </ScopedNotification>
        {rows.map((r) => (
          <div key={r.key} className="flex items-end gap-2">
            <FormElement label={r.label} readOnly="static" className="min-w-0 flex-1">
              <span className="break-all" data-testid={r.key === "password" ? "tenant-password" : undefined}>
                {r.key === "password" ? <strong>{r.value}</strong> : r.value}
              </span>
            </FormElement>
            <Button
              iconLeft={copiedKey === r.key ? Check : Copy}
              onClick={() => copyOne(r.key, r.value)}
              aria-label={fill(t("page.packInstall.creds.copyOne"), { label: r.label })}
            >
              {copiedKey === r.key ? t("page.packInstall.creds.copied") : t("page.packInstall.creds.copy")}
            </Button>
          </div>
        ))}
        <div>
          <Button iconLeft={copiedBlock ? Check : Copy} onClick={copyBlock}>
            {copiedBlock ? t("page.packInstall.creds.copiedBlock") : t("page.packInstall.creds.copyBlock")}
          </Button>
        </div>
        {/* specs/065 D2 + FR-042: say what this account is and what happens when it is used. */}
        <p>{t("page.packInstall.creds.owner")}</p>
        <p>{t("page.packInstall.creds.ownerAudit")}</p>
        <p className="slds-text-body_small slds-text-color_weak">{t("page.packInstall.creds.bcrypt")}</p>
        <Checkbox
          label={t("page.packInstall.creds.ack")}
          checked={ackChecked}
          onChange={(e) => setAckChecked(e.target.checked)}
        />
        <div>
          <Button variant="brand" onClick={onDone} disabled={!ackChecked}>{t("page.packInstall.creds.done")}</Button>
        </div>
      </div>
    </Card>
  );
}
