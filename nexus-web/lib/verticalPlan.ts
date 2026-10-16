// lib/verticalPlan.ts — W8 Vertical pack / deploy: pure view-model logic (unit-tested).
// Consumed by VerticalLibrary ("Cài mẫu & Bàn giao": card install flow · import · history).
// Backend contract: docs/api-conventions.md §12 (manifest · export · installs · dry-run plan).
// Every derivation here is a pure function so the component layer stays a thin renderer and the
// tricky bits (plan → diff rows, conflict severity, install status from ledger, result summary,
// pack-file parsing) are covered by verticalPlan.test.ts.

// ── Contract types (mirror backend json tags — camelCase) ────────────────────────

/** pack.manifest — self-describing header (optional; semver required WHEN present). */
export interface Manifest {
  name: string;
  slug?: string;
  version: string;
  description?: string;
  author?: string;
  minEngine?: string;
}

/** One field install would ADD as a discrete op (relation field or Pass-2b deferred rollup). */
export interface PlanField {
  entity: string;
  field: string;
  deferred: boolean;
}

/** A pre-flight probe warning surfaced by the dry-run. */
export interface PlanConflict {
  kind: string; // "unique_slot_held" | "layout_overwrite" | (future kinds)
  detail: string;
  entity?: string;
  field?: string;
}

/** Dry-run install plan — POST /admin/verticals/install?dryRun=true → { data: { plan } }. */
export interface InstallPlan {
  entitiesToCreate: string[];
  entitiesToSkip: string[];
  fieldsToAdd: PlanField[];
  nnToRegister: string[];
  nnToSkip: string[];
  layoutsToInstall: string[];
  rolesToCreate: string[];
  rolesToSkip: string[];
  /**
   * Inbound webhook hooks install will stand up (Pass 8e) vs. skip by name. Optional because a
   * pack predating PACK-HOOKS omits them — never assume the array exists.
   */
  hooksToCreate?: string[];
  hooksToSkip?: string[];
  /**
   * The late passes the wizard used to omit entirely. Optional for the same reason as hooks: a
   * plan produced by an older server has no such keys, and `?? []` must stay the only assumption.
   */
  dashboardsToCreate?: string[];
  dashboardsToSkip?: string[];
  workflowsToCreate?: string[];
  workflowsToSkip?: string[];
  reportsToCreate?: string[];
  reportsToSkip?: string[];
  workQueuesToCreate?: string[];
  workQueuesToSkip?: string[];
  /** Approvals read as "<entity>:<name>". */
  approvalsToCreate?: string[];
  approvalsToSkip?: string[];
  teamsToCreate?: string[];
  teamsToSkip?: string[];
  samplesCount: number;
  conflicts: PlanConflict[];
  /**
   * Install notes the dry run already predicts, worded exactly as the install's result.warnings
   * (queue #104: no default working calendar ⇒ SLA rules run on wall-clock time). Never gates the
   * confirm. Optional: an older server omits the key.
   */
  warnings?: string[];
}

/** Real install result — POST /admin/verticals/install → { data: { result } }. */
export interface InstallResult {
  entitiesCreated: string[];
  entitiesSkipped: string[];
  relationFieldsAdded: string[];
  fieldsSkipped: string[];
  layoutsInstalled: string[];
  rolesCreated: string[];
  rolesSkipped: string[];
  nnRelationsCreated: string[];
  nnRelationsSkipped: string[];
  deferredFieldsAdded: string[];
  samplesInserted: number;
  linksCreated: number;
  dashboardsCreated: string[];
  dashboardsSkipped: string[];
  workflowsCreated?: string[];
  reportsCreated?: string[];
  /** Queue #720: working calendars and shared list views the install created. */
  calendarsCreated?: string[];
  viewsCreated?: string[];
  teamsCreated?: string[];
  /** PACK-QUEUES: "Việc Hôm Nay" queues the install seeded. */
  workQueuesCreated?: string[];
  /** Non-fatal install notes (KHUON-SONG-P1: provenance skipped/failed) — surface, never drop. */
  warnings?: string[];
}

/** One row of the install ledger — GET /admin/verticals/installs → { data: { installs } }. */
export interface InstallRecord {
  id: string;
  slug: string;
  version: string;
  installedAt: string; // ISO 8601
  result?: Partial<InstallResult> & Record<string, unknown>;
}

