"use client";

// HandoverWizard — "Bàn giao app này" (EASE Nước 4, ../specs/027-handover-wizard/spec.md §4).
// ONE flow from AppOverview closing the factory's B7 dead-end: export a pack scoped to THIS
// app's nav → pick/create the customer workspace (operator-gated MS-B) → dry-run against the
// TARGET tenant (plan) → real cross-tenant install (apply, MS-C ?targetTenant=) → handover
// summary with the show-once credentials when a workspace was just created.
// Reuses — never forks — PlanView / InstallResultView / CreateTenantForm /
// TenantCredentialsReveal / ConfirmPrompt from workspaceShared.tsx; pure step gating lives in
// lib/handoverWizard.ts (unit-tested). Graceful degrade: an older backend that doesn't know
// ?targetTenant= answers 400/404 → the honest "chưa hỗ trợ" note, never a silent same-tenant
// install (explainTargetError, feature-detect — no version sniffing).
// SLDS (specs/038 T045): modals/base (directional wizard footer carrying progress-indicator/base
// on the shaded footer) · checkbox group (entities) · data-tables/base with the radio column of
// data-tables/advanced (target workspace) · scoped-notifications · prompt (type-the-slug confirm).

import { Fragment, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { ArrowRight, Check, Copy, Download, ExternalLink, Package, Plus } from "lucide-react";
import { toast } from "@/components/ui/Toaster";
import {
  adminApp, exportVertical, entityDependencies, listEntities, listTenants, installPack, ApiError,
  type AdminApp, type EntityDependency, type EntityMeta, type ProvisionedTenant, type TenantSummary,
} from "@/lib/api";
import type { InstallPlan, InstallResult, Pack } from "@/lib/verticalPlan";
import { requiresTypeName } from "@/lib/verticalPlan";
import {
  HANDOVER_STEPS, canAdvance, applySelection, preselectEntities, packFileName, explainTargetError,
  type TargetErrorInfo,
} from "@/lib/handoverWizard";
import {
  PlanView, InstallResultView, conflictImpactRows, CreateTenantForm, TenantCredentialsReveal, ConfirmPrompt, Loading,
} from "@/components/studio/workspaceShared";
import { errorMessage } from "@/lib/errorMessages";
import { qk } from "@/lib/query/keys";
import { t } from "@/lib/i18n";
import { Badge } from "@/components/slds/Badge";
import { Button, buttonClassName } from "@/components/slds/Button";
import { Checkbox } from "@/components/slds/Checkbox";
import { DataTable, fill } from "@/components/slds/DataTable";
import { Modal } from "@/components/slds/Modal";
import { ProgressIndicator } from "@/components/slds/ProgressIndicator";
import { ScopedNotification } from "@/components/slds/ScopedNotification";

type OperatorProbe = "loading" | "yes" | "no";

export function HandoverWizard({
  app, navEntityNames, onOpenChange,
}: {
  app: AdminApp;
  /** Entity slugs of the app's nav (AppOverview already resolves them) — the export preselection. */
  navEntityNames: string[];
  /** Mount-per-open: the parent unmounts the wizard to close it, so state always starts fresh. */
  onOpenChange: (open: boolean) => void;
}) {
  const qc = useQueryClient();
  const [step, setStep] = useState(0);

  // ── Step 1 — XUẤT ──
  const [entities, setEntities] = useState<EntityMeta[] | null>(null);
  const [entitiesErr, setEntitiesErr] = useState<string | null>(null);
  // null = untouched → derive the nav preselection at render time (no state-sync effect).
  const [selected, setSelected] = useState<string[] | null>(null);
  const [includeData, setIncludeData] = useState(false);
  const [exporting, setExporting] = useState(false);
  const [exportErr, setExportErr] = useState<string | null>(null);
  const [exportWarnings, setExportWarnings] = useState<string[]>([]);
  const [pack, setPack] = useState<Pack | null>(null);

  // ── Step 2 — WORKSPACE ──
  const [operator, setOperator] = useState<OperatorProbe>("loading");
  const [operatorForbidden, setOperatorForbidden] = useState(true);
  const [tenants, setTenants] = useState<TenantSummary[]>([]);
  const [creating, setCreating] = useState(false);
  const [provisioned, setProvisioned] = useState<ProvisionedTenant | null>(null);
  const [revealOpen, setRevealOpen] = useState(false);
  // MỘT XƯỞNG RỔ-4 P1: mật khẩu admin khách chỉ sống trong bộ nhớ wizard — đóng là mất
  // VĨNH VIỄN và workspace mới có đúng một admin không ai đăng nhập được. Nút 'Copy khối
  // bàn giao' trước đây là TUỲ CHỌN và đường đi tiếp KHÔNG bị chặn, trong khi màn tạo
  // workspace ở CÙNG wizard này đã có checkbox-gate. Đảo lại: hành động không-đảo-ngược
  // phải là hành động bị chặn.
  const [credsSaved, setCredsSaved] = useState(false);
  const [targetSlug, setTargetSlug] = useState<string | null>(null);

  // ── Step 3 — PLAN (dry-run vs TARGET) ──
  const [plan, setPlan] = useState<InstallPlan | null>(null);
  const [planLoading, setPlanLoading] = useState(false);
  const [planErr, setPlanErr] = useState<TargetErrorInfo | null>(null);

  // ── Step 4 — APPLY ──
  const [confirmOpen, setConfirmOpen] = useState(false);
  // Readiness of the app being handed over. The wizard never consulted it, so an app with NO
  // non-admin view role handed over "cleanly" — and the one warning that does fire (a user-targeted
  // approver being dropped) reads like a security win. The customer then opens their new workspace
  // and every employee sees nothing, which is indistinguishable from a broken delivery.
  // useEffect + useState, matching every other fetch in this file.
  const [noStaffRole, setNoStaffRole] = useState(false);
  useEffect(() => {
    let cancelled = false;
    adminApp(app.id)
      .then((d) => { if (!cancelled) setNoStaffRole(d.readiness?.hasNonAdminViewRole === false); })
      // Best-effort: a readiness we could not read must not block a handover, and must not claim
      // the app is fine either — it simply does not raise the gate.
      .catch(() => {});
    return () => { cancelled = true; };
  }, [app.id]);
  const [installing, setInstalling] = useState(false);
  const [installErr, setInstallErr] = useState<TargetErrorInfo | null>(null);
  const [result, setResult] = useState<InstallResult | null>(null);

  // Boot: entity list (for the entity picker) + operator probe, in parallel. The probe uses the
  // SAME GET /admin/tenants WorkspaceKhachSection uses: 200 → operator; FORBIDDEN → honest
  // escalation; any other error must NOT claim "no permission" (retry-able note instead).
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const ents = await listEntities();
        if (!cancelled) setEntities(ents);
      } catch (e) {
        if (!cancelled) setEntitiesErr(errorMessage(e));
      }
    })();
    return () => { cancelled = true; };
  }, []);

  // All state writes happen AFTER the await (mount state is already "loading"); the retry
  // button resets to "loading" in its own event handler — keeps this effect sync-write-free.
  const probeOperator = useCallback(async () => {
    try {
      const list = await listTenants();
      // A retired workspace refuses every login and every inbound signal, so installing a pack
      // into one hands the customer something dead. The registry lists them (purge needs them);
      // a deploy TARGET selector must not.
      setTenants(list.filter((tn) => !tn.retiredAt));
      setOperator("yes");
    } catch (e) {
      setOperatorForbidden(e instanceof ApiError && e.code === "FORBIDDEN");
      setOperator("no");
    }
  }, []);
  useEffect(() => { (async () => { await probeOperator(); })(); }, [probeOperator]);
  const retryProbe = useCallback(() => { setOperator("loading"); void probeOperator(); }, [probeOperator]);

  // Effective selection: untouched → the app's nav entities (orphan-safe), else the user's edit.
  // Queue #296: the menu is not the app — child/lookup tables a field needs ride along, pre-ticked.
  const [deps, setDeps] = useState<EntityDependency[]>([]);
  const navKey = navEntityNames.join(",");
  useEffect(() => {
    let cancelled = false;
    entityDependencies(navKey ? navKey.split(",") : [])
      .then((d) => { if (!cancelled) setDeps(d); })
      // Best-effort: without it the wizard falls back to the menu tables and the export warns.
      .catch(() => {});
    return () => { cancelled = true; };
  }, [navKey]);
  const effSelected = selected ?? (entities ? preselectEntities([...navEntityNames, ...deps.map((d) => d.name)], entities) : []);

  // Any change upstream invalidates everything derived downstream — an exported pack must
  // always match the entity selection, and a plan must always match (pack, target).
  // planRunId makes any IN-FLIGHT dry-run stale too: without it, "pick acme → next → back →
  // pick beta → next" could render acme's late-resolving plan under beta's name.
  const planRunId = useRef(0);
  function invalidatePack() {
    setPack(null); setExportWarnings([]); setExportErr(null);
    invalidatePlan();
  }
  function invalidatePlan() {
    planRunId.current++;
    setPlan(null); setPlanErr(null); setPlanLoading(false); setInstallErr(null);
  }

  async function runExport() {
    if (effSelected.length === 0) return;
    setExporting(true); setExportErr(null); setExportWarnings([]);
    try {
      // Carry the app identity so installing the pack mints THIS app in the customer's sidebar.
      const { pack: p, warnings } = await exportVertical(effSelected, includeData, {
        slug: app.name,
        name: app.label,
        icon: app.icon ?? undefined,
        description: app.description ?? undefined,
      });
      setPack(p);
      setExportWarnings(warnings);
      invalidatePlan();
    } catch (e) {
      setExportErr(errorMessage(e));
    } finally {
      setExporting(false);
    }
  }

  // "Tải bản sao" — optional archive copy; the happy path never requires a file (spec §4.2).
  function downloadCopy() {
    if (!pack) return;
    const blob = new Blob([JSON.stringify(pack, null, 2)], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = packFileName(app.name, includeData);
    a.click();
    URL.revokeObjectURL(url);
  }

  // Dry-run against the TARGET — kicked off by the ONLY way into the plan step (the footer
  // "Tiếp tục" from workspace), re-run via the retry button.
  const runPlan = useCallback(async () => {
    if (!pack || !targetSlug) return;
    const id = ++planRunId.current;
    setPlanLoading(true); setPlanErr(null); setPlan(null);
    try {
      const p = await installPack({ pack }, { dryRun: true, targetTenant: targetSlug });
      if (planRunId.current === id) setPlan(p);
    } catch (e) {
      if (planRunId.current === id) setPlanErr(explainTargetError(e));
    } finally {
      if (planRunId.current === id) setPlanLoading(false);
    }
  }, [pack, targetSlug]);

  function goNext() {
    // Entering the plan step (re)runs the dry-run unless a valid plan for the current
    // (pack, target) is already loaded — upstream edits invalidate it (invalidatePlan).
    if (step === 1 && !plan && !planLoading) void runPlan();
    setStep(step + 1);
  }

  async function doInstall() {
    if (!pack || !targetSlug) return;
    setInstalling(true); setInstallErr(null);
    try {
      const r = await installPack({ pack }, { targetTenant: targetSlug });
      setResult(r);
      setConfirmOpen(false);
    } catch (e) {
      setInstallErr(explainTargetError(e));
      setConfirmOpen(false);
    } finally {
      setInstalling(false);
      // The target list includes THIS workspace, where the pack mints a sidebar app the 5-minute
      // catalog would otherwise never show (VerticalLibrary's install does the same). Always, not
      // only when targetSlug is ours: an SSO session never learns its own slug (lib/api
      // tenantSlug), and for another workspace it is one cheap re-read.
      void qc.invalidateQueries({ queryKey: qk.apps() });
      void qc.invalidateQueries({ queryKey: qk.adminApps() });
    }
  }

  const gate = { hasPack: !!pack, targetSlug, planReady: !!plan && !planErr && !planLoading };
  const nextOk = canAdvance(step, gate);
  // Credentials of a JUST-created workspace, still in wizard memory (never storage) — recapped
  // on the final screen when the apply targeted that same workspace (spec §4.5).
  const freshCreds =
    provisioned && provisioned.oneTimeCredentials && provisioned.tenant.slug === targetSlug
      ? provisioned
      : null;
  const credsUnsaved = !!result && !!freshCreds && !credsSaved;
  // Close (✕ / Esc) is blocked while the real install is writing into the customer workspace —
  // closing mid-write would silently drop the result screen — and while a just-created
  // workspace's one-time password sits unacknowledged (the reveal, or the final recap): closing
  // then destroys the only copy (RỔ-4 P1). Install stays idempotent either way.
  const closeLocked = installing || revealOpen || credsUnsaved;
  const typeName = !!plan && (requiresTypeName(plan) || noStaffRole);

  const footer = !(step === 3 && result) && (
    // Directional wizard footer: back on the left, the shaded progress indicator in the middle,
    // the forward action on the right (progress-indicator docs, "in a modal").
    <div className="flex items-center justify-between gap-4">
      {/* Same lock as ✕/Esc: "Quay lại" off an un-acknowledged reveal would reach step 1, whose
          "Đóng" unmounts the wizard and the only copy of the password (RỔ-4 P1). */}
      <Button onClick={() => (step === 0 ? onOpenChange(false) : setStep(step - 1))} disabled={closeLocked}>
        {step === 0 ? t("action.close") : t("action.back")}
      </Button>
      <ProgressIndicator
        shade
        className="min-w-0 max-w-xs flex-1"
        label={t("page.handover.progress")}
        steps={HANDOVER_STEPS.map((s) => ({ id: s.key, label: t(s.label) }))}
        current={step}
      />
      {step < 3 ? (
        <Button
          variant="brand"
          onClick={goNext}
          // Also locked while the create-form / show-once reveal owns the workspace step —
          // "Tiếp tục" must never skip past an un-acknowledged credentials reveal.
          disabled={!nextOk || (step === 1 && (creating || revealOpen))}
          iconRight={ArrowRight}
        >
          {t("page.handover.next")}
        </Button>
      ) : (
        <Button
          variant="brand"
          onClick={() => setConfirmOpen(true)}
          disabled={!pack || !targetSlug || installing}
          iconLeft={Package}
        >
          {installing ? t("page.handover.installing") : fill(t("page.handover.installInto"), { slug: targetSlug ?? "" })}
        </Button>
      )}
    </div>
  );

  return (
    <Modal
      open
      onOpenChange={(o) => { if (!closeLocked) onOpenChange(o); }}
      size="medium"
      title={fill(t("page.handover.title"), { label: app.label })}
      tagline={t("page.handover.tagline")}
      footer={footer || undefined}
    >
      <div className="flex flex-col gap-4">
        {!(step === 3 && result) && (
          <h2 className="slds-text-heading_small">
            {fill(t("page.handover.stepOf"), { n: step + 1, total: HANDOVER_STEPS.length, label: t(HANDOVER_STEPS[step].label) })}
          </h2>
        )}

        {step === 0 && (
          <StepExport
            app={app}
            entities={entities}
            entitiesErr={entitiesErr}
            selected={effSelected}
            deps={deps}
            onSelect={(sel) => { setSelected(applySelection(effSelected, sel)); invalidatePack(); }}
            includeData={includeData}
            onIncludeData={(v) => { setIncludeData(v); invalidatePack(); }}
            exporting={exporting}
            exportErr={exportErr}
            exportWarnings={exportWarnings}
            pack={pack}
            onExport={runExport}
            onDownload={downloadCopy}
          />
        )}

        {step === 1 && (
          revealOpen && provisioned ? (
            // Show-once handover moment for a JUST-created workspace — the reused ack-gated
            // reveal. "Xong" keeps the plaintext in wizard memory for the final recap.
            <TenantCredentialsReveal
              result={provisioned}
              onDone={() => {
                setRevealOpen(false);
                setTargetSlug(provisioned.tenant.slug);
                void probeOperator();
              }}
            />
          ) : creating ? (
            <CreateTenantForm
              onCancel={() => setCreating(false)}
              onCreated={(res) => {
                setCreating(false);
                setProvisioned(res);
                invalidatePlan();
                if (res.oneTimeCredentials) {
                  setRevealOpen(true);
                } else {
                  toast.success(fill(t("page.handover.ws.created"), { slug: res.tenant.slug }));
                  setTargetSlug(res.tenant.slug);
                  void probeOperator();
                }
              }}
            />
          ) : (
            <StepWorkspace
              operator={operator}
              operatorForbidden={operatorForbidden}
              tenants={tenants}
              targetSlug={targetSlug}
              onPick={(slug) => { setTargetSlug(slug); invalidatePlan(); }}
              onCreate={() => setCreating(true)}
              onRetryProbe={retryProbe}
              hasPack={!!pack}
              onDownload={downloadCopy}
            />
          )
        )}

        {step === 2 && (
          <StepPlan
            targetSlug={targetSlug}
            plan={plan}
            loading={planLoading}
            err={planErr}
            onRetry={runPlan}
            onBackToWorkspace={() => setStep(1)}
          />
        )}

        {step === 3 && (
          result ? (
            <>
              <InstallResultView
                result={result}
                // RỔ-4 P1: chỉ cho đóng khi credentials đã được xác nhận lưu. Bản rõ chỉ sống
                // trong bộ nhớ wizard — một phím ESC/misclick là workspace mới có đúng một admin
                // mà không ai đăng nhập được. Không có credentials mới thì đóng tự do như cũ.
                closeDisabled={credsUnsaved}
                closeDisabledHint={t("page.handover.result.closeHint")}
                onClose={() => onOpenChange(false)}
                subtitle={fill(t("page.handover.result.subtitle"), { slug: targetSlug ?? "" })}
                actions={
                  <a href="/login" target="_blank" rel="noreferrer" className={buttonClassName("brand")}>
                    {t("page.handover.result.open")}
                    <ExternalLink aria-hidden="true" className="slds-button__icon slds-button__icon_right nx-lucide" />
                  </a>
                }
              />
              <p className="slds-text-body_small slds-text-color_weak">
                {fill(t("page.handover.result.loginNote"), { slug: targetSlug ?? "" })}
              </p>
              {freshCreds && <CredsRecap creds={freshCreds} saved={credsSaved} onSaved={setCredsSaved} />}
              <ScopedNotification>{t("page.handover.result.brandingNote")}</ScopedNotification>
            </>
          ) : (
            <StepApply
              app={app}
              pack={pack}
              targetSlug={targetSlug}
              plan={plan}
              installErr={installErr}
              installing={installing}
            />
          )
        )}
      </div>

      {/* Confirm — type the TARGET slug when the plan carries conflicts (cross-tenant write),
          simple confirm when clean. Same discipline as the in-library install flow.
          noStaffRole raises the bar to the SAME type-the-slug gate a conflicting plan gets:
          handing a customer an app no employee can open is not a smaller mistake than a
          conflicting install — it is the one they discover on day one, in front of their team. */}
      {plan && targetSlug && (
        <ConfirmPrompt
          open={confirmOpen}
          onOpenChange={setConfirmOpen}
          danger={typeName}
          confirmName={typeName ? targetSlug : undefined}
          title={
            noStaffRole
              ? t("page.handover.confirm.noStaffTitle")
              : requiresTypeName(plan)
                ? t("page.handover.confirm.conflictTitle")
                : fill(t("page.handover.confirm.cleanTitle"), { app: app.label, slug: targetSlug })
          }
          description={fill(
            t(noStaffRole
              ? "page.handover.confirm.noStaffDesc"
              : requiresTypeName(plan) ? "page.handover.confirm.conflictDesc" : "page.handover.confirm.cleanDesc"),
            { slug: targetSlug },
          )}
          impact={[
            ...conflictImpactRows(plan),
            // Not a conflict the dry-run can see: it is about the app being SENT, not the target.
            ...(noStaffRole ? [{ label: t("page.handover.confirm.noStaffImpact"), tone: "danger" as const }] : []),
          ]}
          confirmLabel={installing ? t("page.handover.installing") : t("page.handover.confirm.install")}
          loading={installing}
          onConfirm={doInstall}
        />
      )}
    </Modal>
  );
}

