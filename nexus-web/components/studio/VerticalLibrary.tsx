"use client";

// "Cài mẫu & Bàn giao" — Vertical Library v4 (Wave B2: DeployScreen merged INTO this screen).
// Pack install is ONE job on ONE screen now:
//   HERO   = builtin pack cards (GET /admin/verticals/builtin). The card IS the install entry:
//            "Cài đặt" opens the install flow in a modal — live dry-run plan (never writes) →
//            prompt (type-name WHEN conflicts) → real install → per-component result.
//   IMPORT = "Nhập pack từ tệp" runs the SAME flow on a parsed pack file — the old
//            sessionStorage handoff to /studio/deploy is dead.
//   EXPORT = unchanged (Xuất Vertical Pack).
//   BOTTOM = "Workspace khách" (TENANT-PROVISION MS-B, operator-only via GET /admin/tenants
//            probe) — provisioning is NOT installation, so it lives in its own separated section.
// Honest guards preserved verbatim: install on THIS screen always targets the CURRENT tenant
// (cross-workspace install lives in the HandoverWizard on AppOverview — EASE Nước 4),
// operator-refusal line, show-once credentials with ack gate.
// Version honesty (FE-CATCHUP-2): card version/entity count render from the live /builtin
// manifest; the ledger supplies the INSTALLED version. Nothing hardcoded.
// Plan/conflict/result view-model logic lives in lib/verticalPlan.ts (unit-tested); thin renderer.
// PlanView / InstallResultView / CreateTenantForm / TenantCredentialsReveal / ConfirmPrompt live in
// workspaceShared.tsx (Nước 4) so the HandoverWizard reuses — never forks — them.
// SLDS (specs/038 T045): page-headers/base · tabs/base · cards (pack, import, export, workspaces) ·
// file-selector/files · data-tables/base (export picker, roadmap, history, workspaces) ·
// modals/base (install flow) · prompt (type-the-name confirm) · scoped-notifications · illustration.

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { studioHref } from "@/lib/studioRoutes";
import { projectSlugAt } from "@/lib/studioContext";
import { useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import {
  ArrowRight, Building2, ChevronUp, Download, Eye, FileUp, GitCompare, History, Layers, Package, Plus, Rocket,
} from "lucide-react";
import { VERTICALS, BUILTIN_PACK_FACTS, orderBuiltinsForDisplay, type Vertical } from "@/lib/studio";
import { toast } from "@/components/ui/Toaster";
import {
  ensureAuth, listEntities, listBuiltinVerticals, exportVertical, listPackInstalls,
  dryRunInstall, installPack, listPackUpgrades, listTenants, retireTenant, purgeTenant, enterWorkspace, setOwnerTier, revertOwnerTier, ApiError,
  type BuiltinVertical, type EntityMeta, type PackSource, type TenantSummary, type ProvisionedTenant,
  type PurgeReport, type UpgradeRun,
} from "@/lib/api";
import { PackDiffPanel } from "@/components/studio/PackDiffPanel";
import { TenantBackupPanel } from "@/components/studio/TenantBackupPanel";
import { PackUpgradePanel } from "@/components/studio/PackUpgradePanel";
import {
  PlanView, InstallResultView, conflictImpactRows, CreateTenantForm, TenantCredentialsReveal, ConfirmPrompt, Loading,
} from "@/components/studio/workspaceShared";
import {
  installStatusForSlug, sortInstallsNewestFirst, parsePackFile, packContentsFromPlan,
  requiresTypeName,
  type InstallRecord, type PackContents, type InstallPlan, type InstallResult,
  type ManifestDisplay,
} from "@/lib/verticalPlan";
import { applySelection } from "@/lib/handoverWizard";
import { errorMessage } from "@/lib/errorMessages";
import { formatDateTime, formatTime } from "@/lib/format";
import { useActiveApp } from "@/lib/apps";
import { useAuth } from "@/lib/auth/AuthProvider";
import { inWorkspace } from "@/lib/workspace";
import { qk } from "@/lib/query/keys";
import { t } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { Badge } from "@/components/slds/Badge";
import { Button, buttonClassName } from "@/components/slds/Button";
import { Card } from "@/components/slds/Card";
import { Checkbox } from "@/components/slds/Checkbox";
import { DataTable, fill } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { FileSelector } from "@/components/slds/FileSelector";
import { Modal } from "@/components/slds/Modal";
import { PageHeader } from "@/components/slds/PageHeader";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Tabs } from "@/components/slds/Tabs";

// Per-slug state of the on-card contents preview (live dry-run, read-only).
interface PreviewState {
  open: boolean;
  loading: boolean;
  error: string | null;
  contents: PackContents | null;
}

// What the install modal is installing: a builtin card or a parsed pack file.
interface InstallTarget {
  source: PackSource;
  displayName: string;
  /** String typed to unlock the confirm when the plan carries conflicts (slug / manifest key). */
  confirmKey: string;
  /** Imported file only: its manifest summary, shown above the plan. */
  manifest?: ManifestDisplay;
  entityCount?: number;
  /** Bumped per open so the flow remounts (fresh dry-run) every time. */
  nonce: number;
}