/** A vertical pack (export payload / imported file). Metadata-first; samples optional. */
export interface Pack {
  manifest?: Manifest;
  entities?: Array<{ name: string; label?: string; fields?: unknown[] }>;
  relationships?: unknown[];
  nnRelationships?: unknown[] | null;
  deferredFields?: unknown[];
  layouts?: unknown[];
  roles?: unknown[];
  samples?: unknown[];
}

// ── Diff-table view model (Step 2) ──────────────────────────────────────────────

export type DiffTone = "create" | "skip" | "info";

/** One entity row in a diff section — tone drives the colour (teal create / muted skip). */
export interface DiffRow {
  label: string;
  tone: DiffTone;
  /** Optional sublabel (e.g. "hoãn — rollup" for deferred fields). */
  note?: string;
}

/** A grouped section of the diff table (entities, fields-by-entity, N-N, layouts, roles). */
export interface DiffSection {
  key: string;
  title: string;
  rows: DiffRow[];
  /** Count shown in the section header (rows.length by default; overridden for samples). */
  count: number;
}

/**
 * planToDiffSections — turn a raw dry-run plan into the grouped, tone-tagged sections the wizard
 * renders as a diff table. Empty sections are dropped so the table only shows what actually moves.
 * fieldsToAdd is grouped by entity; deferred (Pass-2b rollup) fields carry a "hoãn" note.
 */
/**
 * The install passes that run after roles, in execution order. One table instead of six
 * near-identical blocks — and adding a pass means adding a row here, which is the point: the
 * previous shape made "forget to surface it" the default.
 */
const LATE_PASS_SECTIONS: Array<{
  key: string;
  title: string;
  note?: string;
  create: (p: InstallPlan) => string[] | undefined;
  skip: (p: InstallPlan) => string[] | undefined;
}> = [
  { key: "teams", title: "Đội", create: (p) => p.teamsToCreate, skip: (p) => p.teamsToSkip },
  { key: "dashboards", title: "Bảng theo dõi", create: (p) => p.dashboardsToCreate, skip: (p) => p.dashboardsToSkip },
  {
    key: "workflows",
    title: "Luật tự động",
    note: "sẽ chạy ngay sau khi cài",
    create: (p) => p.workflowsToCreate,
    skip: (p) => p.workflowsToSkip,
  },
  { key: "reports", title: "Báo cáo", create: (p) => p.reportsToCreate, skip: (p) => p.reportsToSkip },
  { key: "workQueues", title: "Hàng việc", create: (p) => p.workQueuesToCreate, skip: (p) => p.workQueuesToSkip },
  {
    key: "approvals",
    title: "Quy trình duyệt",
    note: "sẽ chặn bản ghi cho tới khi có người duyệt",
    create: (p) => p.approvalsToCreate,
    skip: (p) => p.approvalsToSkip,
  },
  {
    key: "hooks",
    title: "Đầu nhận tín hiệu",
    note: "endpoint nhận tín hiệu — secret sinh mới",
    create: (p) => p.hooksToCreate,
    skip: (p) => p.hooksToSkip,
  },
];