// ── Step 1 — XUẤT: entity table (select column) preselected from the app nav + export-to-state.
// A table, not a checkbox list: two entities can share a label ("Khách hàng" of CRM and of
// Ticketing) and only the technical name tells them apart. ──────────────────────────────────
function StepExport({
  app, entities, entitiesErr, selected, deps, onSelect, includeData, onIncludeData,
  exporting, exportErr, exportWarnings, pack, onExport, onDownload,
}: {
  app: AdminApp;
  entities: EntityMeta[] | null;
  entitiesErr: string | null;
  selected: string[];
  /** Tables pulled in because a field of the menu tables needs them. */
  deps: EntityDependency[];
  /** Row-selection map from the table (entity name → checked). */
  onSelect: (selection: Record<string, boolean>) => void;
  includeData: boolean;
  onIncludeData: (v: boolean) => void;
  exporting: boolean;
  exportErr: string | null;
  exportWarnings: string[];
  pack: Pack | null;
  onExport: () => void;
  onDownload: () => void;
}) {
  const columns = useMemo<ColumnDef<EntityMeta>[]>(() => [
    { id: "label", header: t("page.handover.export.col.label"), accessorFn: (e) => e.label ?? e.name },
    { id: "name", header: t("page.handover.export.col.name"), accessorKey: "name" },
  ], []);
  const rowSelection = Object.fromEntries(selected.map((n) => [n, true]));
  return (
    <div className="flex flex-col gap-3">
      <p>{fill(t("page.handover.export.intro"), { label: app.label })}</p>

      {entitiesErr && <ScopedNotification theme="error">{entitiesErr}</ScopedNotification>}
      {deps.length > 0 && (
        <ScopedNotification>
          {t("page.handover.export.deps")}
          <ul className="slds-list_dotted">
            {deps.map((d) => <li key={d.name}>{d.label}: {d.reason}</li>)}
          </ul>
        </ScopedNotification>
      )}
      {!entities && !entitiesErr && <Loading>{t("page.handover.export.loading")}</Loading>}

      {entities && (
        entities.length === 0 ? (
          <ScopedNotification>{t("page.handover.export.noEntities")}</ScopedNotification>
        ) : (
          <DataTable
            label={t("page.handover.export.entities")}
            columns={columns}
            data={entities}
            getRowId={(e) => e.name}
            selectable
            rowSelection={rowSelection}
            onRowSelectionChange={onSelect}
            maxHeight="15rem"
            bordered
          />
        )
      )}

      <Checkbox
        label={t("page.handover.export.includeData")}
        help={t("page.handover.export.includeDataHelp")}
        checked={includeData}
        onChange={(e) => onIncludeData(e.target.checked)}
      />

      <div className="flex flex-wrap items-center gap-2">
        <Button
          variant={pack ? "neutral" : "brand"}
          onClick={onExport}
          disabled={exporting || selected.length === 0}
          iconLeft={Package}
        >
          {exporting
            ? t("page.handover.export.running")
            : `${t(pack ? "page.handover.export.rerun" : "page.handover.export.run")}${selected.length > 0 ? ` (${selected.length})` : ""}`}
        </Button>
        {pack && (
          <>
            <Badge theme="success">{t("page.handover.export.ready")}</Badge>
            <Button onClick={onDownload} iconLeft={Download}>{t("page.handover.export.download")}</Button>
          </>
        )}
      </div>
      {exportErr && <ScopedNotification theme="error">{exportErr}</ScopedNotification>}

      {/* Honest excluded-manifest — what the pack could NOT carry (empty = complete). */}
      {pack && exportWarnings.length > 0 && (
        <div className="flex flex-col gap-1">
          <ScopedNotification theme="warning">
            <strong>{fill(t("page.handover.export.excluded"), { n: exportWarnings.length })}</strong>
          </ScopedNotification>
          <ul className="slds-list_dotted">
            {exportWarnings.map((w, i) => <li key={i}>{w}</li>)}
          </ul>
        </div>
      )}
    </div>
  );
}