export function VerticalLibrary() {
  const dataHref = studioHref(projectSlugAt(usePathname()), "dung/du-lieu");
  // Slice 2.3: the app currently being built (Studio scope) — its identity rides into the exported
  // pack so re-install mints a correctly-named sidebar app. null = "Toàn tenant" → generic pack.
  const { activeMeta: activeApp } = useActiveApp();
  const [builtins, setBuiltins] = useState<BuiltinVertical[]>([]);
  const [entities, setEntities] = useState<EntityMeta[]>([]);
  const [installs, setInstalls] = useState<InstallRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  // Contents preview (dry-run) per builtin slug.
  const [previews, setPreviews] = useState<Record<string, PreviewState>>({});

  // Install flow (modal) — a builtin card or an imported pack file.
  const [target, setTarget] = useState<InstallTarget | null>(null);
  const nonce = useRef(0);
  const [importErr, setImportErr] = useState<string | null>(null);
  // An imported file whose pack is already installed opens the upgrade screen, not a reinstall.
  const [upgradeFile, setUpgradeFile] = useState<{ slug: string; name: string; source: PackSource } | null>(null);

  useEffect(() => {
    (async () => {
      try {
        await ensureAuth();
        const [blt, ents, ins] = await Promise.all([
          listBuiltinVerticals(), listEntities(), listPackInstalls(),
        ]);
        setBuiltins(blt);
        setEntities(ents);
        setInstalls(ins);
      } catch (e) {
        setErr(errorMessage(e));
      } finally {
        setLoading(false);
      }
    })();
  }, []);

  // After a real install, re-read the ledger so card badges flip to "Đã cài" immediately.
  const refreshInstalls = useCallback(async () => {
    try {
      setInstalls(await listPackInstalls());
    } catch {
      // Best-effort refresh — the install itself already succeeded and showed its result.
    }
  }, []);

  // Toggle the on-card preview: close if open; reuse loaded contents; else run a real dry-run.
  async function togglePreview(slug: string) {
    const cur = previews[slug];
    if (cur?.open) {
      setPreviews((p) => ({ ...p, [slug]: { ...cur, open: false } }));
      return;
    }
    if (cur?.contents) {
      setPreviews((p) => ({ ...p, [slug]: { ...cur, open: true, error: null } }));
      return;
    }
    setPreviews((p) => ({ ...p, [slug]: { open: true, loading: true, error: null, contents: null } }));
    try {
      const plan = await dryRunInstall({ builtin: slug });
      setPreviews((p) => ({ ...p, [slug]: { open: true, loading: false, error: null, contents: packContentsFromPlan(plan) } }));
    } catch (e) {
      setPreviews((p) => ({ ...p, [slug]: { open: true, loading: false, error: errorMessage(e), contents: null } }));
    }
  }

  function openInstall(next: Omit<InstallTarget, "nonce">) {
    setTarget({ ...next, nonce: ++nonce.current });
  }

  // Import a pack file → validate → open the install flow on it right here.
  function handleImportFile(file: File) {
    setImportErr(null);
    const reader = new FileReader();
    reader.onload = () => {
      const parsed = parsePackFile(String(reader.result ?? ""));
      if (!parsed.ok) { setImportErr(parsed.error); return; }
      const fileSlug = parsed.manifest.slug;
      if (fileSlug && installs.some((i) => i.slug === fileSlug)) {
        setUpgradeFile({ slug: fileSlug, name: parsed.manifest.name || fileSlug, source: { pack: parsed.pack } });
        return;
      }
      openInstall({
        source: { pack: parsed.pack },
        displayName: parsed.manifest.name,
        confirmKey: parsed.manifest.slug || parsed.manifest.name,
        manifest: parsed.manifest,
        entityCount: parsed.entityCount,
      });
    };
    reader.onerror = () => setImportErr(t("page.verticals.import.readFailed"));
    reader.readAsText(file);
  }

  const roadmap = VERTICALS.filter((v) => !builtins.some((b) => b.slug === v.name));

  const library = (
    <div className="flex flex-col gap-6">
      {/* ── HERO — packs that actually install (backend builtins) ── */}
      <section className="flex flex-col gap-3">
        <div>
          <h2 className="slds-text-heading_small">
            {t("page.verticals.packs")}{!loading && builtins.length > 0 ? ` (${builtins.length})` : ""}
          </h2>
          <p className="slds-text-body_small slds-text-color_weak">{t("page.verticals.packsNote")}</p>
        </div>
        {loading ? (
          <Loading>{t("state.loading")}</Loading>
        ) : builtins.length === 0 ? (
          <ScopedNotification>{t("page.verticals.noPacks")}</ScopedNotification>
        ) : (
          <div className="grid gap-4 lg:grid-cols-2">
            {/* One wrapper per grid cell: SLDS spaces adjacent cards (.slds-card + .slds-card), which
                would push every card after the first down inside its row. */}
            {orderBuiltinsForDisplay(builtins).map((b) => (
              <div key={b.slug}>
                <BuiltinPackCard
                  pack={b}
                  installs={installs}
                  preview={previews[b.slug]}
                  onTogglePreview={() => togglePreview(b.slug)}
                  onInstall={(displayName) => openInstall({ source: { builtin: b.slug }, displayName, confirmKey: b.slug })}
                />
              </div>
            ))}
          </div>
        )}
      </section>

      {/* ── Import from file / build from scratch ── */}
      <div className="grid gap-4 lg:grid-cols-2">
        <div>
          <Card className="h-full" title={t("page.verticals.import.title")} icon={{ glyph: FileUp, name: "file" }}>
            <div className="flex flex-col gap-2">
              <FileSelector
                label={t("page.verticals.import.label")}
                accept="application/json,.json"
                buttonLabel={t("page.verticals.import.button")}
                dropLabel={t("page.verticals.import.drop")}
                error={importErr ?? undefined}
                onFiles={(files) => { if (files[0]) handleImportFile(files[0]); }}
              />
              <p className="slds-text-body_small slds-text-color_weak">{t("page.verticals.import.hint")}</p>
            </div>
          </Card>
        </div>
        <div>
          <Card className="h-full" title={t("page.verticals.scratch.title")} icon={{ glyph: Plus, name: "custom" }}>
            <div className="flex flex-col items-start gap-3">
              <p>{t("page.verticals.scratch.hint")}</p>
              <Link href={dataHref} className={buttonClassName("neutral")}>
                {t("page.verticals.scratch.open")}
                <ArrowRight aria-hidden="true" className="slds-button__icon slds-button__icon_right nx-lucide" />
              </Link>
            </div>
          </Card>
        </div>
      </div>

      {entities.length > 0 && (
        <ExportCard
          entities={entities}
          appLabel={activeApp?.label}
          appIdentity={activeApp ? { slug: activeApp.name, name: activeApp.label, icon: activeApp.icon ?? undefined } : undefined}
        />
      )}

      {/* ── Lộ trình — verticals with NO backend pack. Zero install affordance: these are
             direction, not product. Never render them as installable cards. ── */}
      {!loading && roadmap.length > 0 && <Roadmap roadmap={roadmap} />}
    </div>
  );

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
      <div className="flex flex-col gap-4 p-4">
        <PageHeader
          icon={{ glyph: Layers, name: "product", label: t("page.verticals.title") }}
          title={t("page.verticals.title")}
          meta={t("page.verticals.intro")}
        />

        {err && <ScopedNotification theme="error">{err}</ScopedNotification>}

        <Tabs
          label={t("page.verticals.title")}
          defaultValue="library"
          items={[
            { value: "library", label: t("page.verticals.tab.library"), content: library },
            { value: "history", label: t("page.verticals.tab.history"), content: <InstallHistory installs={installs} loading={loading} /> },
          ]}
        />

        {/* ── Workspace khách — provisioning is NOT installation: its own separated section.
               Mounted only after boot's ensureAuth resolved (probe must 403, never race a 401). ── */}
        {!loading && !err && <WorkspaceKhachSection />}
      </div>

      {upgradeFile && (
        <Modal
          open
          onOpenChange={(o) => { if (!o) setUpgradeFile(null); }}
          size="large"
          title={fill(t("page.verticals.import.upgradeTitle"), { name: upgradeFile.name })}
          tagline={t("page.verticals.import.alreadyInstalled")}
          footer={<Button onClick={() => setUpgradeFile(null)}>{t("action.close")}</Button>}
        >
          <PackUpgradePanel slug={upgradeFile.slug} source={upgradeFile.source} onRun={refreshInstalls} />
        </Modal>
      )}

      {target && (
        <InstallFlowModal
          key={target.nonce}
          target={target}
          onInstalled={refreshInstalls}
          onClose={() => setTarget(null)}
        />
      )}
    </div>
  );
}