export function planToDiffSections(plan: InstallPlan): DiffSection[] {
  const sections: DiffSection[] = [];

  // Entities — create (teal) then skip (muted).
  const entRows: DiffRow[] = [
    ...plan.entitiesToCreate.map((n) => ({ label: n, tone: "create" as const })),
    ...plan.entitiesToSkip.map((n) => ({ label: n, tone: "skip" as const, note: "đã có" })),
  ];
  if (entRows.length) {
    sections.push({ key: "entities", title: "Đối tượng", rows: entRows, count: entRows.length });
  }

  // Fields to add — grouped by entity, deferred rollups tagged.
  if (plan.fieldsToAdd.length) {
    const byEntity = new Map<string, DiffRow[]>();
    for (const f of plan.fieldsToAdd) {
      const rows = byEntity.get(f.entity) ?? [];
      rows.push({
        label: f.field,
        tone: "create",
        note: f.deferred ? "hoãn — rollup (Pass 2b)" : undefined,
      });
      byEntity.set(f.entity, rows);
    }
    for (const [entity, rows] of byEntity) {
      sections.push({
        key: `fields:${entity}`,
        title: `Trường · ${entity}`,
        rows,
        count: rows.length,
      });
    }
  }

  // N-N relationships.
  const nnRows: DiffRow[] = [
    ...plan.nnToRegister.map((n) => ({ label: n, tone: "create" as const })),
    ...plan.nnToSkip.map((n) => ({ label: n, tone: "skip" as const, note: "đã đăng ký" })),
  ];
  if (nnRows.length) {
    sections.push({ key: "nn", title: "Quan hệ N-N", rows: nnRows, count: nnRows.length });
  }

  // Layouts (upsert — always "install", overwrite warned separately in conflicts).
  if (plan.layoutsToInstall.length) {
    const rows = plan.layoutsToInstall.map((n) => ({ label: n, tone: "info" as const }));
    sections.push({ key: "layouts", title: "Bố cục", rows, count: rows.length });
  }

  // Roles.
  const roleRows: DiffRow[] = [
    ...plan.rolesToCreate.map((n) => ({ label: n, tone: "create" as const })),
    ...plan.rolesToSkip.map((n) => ({ label: n, tone: "skip" as const, note: "đã có" })),
  ];
  if (roleRows.length) {
    sections.push({ key: "roles", title: "Vai trò", rows: roleRows, count: roleRows.length });
  }

  // Everything the backend plans in the late passes. These were all decided server-side long
  // before the wizard read them, so "cài khuôn này gồm những gì?" answered while omitting that
  // install stands up rules that fire, queues that route work, approval gates that block records,
  // and a LIVE endpoint that can move money. An operator clicking "Cài" is consenting to this
  // list; a list that names half of what happens collects consent for the wrong thing.
  for (const g of LATE_PASS_SECTIONS) {
    const rows: DiffRow[] = [
      ...(g.create(plan) ?? []).map((n) => ({ label: n, tone: "create" as const, note: g.note })),
      ...(g.skip(plan) ?? []).map((n) => ({ label: n, tone: "skip" as const, note: "đã có" })),
    ];
    if (rows.length) sections.push({ key: g.key, title: g.title, rows, count: rows.length });
  }

  return sections;
}

// ── Conflict severity (Step 2 panel) ────────────────────────────────────────────

export type ConflictSeverity = "hard" | "soft";

export interface ConflictMeta {
  severity: ConflictSeverity;
  /** Short human label for the severity. */
  label: string;
  /** Tone token key ("dg" danger for hard, "wn" warn for soft). */
  tone: "dg" | "wn";
}

/**
 * conflictSeverity — map a conflict `kind` to its display severity.
 *   unique_slot_held → HARD (danger): install WILL hit 409 RECORD_DUPLICATE (trap B3).
 *   layout_overwrite → SOFT (warn): idempotent-safe, only a heads-up.
 * Unknown kinds default to HARD (fail-loud — an unrecognised conflict should not be dismissed).
 */
export function conflictSeverity(kind: string): ConflictMeta {
  switch (kind) {
    case "unique_slot_held":
      return { severity: "hard", label: "Sẽ dính trùng khi cài", tone: "dg" };
    case "layout_overwrite":
      return { severity: "soft", label: "Ghi đè bố cục hiện có", tone: "wn" };
    case "upgrade_available":
      return { severity: "soft", label: "Gói đã cài — dùng Nâng cấp", tone: "wn" };
    default:
      return { severity: "hard", label: "Cảnh báo", tone: "dg" };
  }
}

/** Split conflicts into hard (danger) and soft (warn) buckets, preserving order. */
export function partitionConflicts(conflicts: PlanConflict[]): {
  hard: PlanConflict[];
  soft: PlanConflict[];
} {
  const hard: PlanConflict[] = [];
  const soft: PlanConflict[] = [];
  for (const c of conflicts) {
    if (conflictSeverity(c.kind).severity === "hard") hard.push(c);
    else soft.push(c);
  }
  return { hard, soft };
}

/**
 * requiresTypeName — does Step 3 need the type-name ConfirmDialog variant?
 * Task binary: type-name WHEN conflicts exist, simple confirm when clean (zero conflicts).
 * ANY conflict (hard unique OR soft layout-overwrite) forces the admin to acknowledge.
 */
export function requiresTypeName(plan: InstallPlan): boolean {
  return plan.conflicts.length > 0;
}

/** planIsClean — zero conflicts → the wizard shows the green "Sẵn sàng cài" banner. */
export function planIsClean(plan: InstallPlan): boolean {
  return plan.conflicts.length === 0;
}

// ── Install status from ledger (VerticalLibrary card) ───────────────────────────

export interface InstallStatus {
  installed: boolean;
  version?: string;
  installedAt?: string;
}