// ── Step 2 — WORKSPACE: probe → pick existing / create new / honest escalation ───────────────
function StepWorkspace({
  operator, operatorForbidden, tenants, targetSlug, onPick, onCreate, onRetryProbe,
  hasPack, onDownload,
}: {
  operator: OperatorProbe;
  operatorForbidden: boolean;
  tenants: TenantSummary[];
  targetSlug: string | null;
  onPick: (slug: string) => void;
  onCreate: () => void;
  onRetryProbe: () => void;
  hasPack: boolean;
  onDownload: () => void;
}) {
  const columns = useMemo<ColumnDef<TenantSummary>[]>(() => [
    {
      id: "pick",
      header: t("page.handover.ws.col.pick"),
      size: 64,
      // data-tables/advanced single-select row: the bare slds-radio, label for AT only.
      cell: ({ row }) => {
        const tn = row.original;
        const label = fill(t("page.handover.ws.pick"), { name: tn.name });
        const id = `handover-target-${tn.id}`;
        return (
          <span className="slds-radio">
            <input
              type="radio"
              id={id}
              name="handover-target"
              aria-label={label}
              checked={targetSlug === tn.slug}
              disabled={tn.status !== "active"}
              onChange={() => onPick(tn.slug)}
            />
            <label className="slds-radio__label" htmlFor={id} aria-hidden="true">
              <span className="slds-radio_faux" />
              <span className="slds-form-element__label slds-assistive-text">{label}</span>
            </label>
          </span>
        );
      },
    },
    { id: "name", header: t("page.handover.ws.col.name"), accessorKey: "name" },
    { id: "slug", header: t("page.handover.ws.col.slug"), accessorKey: "slug" },
    {
      id: "status",
      header: t("page.handover.ws.col.status"),
      accessorKey: "status",
      cell: ({ row }) => row.original.status === "active"
        ? <Badge theme="success">{t("page.handover.ws.active")}</Badge>
        : <Badge theme="warning">{row.original.status}</Badge>,
    },
    {
      id: "admins",
      header: t("page.handover.ws.col.admins"),
      accessorFn: (tn) => `${tn.usableAdminCount} · ${tn.usableOwnerCount}`,
    },
  ], [targetSlug, onPick]);

  if (operator === "loading") return <Loading>{t("page.handover.ws.checking")}</Loading>;

  if (operator === "no") {
    // Honest escalation — name the need and the way forward, never a one-line dead-end.
    return operatorForbidden ? (
      <div className="flex flex-col gap-2">
        <ScopedNotification theme="warning">
          <strong>{t("page.handover.ws.needOperator")}</strong>
        </ScopedNotification>
        <p>{t("page.handover.ws.needOperatorBody")}</p>
        <ul className="slds-list_dotted">
          <li>{t("page.handover.ws.ask1")}</li>
          <li>{t("page.handover.ws.ask2")}</li>
          <li>{t("page.handover.ws.ask3")}</li>
        </ul>
        <div className="flex flex-wrap items-center gap-2">
          {hasPack && <Button onClick={onDownload} iconLeft={Download}>{t("page.handover.ws.downloadForOperator")}</Button>}
          <Button onClick={onRetryProbe}>{t("page.handover.ws.recheck")}</Button>
        </div>
      </div>
    ) : (
      <div className="flex flex-col items-start gap-2">
        <ScopedNotification theme="error">{t("page.handover.ws.probeFailed")}</ScopedNotification>
        <Button onClick={onRetryProbe}>{t("action.retry")}</Button>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p>{t("page.handover.ws.pickIntro")}</p>
        <Button onClick={onCreate} iconLeft={Plus}>{t("page.handover.ws.createNew")}</Button>
      </div>

      {tenants.length === 0 ? (
        <ScopedNotification>{t("page.handover.ws.none")}</ScopedNotification>
      ) : (
        <DataTable
          label={t("page.handover.ws.table")}
          columns={columns}
          data={tenants}
          getRowId={(tn) => tn.id}
          rowHeader="name"
          bordered
        />
      )}
      <p className="slds-text-body_small slds-text-color_weak">{t("page.handover.ws.footnote")}</p>
    </div>
  );
}

// ── Step 3 — PLAN: dry-run against the TARGET, same PlanView as the library ──────────────────
function StepPlan({
  targetSlug, plan, loading, err, onRetry, onBackToWorkspace,
}: {
  targetSlug: string | null;
  plan: InstallPlan | null;
  loading: boolean;
  err: TargetErrorInfo | null;
  onRetry: () => void;
  onBackToWorkspace: () => void;
}) {
  return (
    <div className="flex flex-col gap-3">
      <ScopedNotification>{fill(t("page.handover.plan.intro"), { slug: targetSlug ?? "" })}</ScopedNotification>

      {loading && <Loading>{t("page.handover.plan.running")}</Loading>}

      {!loading && err && (
        <div className="flex flex-col items-start gap-2">
          <ScopedNotification theme={err.kind === "unsupported" ? "warning" : "error"}>
            <strong>{t(err.kind === "unsupported" ? "page.handover.plan.unsupported" : "page.handover.plan.failed")}</strong>
            {" — "}{err.message}
          </ScopedNotification>
          <div className="flex gap-2">
            <Button onClick={onRetry}>{t("action.retry")}</Button>
            {err.kind === "target-gone" && <Button onClick={onBackToWorkspace}>{t("page.handover.plan.backToWorkspace")}</Button>}
          </div>
        </div>
      )}

      {!loading && !err && plan && <PlanView plan={plan} />}
    </div>
  );
}

// ── Step 4 (pre-install) — recap + honest WRITE warning (the button lives in the footer) ─────
function StepApply({
  app, pack, targetSlug, plan, installErr, installing,
}: {
  app: AdminApp;
  pack: Pack | null;
  targetSlug: string | null;
  plan: InstallPlan | null;
  installErr: TargetErrorInfo | null;
  installing: boolean;
}) {
  const entityCount = pack?.entities?.length ?? 0;
  return (
    <div className="flex flex-col gap-3">
      <p className="slds-text-heading_small">{t("page.handover.apply.ready")}</p>
      <dl className="slds-dl_horizontal">
        <dt className="slds-dl_horizontal__label">{t("page.handover.apply.app")}</dt>
        <dd className="slds-dl_horizontal__detail">{fill(t("page.handover.apply.appValue"), { label: app.label, n: entityCount })}</dd>
        <dt className="slds-dl_horizontal__label">{t("page.handover.apply.target")}</dt>
        <dd className="slds-dl_horizontal__detail">{targetSlug}</dd>
        {plan && (
          <>
            <dt className="slds-dl_horizontal__label">{t("page.handover.apply.plan")}</dt>
            <dd className="slds-dl_horizontal__detail">
              <span className="flex flex-wrap items-center gap-2">
                {fill(t("page.handover.apply.planValue"), { c: plan.entitiesToCreate.length, s: plan.entitiesToSkip.length })}
                {plan.conflicts.length > 0
                  ? <Badge theme="warning">{fill(t("page.handover.apply.conflicts"), { n: plan.conflicts.length })}</Badge>
                  : <Badge theme="success">{t("page.handover.apply.clean")}</Badge>}
              </span>
            </dd>
          </>
        )}
      </dl>

      <ScopedNotification theme="warning">{t("page.handover.apply.writeWarn")}</ScopedNotification>

      {installErr && (
        <ScopedNotification theme={installErr.kind === "unsupported" ? "warning" : "error"}>{installErr.message}</ScopedNotification>
      )}

      {installing && <Loading>{t("page.handover.apply.running")}</Loading>}
    </div>
  );
}

// ── Final-screen credentials recap (spec §4.5) — the plaintext still lives ONLY in wizard
// memory (set at creation, never storage); closing the wizard destroys it for real. ──────────
function CredsRecap({
  creds, saved, onSaved,
}: {
  creds: ProvisionedTenant;
  saved: boolean;
  onSaved: (v: boolean) => void;
}) {
  const [copied, setCopied] = useState(false);
  const loginUrl = typeof window !== "undefined" ? `${window.location.origin}/login` : "/login";
  const rows = [
    { label: t("page.packInstall.creds.url"), value: loginUrl },
    { label: t("page.packInstall.creds.tenant"), value: creds.tenant.slug },
    { label: t("page.packInstall.creds.email"), value: creds.admin.email },
    { label: t("page.packInstall.creds.password"), value: creds.oneTimeCredentials?.password ?? "" },
  ];
  async function copyBlock() {
    try {
      await navigator.clipboard.writeText(rows.map((r) => `${r.label}: ${r.value}`).join("\n"));
      setCopied(true);
      onSaved(true); // chép được = đã cầm trong tay; vẫn cho bỏ tick nếu muốn
      setTimeout(() => setCopied(false), 2000);
    } catch {
      toast.error(t("page.packInstall.creds.copyFailBlock"));
    }
  }
  return (
    <div className="flex flex-col gap-2">
      <ScopedNotification theme="warning">
        <strong>{t("page.handover.creds.title")}</strong>
      </ScopedNotification>
      <dl className="slds-dl_horizontal">
        {rows.map((r) => (
          <Fragment key={r.label}>
            <dt className="slds-dl_horizontal__label">{r.label}</dt>
            <dd className="slds-dl_horizontal__detail break-all">{r.value}</dd>
          </Fragment>
        ))}
      </dl>
      <div>
        <Button onClick={copyBlock} iconLeft={copied ? Check : Copy}>
          {copied ? t("page.packInstall.creds.copiedBlock") : t("page.packInstall.creds.copyBlock")}
        </Button>
      </div>
      <Checkbox
        label={t("page.handover.creds.ack")}
        help={t("page.handover.creds.ackHelp")}
        checked={saved}
        onChange={(e) => onSaved(e.target.checked)}
      />
    </div>
  );
}