// ── Hero card — one REAL builtin pack. Version + entity count come from the pack's live
// manifest (GET /builtin); facts stay the static content checklist mirroring builtin.go;
// preview is a live dry-run. A builtin without a facts entry renders from the manifest alone.
// The card is ALSO the install entry: "Cài đặt" opens the dry-run → confirm → result modal. ──
function BuiltinPackCard({
  pack, installs, preview, onTogglePreview, onInstall,
}: {
  pack: BuiltinVertical;
  installs: InstallRecord[];
  preview: PreviewState | undefined;
  onTogglePreview: () => void;
  onInstall: (displayName: string) => void;
}) {
  const [diffOpen, setDiffOpen] = useState(false);
  const [lastRun, setLastRun] = useState<UpgradeRun | null>(null);
  const { slug, manifest, entityCount } = pack;
  const meta = VERTICALS.find((v) => v.name === slug);
  const facts = BUILTIN_PACK_FACTS[slug];
  const status = installStatusForSlug(slug, installs);
  const installedHere = status.installed;
  // "Lần nâng cấp gần nhất": best-effort read of the history; the card works without it.
  useEffect(() => {
    if (!installedHere) return;
    let alive = true;
    listPackUpgrades(slug, 1).then((rs) => { if (alive) setLastRun(rs[0] ?? null); }).catch(() => { /* history is a nicety */ });
    return () => { alive = false; };
  }, [slug, installedHere]);
  const open = preview?.open ?? false;
  const displayName = meta?.label ?? (manifest.name || slug);
  // Tagline: prefer the curated VN checklist tagline; a facts-less builtin falls back to the
  // manifest's own description (real, from the registry) — never an invented blurb.
  const tagline = facts?.tagline ?? manifest.description;

  return (
    <Card
      className="h-full"
      title={displayName}
      icon={{ glyph: Package, name: "product" }}
      actions={
        status.installed ? (
          <Badge theme="success">
            {status.version ? fill(t("page.verticals.card.installedV"), { v: status.version }) : t("page.verticals.card.installed")}
          </Badge>
        ) : (
          <Badge>{t("page.verticals.card.notInstalled")}</Badge>
        )
      }
    >
      <div className="flex flex-col gap-3">
        <div className="flex flex-wrap items-center gap-2">
          {manifest.version && <Badge variant="lightest">{`v${manifest.version}`}</Badge>}
          <span className="slds-text-body_small slds-text-color_weak">
            {entityCount > 0 && fill(t("page.verticals.card.entities"), { n: entityCount })}
            {meta?.industries ?? slug}
          </span>
        </div>

        {status.installed && status.installedAt && (
          <p className="slds-text-body_small slds-text-color_weak">
            {fill(t("page.verticals.card.lastInstall"), { at: formatDateTime(status.installedAt) })}
            {/* Honest upgrade hint: ledger says an OLDER version is installed than the pack now ships. */}
            {status.version && manifest.version && status.version !== manifest.version && (
              <strong>{fill(t("page.verticals.card.upgrade"), { v: manifest.version })}</strong>
            )}
          </p>
        )}

        {installedHere && lastRun && (
          <p className="slds-text-body_small slds-text-color_weak">
            {fill(t("page.verticals.card.lastUpgrade"), {
              at: formatDateTime(lastRun.startedAt),
              status: t(`page.packUpgrade.runShort.${lastRun.status}` as I18nKey),
            })}
          </p>
        )}

        {tagline && <p>{tagline}</p>}

        {/* Real contents — mirrors the shipped pack; live-verifiable via the preview below. */}
        {facts && (
          <ul className="slds-list_dotted">
            {facts.features.map((f) => <li key={f}>{f}</li>)}
          </ul>
        )}

        <div className="flex flex-wrap gap-2">
          {status.installed && (
            <Button iconLeft={GitCompare} aria-expanded={diffOpen} onClick={() => setDiffOpen((v) => !v)}>
              {t(diffOpen ? "page.verticals.card.diffHide" : "page.verticals.card.diff")}
            </Button>
          )}
          <Button iconLeft={open ? ChevronUp : Eye} aria-expanded={open} onClick={onTogglePreview}>
            {t(open ? "page.verticals.card.previewHide" : "page.verticals.card.preview")}
          </Button>
          {/* An installed pack is upgraded, not reinstalled: the reinstall skips by name and cannot
              bring a changed rule/report/screen across (specs/069). */}
          {status.installed && (
            <Button iconLeft={Rocket} onClick={() => onInstall(displayName)}>
              {t("page.verticals.card.reinstall")}
            </Button>
          )}
          {/* Upgrading an installed pack lives in the project's Bảo trì > Nâng cấp page (specs/079). */}
          {!status.installed && (
            <Button variant="brand" iconLeft={Rocket} onClick={() => onInstall(displayName)}>
              {t("page.verticals.card.install")}
            </Button>
          )}
        </div>

        {/* Live preview — REAL dry-run against the current workspace (never writes). */}
        {open && (
          <div className="slds-box slds-box_x-small slds-theme_shade">
            <div className="flex flex-col gap-2">
              <h3 className="slds-text-title_caps">{t("page.verticals.card.previewTitle")}</h3>
              {preview?.loading && <Loading>{t("page.verticals.card.previewRunning")}</Loading>}
              {preview?.error && <ScopedNotification theme="error">{preview.error}</ScopedNotification>}
              {preview?.contents && <PackContentsView contents={preview.contents} />}
            </div>
          </div>
        )}

        {/* KHUON-SONG-P1: read-only provenance diff — what still matches the installed khuôn. */}
        {diffOpen && status.installed && (
          <div className="slds-box slds-box_x-small">
            <PackDiffPanel slug={slug} />
          </div>
        )}
      </div>
    </Card>
  );
}