/**
 * installStatusForSlug — derive a card's install status from the append-only ledger.
 * The ledger is history: the NEWEST row per slug is the current install. When no row exists the
 * card is "Chưa cài" (honest — a pack installed before the ledger existed simply isn't recorded;
 * the dry-run reveals the real on-disk state via its skips).
 */
export function installStatusForSlug(slug: string, installs: InstallRecord[]): InstallStatus {
  const rows = installs
    .filter((r) => r.slug === slug)
    .sort((a, b) => tsOf(b.installedAt) - tsOf(a.installedAt));
  if (rows.length === 0) return { installed: false };
  const latest = rows[0];
  return { installed: true, version: latest.version || undefined, installedAt: latest.installedAt };
}

/** sortInstallsNewestFirst — defensive sort for the install-history list (backend is newest-first,
 *  but never trust order for a UI that must be right). */
export function sortInstallsNewestFirst(installs: InstallRecord[]): InstallRecord[] {
  return [...installs].sort((a, b) => tsOf(b.installedAt) - tsOf(a.installedAt));
}

function tsOf(iso: string | undefined): number {
  if (!iso) return 0;
  const t = Date.parse(iso);
  return Number.isNaN(t) ? 0 : t;
}

// ── Pack contents preview (VerticalLibrary "Xem trước nội dung") ─────────────────

/** One entity the pack ships — `exists` = already present in the current workspace. */
export interface PackContentsEntity {
  name: string;
  exists: boolean;
}

/**
 * Compact, REAL view of what a pack contains, derived from a dry-run plan (never written).
 * Counts are pack-total (create + already-present) so the card preview describes the PACK,
 * not just the delta — an installed pack still shows its full contents (tagged "đã có").
 */
export interface PackContents {
  entities: PackContentsEntity[];
  /** Discrete field ops the install would run now (relation fields on existing entities…). */
  fieldsToAddCount: number;
  /** Deferred Pass-2b rollup fields among fieldsToAdd. */
  rollupCount: number;
  /** Total N-N relationships the pack ships (to register + already registered). */
  nnCount: number;
  layoutsCount: number;
  /** Total roles the pack ships (to create + already present). */
  rolesCount: number;
  /** Inbound hooks the pack ships (to create + already present). */
  hooksCount: number;
  /** Late-pass automation the pack ships: workflows + approvals + work queues. */
  automationCount: number;
  /** Dashboards + reports the pack ships. */
  insightsCount: number;
  teamsCount: number;
  samplesCount: number;
}

/**
 * packContentsFromPlan — turn a dry-run InstallPlan into the card preview view-model.
 * Entities keep plan order: to-create first (new), then skips (already in the workspace).
 * This is the honest pre-install answer to "pack này chứa gì?" using live backend data.
 */
/** Length of an optional plan list — older servers omit the key entirely. */
const len = (xs: string[] | undefined) => (xs ?? []).length;

export function packContentsFromPlan(plan: InstallPlan): PackContents {
  const entities: PackContentsEntity[] = [
    ...plan.entitiesToCreate.map((name) => ({ name, exists: false })),
    ...plan.entitiesToSkip.map((name) => ({ name, exists: true })),
  ];
  return {
    entities,
    fieldsToAddCount: plan.fieldsToAdd.length,
    rollupCount: plan.fieldsToAdd.filter((f) => f.deferred).length,
    nnCount: plan.nnToRegister.length + plan.nnToSkip.length,
    layoutsCount: plan.layoutsToInstall.length,
    rolesCount: plan.rolesToCreate.length + plan.rolesToSkip.length,
    hooksCount: len(plan.hooksToCreate) + len(plan.hooksToSkip),
    automationCount:
      len(plan.workflowsToCreate) + len(plan.workflowsToSkip) +
      len(plan.approvalsToCreate) + len(plan.approvalsToSkip) +
      len(plan.workQueuesToCreate) + len(plan.workQueuesToSkip),
    insightsCount:
      len(plan.dashboardsToCreate) + len(plan.dashboardsToSkip) +
      len(plan.reportsToCreate) + len(plan.reportsToSkip),
    teamsCount: len(plan.teamsToCreate) + len(plan.teamsToSkip),
    samplesCount: plan.samplesCount,
  };
}

// ── Manifest display (Step 1) ───────────────────────────────────────────────────

export interface ManifestDisplay {
  name: string;
  slug?: string;
  version?: string;
  description?: string;
  author?: string;
}