// ── Install flow (modal) — mounts → runs the dry-run → renders the plan → prompt (type-name
// WHEN conflicts exist) → real install → per-component result. Used by the pack cards and the
// import section alike; the parent remounts it per open, so every open is a FRESH dry-run. ──────
function InstallFlowModal({
  target, onInstalled, onClose,
}: {
  target: InstallTarget;
  onInstalled: () => void;
  onClose: () => void;
}) {
  const { source, displayName, confirmKey } = target;
  const qc = useQueryClient();
  const [plan, setPlan] = useState<InstallPlan | null>(null);
  const [planLoading, setPlanLoading] = useState(true);
  const [planErr, setPlanErr] = useState<string | null>(null);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [installing, setInstalling] = useState(false);
  const [installErr, setInstallErr] = useState<string | null>(null);
  const [result, setResult] = useState<InstallResult | null>(null);

  // The dry-run is an external subscription: state is written only from its callbacks. `source`
  // is fixed for this mount (the parent remounts per open); retry bumps `attempt`.
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    let alive = true;
    dryRunInstall(source)
      .then((p) => { if (alive) setPlan(p); }, (e) => { if (alive) setPlanErr(errorMessage(e)); })
      .finally(() => { if (alive) setPlanLoading(false); });
    return () => { alive = false; };
  }, [source, attempt]);
  const retryPlan = () => { setPlanLoading(true); setPlanErr(null); setPlan(null); setAttempt((n) => n + 1); };

  async function doInstall() {
    setInstalling(true); setInstallErr(null);
    try {
      const r = await installPack(source);
      setResult(r);
      setConfirmOpen(false);
      onInstalled();
    } catch (e) {
      setInstallErr(errorMessage(e));
      setConfirmOpen(false);
    } finally {
      setInstalling(false);
      // A pack mints (or fills the nav of) its own sidebar app. Without this the 5-minute app
      // catalog never learns of it: the App Launcher hides it and its Studio workspace resolves to
      // "*" (tenant-wide) — same pair CreateAppDialog invalidates. qk.apps() also covers the nav
      // keys under it. In `finally`: a failed response (timeout) may still have committed the app.
      void qc.invalidateQueries({ queryKey: qk.apps() });
      void qc.invalidateQueries({ queryKey: qk.adminApps() });
    }
  }

  const typeName = !!plan && requiresTypeName(plan);
  return (
    <Modal
      open
      // Closing mid-install would drop the result screen; the install itself is idempotent.
      onOpenChange={(o) => { if (!o && !installing) onClose(); }}
      size="medium"
      title={fill(t("page.verticals.flow.title"), { name: displayName })}
      // Honest note — install on this screen always targets the CURRENT tenant.
      tagline={t("page.verticals.flow.hereNote")}
      footer={result ? undefined : (
        <div className="flex items-center justify-between gap-3">
          <Button onClick={onClose} disabled={installing}>{t("action.close")}</Button>
          <Button
            variant="brand"
            iconRight={ArrowRight}
            disabled={planLoading || !!planErr || !plan || installing}
            onClick={() => setConfirmOpen(true)}
          >
            {t("page.verticals.flow.install")}
          </Button>
        </div>
      )}
    >
      <div className="flex flex-col gap-3">
        {target.manifest && <ManifestSummary manifest={target.manifest} entityCount={target.entityCount} />}
        {result ? (
          <InstallResultView result={result} onClose={onClose} />
        ) : (
          <>
            {planLoading && <Loading>{t("page.verticals.flow.running")}</Loading>}
            {!planLoading && planErr && (
              <div className="flex flex-col items-start gap-2">
                <ScopedNotification theme="error">
                  <strong>{t("page.verticals.flow.failed")}</strong>{" — "}{planErr}
                </ScopedNotification>
                <Button onClick={retryPlan}>{t("action.retry")}</Button>
              </div>
            )}
            {!planLoading && !planErr && plan && <PlanView plan={plan} />}
            {installErr && <ScopedNotification theme="error">{installErr}</ScopedNotification>}
          </>
        )}
      </div>

      {/* Confirm — type-name WHEN conflicts exist, else simple. */}
      {plan && (
        <ConfirmPrompt
          open={confirmOpen}
          onOpenChange={setConfirmOpen}
          danger={typeName}
          confirmName={typeName ? confirmKey : undefined}
          title={typeName ? t("page.verticals.flow.confirmConflictTitle") : fill(t("page.verticals.flow.confirmCleanTitle"), { name: displayName })}
          description={t(typeName ? "page.verticals.flow.confirmConflictDesc" : "page.verticals.flow.confirmCleanDesc")}
          impact={conflictImpactRows(plan)}
          confirmLabel={installing ? t("page.verticals.flow.installing") : t("page.verticals.flow.install")}
          loading={installing}
          onConfirm={doInstall}
        />
      )}
    </Modal>
  );
}

// ── Manifest summary of an imported pack file (ported from the wizard's Step 1) ──
function ManifestSummary({ manifest, entityCount }: { manifest: ManifestDisplay; entityCount?: number }) {
  const facts: [I18nKey, ReactNode][] = [];
  if (manifest.slug) facts.push(["page.verticals.manifest.slug", manifest.slug]);
  if (manifest.author) facts.push(["page.verticals.manifest.author", manifest.author]);
  if (typeof entityCount === "number") facts.push(["page.verticals.manifest.entities", entityCount]);
  return (
    <div className="flex flex-col gap-1">
      <div className="flex flex-wrap items-center gap-2">
        <h2 className="slds-text-heading_small">{manifest.name}</h2>
        <Badge variant="lightest">{manifest.version ? `v${manifest.version}` : t("page.verticals.manifest.noVersion")}</Badge>
        <Badge>{t("page.verticals.manifest.import")}</Badge>
      </div>
      {manifest.description && <p className="slds-text-color_weak">{manifest.description}</p>}
      {facts.length > 0 && (
        <ul className="slds-list_horizontal slds-has-dividers_left slds-text-body_small">
          {facts.map(([k, v]) => <li key={k} className="slds-item">{t(k)}: {v}</li>)}
        </ul>
      )}
    </div>
  );
}

// Compact renderer of the dry-run derived contents (entities + pack-total counts).
function PackContentsView({ contents }: { contents: PackContents }) {
  const stats: [number, I18nKey][] = ([
    [contents.fieldsToAddCount, "page.verticals.contents.fields"],
    [contents.rollupCount, "page.verticals.contents.rollups"],
    [contents.nnCount, "page.verticals.contents.nn"],
    [contents.layoutsCount, "page.verticals.contents.layouts"],
    [contents.rolesCount, "page.verticals.contents.roles"],
    [contents.teamsCount, "page.verticals.contents.teams"],
    [contents.insightsCount, "page.verticals.contents.insights"],
    [contents.automationCount, "page.verticals.contents.automation"],
    [contents.hooksCount, "page.verticals.contents.hooks"],
    [contents.samplesCount, "page.verticals.contents.samples"],
  ] as [number, I18nKey][]).filter(([v]) => v > 0);

  return (
    <div className="flex flex-col gap-2">
      {contents.entities.length > 0 && (
        <ul className="flex flex-col gap-1">
          {contents.entities.map((e) => (
            <li key={e.name} className="flex items-center gap-2">
              {e.exists
                ? <Badge variant="lightest">{t("page.verticals.contents.exists")}</Badge>
                : <Badge theme="success">{t("page.verticals.contents.create")}</Badge>}
              <span className="slds-truncate" title={e.name}>{e.name}</span>
            </li>
          ))}
        </ul>
      )}
      {stats.length > 0 && (
        <p className="slds-text-body_small">
          {stats.map(([v, k], i) => (
            <span key={k}>
              {i > 0 && " · "}
              <strong className="tabular-nums">{v}</strong> {t(k)}
            </span>
          ))}
        </p>
      )}
      <p className="slds-text-body_small slds-text-color_weak">{t("page.verticals.contents.note")}</p>
    </div>
  );
}

// ── Export pack — pick entities, download the JSON pack ──
function ExportCard({
  entities, appLabel, appIdentity,
}: {
  entities: EntityMeta[];
  appLabel?: string;
  appIdentity?: { slug: string; name: string; icon?: string };
}) {
  const [exporting, setExporting] = useState(false);
  const [exportOk, setExportOk] = useState(false);
  const [exportErr, setExportErr] = useState<string | null>(null);
  // Slice 2.2: things the pack could not faithfully carry (returned by the export endpoint) —
  // shown as an honest "excluded" manifest so a maker knows the pack is incomplete before shipping.
  const [exportWarnings, setExportWarnings] = useState<string[]>([]);
  const [selected, setSelected] = useState<string[]>([]);
  const [includeData, setIncludeData] = useState(false);
  const columns = useMemo<ColumnDef<EntityMeta>[]>(() => [
    { id: "label", header: t("page.handover.export.col.label"), accessorFn: (e) => e.label ?? e.name },
    { id: "name", header: t("page.handover.export.col.name"), accessorKey: "name" },
  ], []);

  async function handleExport() {
    if (selected.length === 0) return;
    setExporting(true); setExportOk(false); setExportErr(null); setExportWarnings([]);
    try {
      // Slice 2.3: carry the active app's identity so the pack mints a named sidebar app on install.
      const { pack, warnings } = await exportVertical(selected, includeData, appIdentity);
      setExportWarnings(warnings);
      const blob = new Blob([JSON.stringify(pack, null, 2)], { type: "application/json" });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `vertical-pack${includeData ? "-with-data" : ""}.json`;
      a.click();
      URL.revokeObjectURL(url);
      setExportOk(true);
    } catch (e) {
      setExportErr(errorMessage(e));
    } finally {
      setExporting(false);
    }
  }

  return (
    <Card title={t("page.verticals.export.title")} icon={{ glyph: Download, name: "product" }}>
      <div className="flex flex-col gap-3">
        <p>{t("page.verticals.export.intro")}</p>
        {appLabel && <ScopedNotification>{fill(t("page.verticals.export.appNote"), { label: appLabel })}</ScopedNotification>}
        <DataTable
          label={t("page.handover.export.entities")}
          columns={columns}
          data={entities}
          getRowId={(e) => e.name}
          selectable
          rowSelection={Object.fromEntries(selected.map((n) => [n, true]))}
          onRowSelectionChange={(sel) => setSelected((prev) => applySelection(prev, sel))}
          maxHeight="15rem"
          bordered
        />
        <Checkbox
          label={t("page.verticals.export.includeData")}
          help={t("page.verticals.export.includeDataHelp")}
          checked={includeData}
          onChange={(e) => setIncludeData(e.target.checked)}
        />
        <div className="flex flex-wrap items-center gap-2">
          <Button variant="brand" iconLeft={Download} disabled={exporting || selected.length === 0} onClick={handleExport}>
            {exporting
              ? t("page.verticals.export.running")
              : `${t("page.verticals.export.run")}${selected.length > 0 ? ` (${selected.length})` : ""}${includeData ? t("page.verticals.export.withData") : ""}`}
          </Button>
          {exportOk && <Badge theme="success">{t("page.verticals.export.done")}</Badge>}
        </div>
        {exportErr && <ScopedNotification theme="error">{exportErr}</ScopedNotification>}
        {/* Slice 2.2: honest excluded-manifest — what the pack could not carry. Empty = complete. */}
        {exportOk && exportWarnings.length > 0 && (
          <div className="flex flex-col gap-1">
            <ScopedNotification theme="warning">
              <strong>{fill(t("page.verticals.export.excluded"), { n: exportWarnings.length })}</strong>
            </ScopedNotification>
            <ul className="slds-list_dotted">
              {exportWarnings.map((w, i) => <li key={i}>{w}</li>)}
            </ul>
          </div>
        )}
      </div>
    </Card>
  );
}

// ── Lộ trình — catalog verticals without a backend pack: direction, not product ──
function Roadmap({ roadmap }: { roadmap: Vertical[] }) {
  const dataHref = studioHref(projectSlugAt(usePathname()), "dung/du-lieu");
  const columns = useMemo<ColumnDef<Vertical>[]>(() => [
    { id: "label", header: t("page.verticals.roadmap.col.name"), accessorKey: "label" },
    { id: "industries", header: t("page.verticals.roadmap.col.industries"), accessorKey: "industries" },
    { id: "entities", header: t("page.verticals.roadmap.col.entities"), accessorFn: (v) => v.entities.join(" · ") },
    {
      id: "status",
      header: t("page.verticals.roadmap.col.status"),
      accessorFn: () => t("page.verticals.roadmap.none"),
      cell: () => <Badge>{t("page.verticals.roadmap.none")}</Badge>,
    },
    {
      id: "action",
      header: t("page.verticals.roadmap.col.action"),
      cell: () => <Link href={dataHref}>{t("page.verticals.roadmap.build")}</Link>,
    },
  ], [dataHref]);
  return (
    <section className="flex flex-col gap-2">
      <div>
        <h2 className="slds-text-heading_small">{t("page.verticals.roadmap.title")}</h2>
        <p className="slds-text-body_small slds-text-color_weak">{t("page.verticals.roadmap.note")}</p>
      </div>
      <DataTable label={t("page.verticals.roadmap.title")} columns={columns} data={roadmap} getRowId={(v) => v.name} bordered />
    </section>
  );
}