/**
 * manifestDisplay — normalise a (possibly absent / partial) manifest for the Step-1 summary.
 * A manifest-less pack (hand-written / legacy) is valid; we fall back to a neutral name and leave
 * version undefined so the UI can say "không khai báo" rather than invent one.
 */
export function manifestDisplay(m: Manifest | undefined, fallbackName: string): ManifestDisplay {
  if (!m) return { name: fallbackName };
  return {
    name: m.name?.trim() || fallbackName,
    slug: m.slug?.trim() || undefined,
    version: m.version?.trim() || undefined,
    description: m.description?.trim() || undefined,
    author: m.author?.trim() || undefined,
  };
}

// ── Install-result summary (Step 3) ─────────────────────────────────────────────

export interface SummaryLine {
  label: string;
  count: number;
}

/**
 * installResultSummary — condense a real InstallResult into the headline counts the success screen
 * shows. Zero-count lines are dropped so the summary reads as "what actually happened".
 */
export function installResultSummary(r: InstallResult): SummaryLine[] {
  const lines: SummaryLine[] = [
    { label: "Đối tượng đã tạo", count: r.entitiesCreated?.length ?? 0 },
    { label: "Trường quan hệ đã thêm", count: r.relationFieldsAdded?.length ?? 0 },
    { label: "Trường tổng hợp (rollup) đã thêm", count: r.deferredFieldsAdded?.length ?? 0 },
    { label: "Quan hệ N-N đã đăng ký", count: r.nnRelationsCreated?.length ?? 0 },
    { label: "Bố cục đã cài", count: r.layoutsInstalled?.length ?? 0 },
    { label: "Vai trò đã tạo", count: r.rolesCreated?.length ?? 0 },
    { label: "Bản ghi mẫu đã chèn", count: r.samplesInserted ?? 0 },
    { label: "Liên kết N-N đã tạo", count: r.linksCreated ?? 0 },
    { label: "Dashboard đã tạo", count: r.dashboardsCreated?.length ?? 0 },
    { label: "Tự động hoá đã tạo", count: r.workflowsCreated?.length ?? 0 },
    { label: "Báo cáo đã tạo", count: r.reportsCreated?.length ?? 0 },
    { label: "Đội nhóm đã tạo", count: r.teamsCreated?.length ?? 0 },
    { label: "Lịch làm việc đã tạo", count: r.calendarsCreated?.length ?? 0 },
    { label: "Chế độ xem dùng chung đã tạo", count: r.viewsCreated?.length ?? 0 },
    { label: "Hàng đợi Việc Hôm Nay đã tạo", count: r.workQueuesCreated?.length ?? 0 },
  ];
  return lines.filter((l) => l.count > 0);
}

// ── Pack-file parsing (Import) ───────────────────────────────────────────────────

export type ParsePackResult =
  | { ok: true; pack: Pack; manifest: ManifestDisplay; entityCount: number }
  | { ok: false; error: string };

/**
 * parsePackFile — parse + shape-validate an imported JSON string as a vertical pack.
 * Accepts EITHER a bare pack ({manifest,entities,...}) OR an export envelope ({data:{pack:{...}}})
 * so a user can drop in a file downloaded straight from the Export button. A pack must carry at
 * least one entity to be installable; everything else is optional.
 */
export function parsePackFile(text: string): ParsePackResult {
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    return { ok: false, error: "Tệp không phải JSON hợp lệ." };
  }
  if (!raw || typeof raw !== "object") {
    return { ok: false, error: "Nội dung tệp không phải một object pack." };
  }

  // Unwrap the export envelope { data: { pack } } if present.
  const obj = raw as Record<string, unknown>;
  const envelope = obj.data as Record<string, unknown> | undefined;
  const packCandidate = (envelope?.pack ?? obj) as Record<string, unknown>;

  const entities = packCandidate.entities;
  if (!Array.isArray(entities) || entities.length === 0) {
    return { ok: false, error: "Pack không có đối tượng nào (thiếu mảng \"entities\")." };
  }
  // Every entity needs a name — otherwise install can't key it.
  const named = entities.every(
    (e) => e && typeof e === "object" && typeof (e as Record<string, unknown>).name === "string",
  );
  if (!named) {
    return { ok: false, error: "Có đối tượng thiếu tên (\"name\") — pack không hợp lệ." };
  }

  const pack = packCandidate as Pack;
  const manifest = manifestDisplay(pack.manifest, "Pack nhập từ tệp");
  return { ok: true, pack, manifest, entityCount: entities.length };
}