// ── Install history — GET /admin/verticals/installs, newest-first ────────────────
function InstallHistory({ installs, loading }: { installs: InstallRecord[]; loading: boolean }) {
  const columns = useMemo<ColumnDef<InstallRecord>[]>(() => [
    {
      id: "pack",
      header: t("page.verticals.history.col.pack"),
      accessorFn: (r) => VERTICALS.find((v) => v.name === r.slug)?.label ?? (r.slug || t("page.verticals.history.unnamed")),
    },
    { id: "version", header: t("page.verticals.history.col.version"), accessorFn: (r) => (r.version ? `v${r.version}` : "") },
    {
      id: "entities",
      header: t("page.verticals.history.col.entities"),
      accessorFn: (r) => (Array.isArray(r.result?.entitiesCreated) ? (r.result!.entitiesCreated as string[]).length : 0),
      meta: { numeric: true },
    },
    {
      id: "samples",
      header: t("page.verticals.history.col.samples"),
      accessorFn: (r) => (typeof r.result?.samplesInserted === "number" ? (r.result!.samplesInserted as number) : 0),
      meta: { numeric: true },
    },
    { id: "slug", header: t("page.verticals.history.col.slug"), accessorFn: (r) => r.slug ?? "" },
    { id: "at", header: t("page.verticals.history.col.at"), accessorFn: (r) => formatDateTime(r.installedAt) },
  ], []);

  if (loading) return <Loading>{t("state.loading")}</Loading>;
  if (installs.length === 0) {
    return (
      <EmptyState
        icon={History}
        title={t("page.verticals.history.emptyTitle")}
        description={t("page.verticals.history.emptyDesc")}
      />
    );
  }
  return (
    <DataTable
      label={t("page.verticals.history.label")}
      columns={columns}
      data={sortInstallsNewestFirst(installs)}
      getRowId={(r) => r.id}
      bordered
    />
  );
}

// ── Workspace khách (TENANT-PROVISION MS-B, operator-only) — ported from the deploy wizard ──
// GET /admin/tenants doubles as the permission probe: 200 → operator, render the panel;
// FORBIDDEN → not an operator, hide it with an honest note. Any OTHER error (network/500) must
// NOT claim "no permission" — that would overclaim — so it gets a distinct, retry-able message.
// Mounted only after the page's ensureAuth boot resolved, so the probe can never race a 401.
function WorkspaceKhachSection() {
  // Building INSIDE a customer (specs/062 L2, FR-017): the registry belongs to the Nexus tenant and every
  // registry route refuses a session token, so the area is one reminder — no probe, no list.
  // Read on first client render only: this section mounts after the page's boot effect, never on the server.
  const [inSession] = useState(() => inWorkspace());
  const [operator, setOperator] = useState<"loading" | "yes" | "no">(inSession ? "no" : "loading");
  const [operatorForbidden, setOperatorForbidden] = useState(true);
  const [tenants, setTenants] = useState<TenantSummary[]>([]);

  // State is written only from the probe's callbacks. A refresh after a create/retire keeps the
  // panel mounted and just swaps the list; the retry after a failed probe shows the spinner again.
  const [probe, setProbe] = useState(0);
  useEffect(() => {
    if (inSession) return;
    let alive = true;
    listTenants().then(
      (list) => { if (!alive) return; setTenants(list); setOperator("yes"); },
      (e) => { if (!alive) return; setOperatorForbidden(e instanceof ApiError && e.code === "FORBIDDEN"); setOperator("no"); },
    );
    return () => { alive = false; };
  }, [probe, inSession]);
  const refresh = () => setProbe((n) => n + 1);
  const retry = () => { setOperator("loading"); refresh(); };

  if (inSession) {
    return (
      <section className="flex flex-col gap-3">
        <ScopedNotification>{t("workspace.manageElsewhere")}</ScopedNotification>
      </section>
    );
  }

  return (
    <section className="flex flex-col gap-3">
      <div>
        <h2 className="slds-text-heading_small">{t("page.verticals.ws.section")}</h2>
        <p className="slds-text-color_weak">{t("page.verticals.ws.sectionIntro")}</p>
      </div>

      {operator === "loading" && <Loading>{t("page.verticals.ws.checking")}</Loading>}
      {operator === "yes" && <WorkspaceKhachPanel tenants={tenants} onChanged={refresh} />}
      {operator === "no" && (
        operatorForbidden ? (
          <ScopedNotification>{t("page.verticals.ws.forbidden")}</ScopedNotification>
        ) : (
          <div className="flex flex-col items-start gap-2">
            <ScopedNotification theme="error">{t("page.verticals.ws.probeFailed")}</ScopedNotification>
            <Button onClick={retry}>{t("action.retry")}</Button>
          </div>
        )
      )}
    </section>
  );
}

// Registry of existing customer tenants + a create-workspace form. Rendered ONLY when the
// GET /admin/tenants probe succeeds. Job here is create + hand over credentials + retire/purge —
// never claims the pack install above targets the new workspace.
function WorkspaceKhachPanel({ tenants, onChanged }: { tenants: TenantSummary[]; onChanged: () => void }) {
  const [creating, setCreating] = useState(false);
  const [reveal, setReveal] = useState<ProvisionedTenant | null>(null);
  // Retire = soft-delete a customer workspace. Until this landed the only way was hand-editing
  // MySQL, so a trial workspace could never be cleaned up from the product.
  const [retiring, setRetiring] = useState<TenantSummary | null>(null);
  const [retireBusy, setRetireBusy] = useState(false);
  // Purge = the second, irreversible half. Only offered on an ALREADY-RETIRED workspace, and only
  // after a dry run has said out loud how many rows are about to go — an operator should never be
  // asked to confirm a number nobody showed them.
  // Owner tier (specs/065 lát D): the prompt for converting admins to customer owners ("on") or back ("off").
  const [tierPrompt, setTierPrompt] = useState<{ tn: TenantSummary; on: boolean } | null>(null);
  const [tierBusy, setTierBusy] = useState(false);
  const [purging, setPurging] = useState<TenantSummary | null>(null);
  const [purgeBusy, setPurgeBusy] = useState(false);
  const [purgePreview, setPurgePreview] = useState<PurgeReport | null>(null);
  // Per-customer backups (specs/068): the panel opens in a modal from the row. A retired customer is
  // still backed up / downloaded, so the button is on every row.
  const [backupFor, setBackupFor] = useState<TenantSummary | null>(null);
  // "Mở Studio của khách" (specs/062 L2): the slug being opened. enterWorkspace reloads the page on
  // success, so it is only ever reset on failure.
  const [opening, setOpening] = useState<string | null>(null);
  const homeTenantId = useAuth().user?.tenantId;

  const openStudio = async (tn: TenantSummary) => {
    setOpening(tn.slug);
    try {
      await enterWorkspace(tn.slug);
    } catch (e) {
      toast.error(errorMessage(e));
      setOpening(null);
    }
  };

  const doRetire = async () => {
    if (!retiring) return;
    setRetireBusy(true);
    try {
      await retireTenant(retiring.id);
      toast.success(fill(t("page.verticals.ws.retiredToast"), { name: retiring.name }));
      setRetiring(null);
      onChanged();
    } catch (e) {
      // TENANT_SELF_RETIRE is the interesting one: show the server's own sentence rather than a
      // generic failure, because it explains WHY (you are standing in this workspace).
      toast.error(e instanceof ApiError ? e.message : t("page.verticals.ws.retireFailed"));
    } finally {
      setRetireBusy(false);
    }
  };

  const doTier = async () => {
    if (!tierPrompt) return;
    const { tn, on } = tierPrompt;
    setTierBusy(true);
    try {
      if (on) {
        const r = await setOwnerTier(tn.id);
        toast.success(fill(t("page.verticals.ws.ownerOnDone"), { n: r.converted })
          + (r.skippedWithRole > 0 ? fill(t("page.verticals.ws.ownerOnSkipped"), { m: r.skippedWithRole }) : ""));
        if (r.revokeFailed > 0) toast.error(fill(t("page.verticals.ws.ownerRevokeFailed"), { n: r.revokeFailed }));
      } else {
        const r = await revertOwnerTier(tn.id);
        toast.success(fill(t("page.verticals.ws.ownerOffDone"), { n: r.reverted }));
        if (r.revokeFailed > 0) toast.error(fill(t("page.verticals.ws.ownerRevokeFailed"), { n: r.revokeFailed }));
      }
      setTierPrompt(null);
      onChanged();
    } catch (e) {
      toast.error(e instanceof ApiError ? e.message : t("page.verticals.ws.ownerFailed"));
    } finally {
      setTierBusy(false);
    }
  };

  // Opening the purge dialog runs a DRY RUN first (server counts, deletes nothing) so the impact
  // list shows real numbers from this database, not a guess.
  const openPurge = async (tn: TenantSummary) => {
    setPurging(tn);
    setPurgePreview(null);
    try {
      setPurgePreview(await purgeTenant(tn.id, tn.slug, true));
    } catch (e) {
      toast.error(e instanceof ApiError ? e.message : t("page.verticals.ws.countFailed"));
    }
  };

  const doPurge = async () => {
    if (!purging) return;
    setPurgeBusy(true);
    try {
      const rep = await purgeTenant(purging.id, purging.slug, false);
      toast.success(fill(t("page.verticals.ws.purged"), { name: purging.name, rows: rep.totalRows.toLocaleString("vi-VN") })
        + (rep.backupsRemoved ? ` ${fill(t("backup.purgedFiles"), { n: rep.backupsRemoved })}` : ""));
      // The server says out loud when a backup file could NOT be removed (it is still on disk).
      if (rep.backupsNote) toast.error(rep.backupsNote);
      setPurging(null);
      setPurgePreview(null);
      onChanged();
    } catch (e) {
      // The server's own sentence explains WHY (not retired yet / wrong slug / schema drift).
      toast.error(e instanceof ApiError ? e.message : t("page.verticals.ws.purgeFailed"));
    } finally {
      setPurgeBusy(false);
    }
  };

  const columns = useMemo<ColumnDef<TenantSummary>[]>(() => [
    { id: "name", header: t("page.verticals.ws.col.name"), accessorKey: "name" },
    { id: "slug", header: t("page.verticals.ws.col.slug"), accessorKey: "slug" },
    {
      id: "status",
      header: t("page.verticals.ws.col.status"),
      accessorKey: "status",
      cell: ({ row: { original: tn } }) =>
        tn.retiredAt ? <Badge>{t("page.verticals.ws.retired")}</Badge>
          : tn.status === "active" ? (
            <>
              <Badge theme="success">{t("page.verticals.ws.active")}</Badge>
              {/* specs/068 lát B: shown while the write freeze has not lapsed (the list is refreshed after a switch). */}
              {tn.maintenanceUntil && <Badge theme="warning" className="slds-m-left_x-small">{fill(t("maintenance.badge"), { time: formatTime(tn.maintenanceUntil) })}</Badge>}
            </>
          ) : <Badge theme="warning">{tn.status}</Badge>,
    },
    {
      id: "admins",
      header: t("page.verticals.ws.col.admins"),
      accessorFn: (tn) => (tn.retiredAt ? t("page.verticals.ws.retiredNote") : fill(t("page.verticals.ws.admins"), { a: tn.usableAdminCount, o: tn.usableOwnerCount })),
    },
    {
      id: "actions",
      header: t("page.verticals.ws.col.actions"),
      cell: ({ row: { original: tn } }) => (
        <div className="flex items-center gap-2">
          {/* FR-012: an active, un-retired customer that is not the builder's own tenant. */}
          {!tn.retiredAt && tn.status === "active" && tn.id !== homeTenantId && (
            <Button
              variant="brand"
              aria-label={fill(t("workspace.openFor"), { name: tn.name })}
              disabled={opening !== null}
              onClick={() => void openStudio(tn)}
            >
              {opening === tn.slug ? t("workspace.opening") : t("workspace.open")}
            </Button>
          )}
          {/* specs/065 lát D: not on the builder's own workspace (BE answers 422) and not on a suspended one. */}
          {!tn.retiredAt && tn.status === "active" && tn.id !== homeTenantId && tn.convertibleAdminCount > 0 && (
            <Button aria-label={fill(t("page.verticals.ws.ownerOnFor"), { name: tn.name })} onClick={() => setTierPrompt({ tn, on: true })}>{t("page.verticals.ws.ownerOn")}</Button>
          )}
          {!tn.retiredAt && tn.status === "active" && tn.id !== homeTenantId && tn.usableOwnerCount > 0 && (
            <Button aria-label={fill(t("page.verticals.ws.ownerOffFor"), { name: tn.name })} onClick={() => setTierPrompt({ tn, on: false })}>{t("page.verticals.ws.ownerOff")}</Button>
          )}
          <Button aria-label={fill(t("backup.openFor"), { name: tn.name })} title={fill(t("backup.titleHint"), { name: tn.name })} onClick={() => setBackupFor(tn)}>
            {t("backup.title")}
          </Button>
          {tn.retiredAt ? (
            <Button variant="text-destructive" title={fill(t("page.verticals.ws.purgeTitle"), { slug: tn.slug })} onClick={() => void openPurge(tn)}>
              {t("page.verticals.ws.purge")}
            </Button>
          ) : (
            <Button variant="text-destructive" title={fill(t("page.verticals.ws.retireTitle"), { slug: tn.slug })} onClick={() => setRetiring(tn)}>
              {t("page.verticals.ws.retire")}
            </Button>
          )}
        </div>
      ),
    },
  ], [opening, homeTenantId]);

  if (reveal) {
    return <TenantCredentialsReveal result={reveal} onDone={() => { setReveal(null); onChanged(); }} />;
  }

  if (creating) {
    return (
      <CreateTenantForm
        onCancel={() => setCreating(false)}
        onCreated={(res) => {
          setCreating(false);
          if (res.oneTimeCredentials) {
            setReveal(res);
          } else {
            toast.success(fill(t("page.verticals.ws.created"), { slug: res.tenant.slug }));
            onChanged();
          }
        }}
      />
    );
  }

  return (
    <Card
      title={t("page.verticals.ws.title")}
      icon={{ glyph: Building2, name: "household" }}
      actions={
        <div className="flex items-center gap-2">
          <Badge variant="lightest">{t("page.verticals.ws.operatorOnly")}</Badge>
          <Button iconLeft={Plus} onClick={() => setCreating(true)}>{t("page.verticals.ws.create")}</Button>
        </div>
      }
    >
      <div className="flex flex-col gap-3">
        <p className="slds-text-color_weak">{t("page.verticals.ws.intro")}</p>
        {tenants.length === 0 ? (
          <ScopedNotification>{t("page.verticals.ws.none")}</ScopedNotification>
        ) : (
          <DataTable label={t("page.verticals.ws.table")} columns={columns} data={tenants} getRowId={(tn) => tn.id} maxHeight="24rem" bordered />
        )}
      </div>

      {backupFor && (
        <Modal
          open
          onOpenChange={(o) => { if (!o) setBackupFor(null); }}
          size="large"
          title={fill(t("backup.modalTitle"), { name: backupFor.name })}
          footer={<Button onClick={() => setBackupFor(null)}>{t("backup.close")}</Button>}
        >
          <TenantBackupPanel tenant={backupFor} onMaintenanceChange={onChanged} />
        </Modal>
      )}

      <ConfirmPrompt
        open={!!retiring}
        onOpenChange={(o) => { if (!o) setRetiring(null); }}
        title={retiring ? fill(t("page.verticals.ws.retireConfirmTitle"), { name: retiring.name }) : ""}
        danger
        confirmName={retiring?.slug ?? ""}
        confirmLabel={t("page.verticals.ws.retireConfirm")}
        loading={retireBusy}
        onConfirm={doRetire}
        impact={[
          { label: fill(t("page.verticals.ws.retireImpactAdmins"), { a: retiring?.usableAdminCount ?? 0, o: retiring?.usableOwnerCount ?? 0 }), tone: "danger" },
          { label: t("page.verticals.ws.retireImpactHooks"), tone: "danger" },
          { label: t("page.verticals.ws.retireImpactData"), tone: "neutral" },
        ]}
        description={
          <>
            {t("page.verticals.ws.retireDesc")} <strong>{t("page.verticals.ws.retireDescStrong")}</strong>
            {t("page.verticals.ws.retireDescEnd")}
          </>
        }
      />

      <ConfirmPrompt
        open={!!tierPrompt}
        onOpenChange={(o) => { if (!o) setTierPrompt(null); }}
        title={tierPrompt ? fill(t(tierPrompt.on ? "page.verticals.ws.ownerOnConfirmTitle" : "page.verticals.ws.ownerOffConfirmTitle"), { name: tierPrompt.tn.name }) : ""}
        confirmLabel={t(tierPrompt?.on === false ? "page.verticals.ws.ownerOff" : "page.verticals.ws.ownerOn")}
        loading={tierBusy}
        onConfirm={doTier}
        impact={tierPrompt
          ? [tierPrompt.on
            ? { label: fill(t("page.verticals.ws.ownerOnImpact"), { n: tierPrompt.tn.convertibleAdminCount }), tone: "danger" as const }
            : { label: fill(t("page.verticals.ws.ownerOffImpact"), { o: tierPrompt.tn.usableOwnerCount }), tone: "danger" as const }]
          : []}
        description={tierPrompt
          ? (tierPrompt.on
            ? <>{t("page.verticals.ws.ownerOnDesc")} {t("page.verticals.ws.ownerOnEscape")} {t("page.verticals.ws.ownerOwnerNote")}</>
            : <>{t("page.verticals.ws.ownerOffDesc")}</>)
          : undefined}
      />

      <ConfirmPrompt
        open={!!purging}
        onOpenChange={(o) => { if (!o) { setPurging(null); setPurgePreview(null); } }}
        title={purging ? fill(t("page.verticals.ws.purgeConfirmTitle"), { name: purging.name }) : ""}
        danger
        confirmName={purging?.slug ?? ""}
        confirmLabel={t("page.verticals.ws.purge")}
        loading={purgeBusy}
        onConfirm={doPurge}
        impact={
          purgePreview
            ? [
                {
                  label: fill(t("page.verticals.ws.purgeImpactRows"), {
                    rows: purgePreview.totalRows.toLocaleString("vi-VN"),
                    tables: purgePreview.tables.length,
                  }),
                  tone: "danger",
                },
                { label: t("page.verticals.ws.purgeImpactBackup"), tone: "danger" },
                { label: t("page.verticals.ws.purgeImpactGone"), tone: "neutral" },
              ]
            : [{ label: t("page.verticals.ws.purgeCounting"), tone: "neutral" }]
        }
        description={
          <>
            {t("page.verticals.ws.purgeDesc")} <strong>{t("page.verticals.ws.purgeDescStrong")}</strong>{" "}
            {t("page.verticals.ws.purgeDescEnd")}
          </>
        }
      />
    </Card>
  );
}
