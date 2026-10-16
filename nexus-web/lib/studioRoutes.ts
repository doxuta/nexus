// lib/studioRoutes.ts — the ONE table of pages inside a Studio project (specs/073 kh1a, architecture 1.3/1.4/1.7).
// Everything that links into /studio/du-an/<slug>/… goes through studioHref(); parseStudioPath() is its inverse.
// Pure, no React, no network: safeNext() and projectTokenMatches() are security gates, so they stay testable
// without a browser. `mounted` says which routes have a real page yet (kh1b1: dung/du-lieu; kh2: Màn hình + Tự động; kh3: Người dùng & quyền, Báo cáo, Thương hiệu, Chạy thử, Giao; kh4: Vận hành, Bảo trì).
import type { I18nKey } from "@/lib/i18n/vi";
import type { PanelKey } from "@/lib/workshopPanels";

/** phien = runs inside the customer's workspace session; nha = calls operator routes with the home token. */
export type RouteCtx = "phien" | "nha";
type ParamName = "bang" | "id";

export interface StudioRoute {
  /** Path after /studio/du-an/<slug>/ with `:bang` / `:id` placeholders — also the stable route id. */
  id: string;
  segments: readonly string[];
  params: readonly ParamName[];
  labelKey: I18nKey;
  ctx: RouteCtx;
  /** `context.panel` sent to the assistant (067). null = no code yet (four codes 067 adds) or hidden on `nha` pages. */
  aiPanel: PanelKey | null;
  /** URL query keys this page reads; any other key is dropped when building an href. */
  query: readonly string[];
  /** Name of the component mounted here, null while the page shows PendingPage. Set by the slice that mounts it. */
  mounts: string | null;
  /** "đã gắn" flag: true only once the page really works. Links to a not-yet-mounted route must not be drawn (FR-132). */
  mounted: boolean;
}

// `nha` pages call operator routes: no builder component may ever mount there (it would write into Nexus).
export const NHA_MOUNT_ALLOWLIST: readonly string[] = ["BackupsPage", "RestorePage", "RestoreProgress", "ProjectSettings"];

/** Marker in `query`: the page keeps the LayoutList state (`q`, `sort`, `order`, `view`, `<field>[op]`) in its URL. */
export const LIST_STATE = "~lista";
const LIST_KEY_RE = /^(?:q|sort|order|view|[a-z][a-z0-9_]{0,63}\[[a-z_]{1,16}\])$/;
const LIST_VALUE_RE = /^[^\u0000-\u001f\u007f]{1,200}$/;

const r = (path: string, labelKey: I18nKey, aiPanel: PanelKey | null, query: string[] = [], ctx: RouteCtx = "phien"): StudioRoute => {
  const segments = path.split("/");
  return {
    id: path,
    segments,
    params: segments.filter((s) => s.startsWith(":")).map((s) => s.slice(1) as ParamName),
    labelKey,
    ctx,
    aiPanel,
    query,
    mounts: null,
    mounted: false,
  };
};

// ?dateFrom/?dateTo/?ownerId of useRunFilters: a report page keeps them in its URL; changing ?bc= (built without them) drops them.
const RUN_FILTER_KEYS = ["dateFrom", "dateTo", "ownerId"] as const;

const mounted = (x: StudioRoute, component: string): StudioRoute => ({ ...x, mounts: component, mounted: true });

export const ROUTES: readonly StudioRoute[] = [
  mounted(r("dung/du-lieu", "studio.route.dataList", "bang", ["app"]), "EntityListPage"),
  mounted(r("dung/du-lieu/:bang", "studio.route.dataTable", "bang", ["truong", "moi"]), "EntityPage"),
  mounted(r("dung/du-lieu/so-do", "studio.route.dataDiagram", "bang", ["bang"]), "RelationMap"),
  mounted(r("dung/du-lieu/them-tu-goi", "studio.route.dataPack", "mau"), "AddFromPackPage"),
  mounted(r("dung/man-hinh", "studio.route.screens", "man-hinh", ["app", "bang", "man", "truong"]), "ScreensPage"),
  mounted(r("dung/man-hinh/:bang", "studio.route.screenTable", "man-hinh", ["app", "man", "che-do", "truong", "lien-quan"]), "ScreensPage"),
  mounted(r("dung/man-hinh/app", "studio.route.screenApps", "menu", ["app"]), "AppsPage"),
  mounted(r("dung/tu-dong", "studio.route.rules", "tu-dong", ["bang", "app"]), "AutomationPage"),
  mounted(r("dung/tu-dong/luat/moi", "studio.route.rule", "tu-dong", ["app", "bang", "khi", "ten"]), "RulePage"),
  mounted(r("dung/tu-dong/luat/:id", "studio.route.rule", "tu-dong", ["app"]), "RulePage"),
  mounted(r("dung/tu-dong/duyet", "studio.route.approvals", "duyet", ["bang", "app"]), "ApprovalPage"),
  mounted(r("dung/tu-dong/duyet/:id", "studio.route.approval", "duyet", ["bang", "app"]), "ApprovalPage"),
  mounted(r("dung/tu-dong/hang-doi", "studio.route.queues", "hang-doi", ["bang", "app"]), "QueuePage"),
  mounted(r("dung/tu-dong/hang-doi/:id", "studio.route.queue", "hang-doi", ["bang", "app"]), "QueuePage"),
  mounted(r("dung/tu-dong/cong-nhan", "studio.route.hooks", "webhook"), "HookPage"),
  mounted(r("dung/tu-dong/cong-nhan/:id", "studio.route.hook", "webhook"), "HookPage"),
  mounted(r("dung/tu-dong/lich-lam-viec", "studio.route.calendars", "tu-dong"), "CalendarPage"),
  mounted(r("dung/nguoi-dung-quyen", "studio.route.access", "vai-tro", ["bang", "app"]), "AccessPage"),
  mounted(r("dung/nguoi-dung-quyen/vai/:id", "studio.route.accessRole", "vai-tro"), "RolePage"),
  mounted(r("dung/nguoi-dung-quyen/quyen-truong", "studio.route.accessFields", "quyen-truong", ["bang", "truong"]), "FieldAclPage"),
  mounted(r("dung/nguoi-dung-quyen/doi", "studio.route.accessTeams", "vai-tro"), "TeamsPage"),
  mounted(r("dung/nguoi-dung-quyen/chia-se", "studio.route.accessSharing", "vai-tro"), "SharingPage"),
  mounted(r("dung/bao-cao", "studio.route.reports", "bao-cao", ["bc", "bang", "app", ...RUN_FILTER_KEYS]), "ReportsPage"),
  r("dung/bao-cao/:id", "studio.route.report", "bao-cao", ["bc", "bang", "app", ...RUN_FILTER_KEYS]),
  mounted(r("dung/bao-cao/tong-quan/:id", "studio.route.dashboard", "dashboard"), "DashboardPage"),
  mounted(r("dung/thuong-hieu", "studio.route.branding", "thuong-hieu"), "BrandingPage"),
  mounted(r("dung/chay-thu", "studio.route.trial", "chay-thu", ["app"]), "TrialPage"),
  mounted(r("dung/giao-cho-khach", "studio.route.deliver", "ban-giao"), "DeliverPage"),
  mounted(r("van-hanh/nguoi-dung", "studio.route.operUsers", "nguoi-dung", ["nguoi"]), "UserList"),
  mounted(r("van-hanh/nguoi-dung/dang-nhap-mot-lan", "studio.route.operSso", "van-hanh"), "SsoProviders"),
  mounted(r("van-hanh/nguoi-dung/nhat-ky", "studio.route.operAudit", "van-hanh", ["nguoi"]), "AuditLog"),
  mounted(r("van-hanh/du-lieu", "studio.route.operData", "van-hanh"), "DataTablesPage"),
  mounted(r("van-hanh/du-lieu/:bang", "studio.route.operRecords", "van-hanh", [LIST_STATE]), "EntityRecordsPage"),
  mounted(r("van-hanh/du-lieu/:bang/moi", "studio.route.operRecordNew", "van-hanh", ["cloneFrom", "pf_*"]), "RecordPage"),
  mounted(r("van-hanh/du-lieu/:bang/tao-nhieu", "studio.route.operBatch", "van-hanh"), "OperateBatchCreate"),
  mounted(r("van-hanh/du-lieu/:bang/nhap", "studio.route.operImport", "van-hanh"), "OperateImport"),
  mounted(r("van-hanh/du-lieu/:bang/:id", "studio.route.operRecord", "van-hanh", ["sua"]), "RecordPage"),
  mounted(r("van-hanh/du-lieu/:bang/thung-rac", "studio.route.operRecycle", "van-hanh"), "OperateRecycleBin"),
  mounted(r("van-hanh/tu-dong", "studio.route.operAuto", "van-hanh"), "OperateOverview"),
  mounted(r("van-hanh/tu-dong/lan-chay", "studio.route.operRuns", "van-hanh", ["luat"]), "WorkflowRunHistory"),
  mounted(r("van-hanh/tu-dong/email", "studio.route.operEmail", "van-hanh", ["trang-thai"]), "EmailLog"),
  mounted(r("van-hanh/tu-dong/viec-nen", "studio.route.operJobs", "van-hanh"), "BackgroundJobs"),
  mounted(r("van-hanh/tu-dong/cong-nhan", "studio.route.operInbound", "van-hanh", ["cong"]), "InboundLink"),
  mounted(r("van-hanh/tu-dong/khoa-ket-noi", "studio.route.operCredentials", "van-hanh"), "Credentials"),
  mounted(r("van-hanh/tu-dong/khoa-truy-cap", "studio.route.operKeys", "van-hanh"), "ApiKeys"),
  mounted(r("van-hanh/suc-khoe", "studio.route.operHealth", "van-hanh", ["khoang"]), "HealthPage"),
  mounted(r("bao-tri/lich-su", "studio.route.history", "bao-tri"), "HistoryPage"),
  mounted(r("bao-tri/lich-su/:id", "studio.route.version", "bao-tri", ["loai", "bang"]), "VersionPage"),
  mounted(r("bao-tri/sao-luu", "studio.route.backups", null, [], "nha"), "BackupsPage"),
  mounted(r("bao-tri/sao-luu/khoi-phuc/:id", "studio.route.restore", null, ["lan"], "nha"), "RestorePage"),
  mounted(r("bao-tri/kham", "studio.route.check", "bao-tri"), "DoctorScreen"),
  mounted(r("bao-tri/nang-cap", "studio.route.upgrade", "bao-tri"), "PackDiffPanel"),
  mounted(r("bao-tri/nang-cap/:id", "studio.route.upgradeOne", "bao-tri"), "UpgradePage"),
  mounted(r("cai-dat", "studio.route.settings", null, [], "nha"), "ProjectSettings"),
];

/** The four sub tabs of Vận hành and Bảo trì (architecture 1.4/3.4): `route` is the tab's own page, `short` the < 768px label. */
export const ZONE_TABS = {
  "van-hanh": [
    { route: "van-hanh/nguoi-dung", label: "studio.route.operUsers", short: "studio.operate.short.users" },
    { route: "van-hanh/du-lieu", label: "studio.route.operData", short: "studio.operate.short.data" },
    { route: "van-hanh/tu-dong", label: "studio.route.operAuto", short: "studio.operate.short.auto" },
    { route: "van-hanh/suc-khoe", label: "studio.route.operHealth", short: "studio.operate.short.health" },
  ],
  "bao-tri": [
    { route: "bao-tri/lich-su", label: "studio.route.history", short: "studio.maintain.short.history" },
    { route: "bao-tri/sao-luu", label: "studio.route.backups", short: "studio.maintain.short.backups" },
    { route: "bao-tri/kham", label: "studio.route.check", short: "studio.maintain.short.check" },
    { route: "bao-tri/nang-cap", label: "studio.route.upgrade", short: "studio.maintain.short.upgrade" },
  ],
} as const satisfies Record<string, readonly { route: string; label: I18nKey; short: I18nKey }[]>;

/** The tab (its `route` id) a page sits under; null for Dựng pages, Cài đặt and unknown paths. */
export function tabOfRoute(route: StudioRoute | null): string | null {
  const zone = route?.segments[0];
  if (!route || (zone !== "van-hanh" && zone !== "bao-tri")) return null;
  return ZONE_TABS[zone].find((x) => x.route.split("/")[1] === route.segments[1])?.route ?? null;
}

/** The zone (the strip's three links) a route sits in; Cài đặt belongs to Bảo trì. */
export function zoneOfRoute(route: StudioRoute | null): "dung" | "van-hanh" | "bao-tri" {
  const head = route?.segments[0];
  return head === "van-hanh" ? "van-hanh" : head === "bao-tri" || head === "cai-dat" ? "bao-tri" : "dung";
}

/** `?man=` of the Màn hình pages -> the LayoutBuilder view; anything else = undefined (the builder's own default). */
export function viewOfMan(man: string | undefined): "Form" | "Detail" | "List" | undefined {
  return man === "form" ? "Form" : man === "ho-so" ? "Detail" : man === "danh-sach" ? "List" : undefined;
}

export type RouteId = (typeof ROUTES)[number]["id"];

const BY_ID = new Map(ROUTES.map((x) => [x.id, x]));

export function isMounted(id: RouteId): boolean {
  return BY_ID.get(id)?.mounted === true;
}

/** The mounted page with no path parameter that the assistant calls panel `panel` (067), or null. */
export function routeForPanel(panel: string | null | undefined): RouteId | null {
  if (!panel) return null; // pages with no assistant code have aiPanel null — they must not match "no panel"
  return ROUTES.find((x) => x.mounted && x.ctx === "phien" && x.params.length === 0 && x.aiPanel === panel)?.id ?? null;
}

// ── Validation of every value that comes from a URL (FR-028) ──
export const SLUG_RE = /^[a-z0-9][a-z0-9_-]{0,63}$/; // = TenantSlugPattern on the server
export const NAME_RE = /^[a-z][a-z0-9_]*$/;
export const ID_RE = /^[A-Za-z0-9_-]{1,64}$/;
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const QUERY_RE: Record<string, RegExp> = { lan: UUID_RE, bang: NAME_RE, truong: NAME_RE, app: NAME_RE, moi: /^1$/, sua: /^1$/, ten: /^[^\u0000-\u001f\u007f]{1,120}$/ };
const queryRe = (key: string) => QUERY_RE[key] ?? ID_RE;
const paramRe = (p: ParamName) => (p === "bang" ? NAME_RE : ID_RE);
// `pf_<field>=<value>` seeds a new record's form (LayoutForm reads it): the prefix is declared as "pf_*" in a route's query list.
const PREFILL_KEY_RE = /^pf_[a-z][a-z0-9_]*$/;
const PREFILL_VALUE_RE = /^[^\u0000-\u001f\u007f]{1,200}$/;
// The words that are sibling pages of a record id under van-hanh/du-lieu/<bang>/: never a record id.
const RESERVED_RECORD_IDS = new Set(["nhap", "tao-nhieu", "thung-rac", "moi"]);
const paramOk = (route: StudioRoute, name: ParamName, v: string) =>
  paramRe(name).test(v) && !(route.id === "bao-tri/sao-luu/khoi-phuc/:id" && !UUID_RE.test(v)) && !(name === "id" && route.id === "van-hanh/du-lieu/:bang/:id" && RESERVED_RECORD_IDS.has(v));
const prefillPairs = (route: StudioRoute) => route.query.includes("pf_*");

const BASE = "/studio/du-an";

/** True under /studio/du-an/ — the one definition shared by the shell and the active-app lens. */
export const inProjectPath = (pathname: string): boolean => pathname.startsWith(`${BASE}/`);

type Query = Record<string, string | undefined>;

function queryString(route: StudioRoute, query: Query | undefined): string {
  const pairs: string[] = [];
  for (const key of route.query) {
    if (key === LIST_STATE) {
      for (const [k, v] of Object.entries(query ?? {})) {
        if (LIST_KEY_RE.test(k) && typeof v === "string" && LIST_VALUE_RE.test(v)) pairs.push(`${encodeURIComponent(k)}=${encodeURIComponent(v)}`);
      }
      continue;
    }
    if (key === "pf_*") continue;
    const v = query?.[key];
    if (typeof v === "string" && queryRe(key).test(v)) pairs.push(`${key}=${encodeURIComponent(v)}`);
  }
  if (prefillPairs(route)) {
    for (const [key, v] of Object.entries(query ?? {})) {
      if (PREFILL_KEY_RE.test(key) && typeof v === "string" && PREFILL_VALUE_RE.test(v)) pairs.push(`${key}=${encodeURIComponent(v)}`);
    }
  }
  return pairs.length ? `?${pairs.join("&")}` : "";
}

/**
 * The ONLY builder of Studio links. An invalid slug gives "/studio"; an invalid path parameter gives the project
 * root (which redirects to the default page) — a bad value never enters a path.
 */
export function studioHref(slug: string, routeId: RouteId, params: Partial<Record<ParamName, string>> = {}, query?: Query): string {
  if (!SLUG_RE.test(slug)) return "/studio";
  const route = BY_ID.get(routeId);
  if (!route) return `${BASE}/${slug}`;
  // A table literally named `app` collides with the static `…/app` page: use the ?bang= form on the parent.
  if (route.id === "dung/man-hinh/:bang" && params.bang === "app") {
    return studioHref(slug, "dung/man-hinh", {}, { ...query, bang: "app" });
  }
  const parts: string[] = [];
  for (const seg of route.segments) {
    if (!seg.startsWith(":")) {
      parts.push(seg);
      continue;
    }
    const name = seg.slice(1) as ParamName;
    const v = params[name];
    if (typeof v !== "string" || !paramOk(route, name, v)) return `${BASE}/${slug}`;
    parts.push(v);
  }
  return `${BASE}/${slug}/${parts.join("/")}${queryString(route, query)}`;
}

export interface ParsedStudioPath {
  slug: string;
  route: StudioRoute | null;
  params: Partial<Record<ParamName, string>>;
  query: Record<string, string>;
  /** `/studio/du-an/<slug>` with nothing after it — the caller redirects to the default page. */
  root: boolean;
}

// Static segments beat parameters, so `so-do` is a page and never a table (tables have no hyphen anyway).
const MATCH_ORDER = [...ROUTES].sort((a, b) => staticCount(b) - staticCount(a));
function staticCount(x: StudioRoute): number {
  return x.segments.filter((s) => !s.startsWith(":")).length;
}

/** Inverse of studioHref. null = not a project path or the slug is invalid. */
export function parseStudioPath(href: string): ParsedStudioPath | null {
  const qAt = href.search(/[?#]/);
  const path = qAt < 0 ? href : href.slice(0, qAt);
  const search = qAt >= 0 && href[qAt] === "?" ? href.slice(qAt + 1).split("#")[0] : "";
  if (!path.startsWith(`${BASE}/`)) return null;
  const [slug, ...rest] = path.slice(BASE.length + 1).split("/");
  if (!SLUG_RE.test(slug)) return null;
  const segs = rest.filter((s, i) => !(s === "" && i === rest.length - 1)); // one trailing slash is fine
  if (segs.some((s) => s === "" || s === "." || s === "..")) return null; // dot-segments never name a page
  if (segs.length === 0) return { slug, route: null, params: {}, query: {}, root: true };
  for (const route of MATCH_ORDER) {
    if (route.segments.length !== segs.length) continue;
    const params: Partial<Record<ParamName, string>> = {};
    const hit = route.segments.every((seg, i) => {
      if (!seg.startsWith(":")) return seg === segs[i];
      const name = seg.slice(1) as ParamName;
      if (!paramOk(route, name, segs[i])) return false;
      params[name] = segs[i];
      return true;
    });
    if (!hit) continue;
    const query: Record<string, string> = {};
    const sp = new URLSearchParams(search);
    for (const key of route.query) {
      if (key === LIST_STATE) {
        for (const [k, v] of sp) if (LIST_KEY_RE.test(k) && LIST_VALUE_RE.test(v)) query[k] = v;
        continue;
      }
      if (key === "pf_*") continue;
      const v = sp.get(key);
      if (v !== null && queryRe(key).test(v)) query[key] = v;
    }
    if (prefillPairs(route)) {
      for (const [key, v] of sp) if (PREFILL_KEY_RE.test(key) && PREFILL_VALUE_RE.test(v)) query[key] = v;
    }
    return { slug, route, params, query, root: false };
  }
  return { slug, route: null, params: {}, query: {}, root: false };
}

// ── routeForChange (specs/076 FR-071/FR-129): where an assistant proposal can be previewed ──
/** The slice of an assistant `change` the router reads. Names/keys are model-made: validated, never joined into a path. */
export interface ChangeForRoute {
  docType: string;
  docKey?: string;
  target?: { entity?: string; field?: string; view?: string; name?: string };
  op?: string;
}
export interface ChangeRoute {
  href: string;
  /** The current page already draws this kind: the assistant must not navigate. */
  stay: boolean;
  /** Target page, for labels. */
  route: RouteId;
}

// Closed table for `?man=` (FR-129): anything else is dropped.
const MAN_BY_VIEW: Record<string, string> = {
  form: "form", edit: "form", create: "form", detail: "ho-so", "ho-so": "ho-so", list: "danh-sach", "danh-sach": "danh-sach",
};
const okName = (v: string | undefined): boolean => v === undefined || (v.length <= 64 && NAME_RE.test(v));

/**
 * The page where the proposal's ghost can be drawn, or null (no valid target ⇒ no "Xem trước"). Every value goes
 * through studioHref(), so a bad `entity`/`field`/`docKey` never enters a path; the result is always inside
 * /studio/du-an/<slug>/.
 */
export function routeForChange(change: ChangeForRoute, current: ParsedStudioPath | null, slug: string): ChangeRoute | null {
  const t = change.target ?? {};
  if (!SLUG_RE.test(slug) || !okName(t.entity) || !okName(t.field) || !okName(t.name)) return null;
  if (change.docKey !== undefined && !ID_RE.test(change.docKey)) return null;
  const cur = current?.route ?? null;
  const here = (...ids: string[]) => cur !== null && ids.includes(cur.id);
  const done = (route: RouteId, stay: boolean, params: Partial<Record<ParamName, string>> = {}, query?: Query): ChangeRoute | null => {
    const href = studioHref(slug, route, params, query);
    return href === `${BASE}/${slug}` || href === "/studio" ? null : { href, stay, route };
  };
  switch (change.docType) {
    case "entity":
      return change.op !== "create" && t.entity
        ? done("dung/du-lieu/:bang", here("dung/du-lieu/:bang") && current?.params.bang === t.entity, { bang: t.entity })
        : done("dung/du-lieu", here("dung/du-lieu", "dung/du-lieu/:bang"));
    case "field":
      if (!t.entity) return null;
      if (here("dung/man-hinh/:bang") && current?.params.bang === t.entity) return done("dung/man-hinh/:bang", true, { bang: t.entity });
      return done("dung/du-lieu/:bang", here("dung/du-lieu/:bang") && current?.params.bang === t.entity, { bang: t.entity }, { truong: t.field });
    case "layout": {
      if (!t.entity) return null;
      const man = t.view ? MAN_BY_VIEW[t.view.toLowerCase()] : undefined;
      return done("dung/man-hinh/:bang", here("dung/man-hinh/:bang") && current?.params.bang === t.entity && (!man || current?.query.man === man), { bang: t.entity }, { man });
    }
    case "workflow":
      return change.docKey && change.op !== "create"
        ? done("dung/tu-dong/luat/:id", here("dung/tu-dong/luat/:id") && current?.params.id === change.docKey, { id: change.docKey })
        : done("dung/tu-dong", here("dung/tu-dong", "dung/tu-dong/luat/:id", "dung/tu-dong/luat/moi"));
    case "report":
    case "dashboard":
      return done("dung/bao-cao", here("dung/bao-cao", "dung/bao-cao/:id", "dung/bao-cao/tong-quan/:id"));
    case "access":
      return done("dung/nguoi-dung-quyen", here("dung/nguoi-dung-quyen"));
    case "branding":
      return done("dung/thuong-hieu", here("dung/thuong-hieu"));
    case "app":
      return done("dung/man-hinh/app", here("dung/man-hinh/app"), {}, { app: t.name });
    case "relationship":
      return done("dung/du-lieu/so-do", here("dung/du-lieu/so-do"));
    case "approval":
      return done("dung/tu-dong/duyet", here("dung/tu-dong/duyet", "dung/tu-dong/duyet/:id"));
    case "workqueue":
      return done("dung/tu-dong/hang-doi", here("dung/tu-dong/hang-doi", "dung/tu-dong/hang-doi/:id"));
    case "records":
      return t.entity ? done("van-hanh/du-lieu/:bang", here("van-hanh/du-lieu/:bang") && current?.params.bang === t.entity, { bang: t.entity }) : null;
    default:
      return null;
  }
}

/** The project code in a pathname, null when the path is not inside a valid project. */
// Where the builder face can still be re-entered (specs/073 FR-053): the Studio's own roots and any path the project
// route table parses. A path remembered before a route was deleted (/studio/apps/crm) falls back to /studio, not a 404.
const LIVE_STUDIO_PATHS = new Set(["/studio", "/studio/verticals"]);
export const isLiveBuilderPath = (p: string) => {
  const path = p.split(/[?#]/)[0];
  const parsed = parseStudioPath(p); // already rejects dot-segments; route null + not root = unknown sub-path
  return LIVE_STUDIO_PATHS.has(path) || (parsed !== null && (parsed.root || parsed.route !== null));
};

export function currentProjectSlug(pathname: string): string | null {
  return parseStudioPath(pathname)?.slug ?? null;
}

/** Where "manage working calendars" goes: the Studio page inside a project, the old admin screen elsewhere. */
export function calendarsHref(pathname: string | null): string {
  const slug = currentProjectSlug(pathname ?? "");
  return slug ? studioHref(slug, "dung/tu-dong/lich-lam-viec") : "/admin/business-calendars";
}

const OUTSIDE = new Set(["/studio", "/studio/moi", "/studio/tai-khoan"]);
const UNSAFE = /[\u0000-\u001f\u007f\\]|%(?:2e|2f|5c|00|0a|0d)/i;

/**
 * Open-redirect guard for `next` (FR-021): only same-origin Studio paths for the target project S pass; anything
 * else becomes the first page of S. Compares the path prefix with a trailing "/", so "S" never matches "SX".
 */
export function safeNext(raw: unknown, S: string): string {
  if (!SLUG_RE.test(S)) return "/studio";
  const fallback = studioHref(S, "dung/du-lieu");
  if (typeof raw !== "string" || raw === "" || UNSAFE.test(raw)) return fallback;
  const qAt = raw.search(/[?#]/);
  const path = qAt < 0 ? raw : raw.slice(0, qAt);
  if (path.includes("//") || path.split("/").some((s) => s === "." || s === "..")) return fallback;
  const base = `${BASE}/${S}`;
  if (OUTSIDE.has(path) || path === base || path.startsWith(`${base}/`)) return raw;
  return fallback;
}

/** What /auth/me says about the token the tab is carrying — the only evidence projectTokenMatches trusts. */
export interface TokenIdentity {
  /** Tenant of the token (carried by /auth/me); the backup page's id for the workshop itself. */
  tenantId?: string;
  isPlatformOperator?: boolean;
  workspace?: { tenantSlug: string; tenantName?: string } | null;
}

/**
 * True when the token in this tab belongs to project S (FR-022/FR-026): in a build session it must be S's session;
 * otherwise it must be the operator's own home token and S the home project. Closed when the server has not
 * answered or the home code could not be read.
 */
export function projectTokenMatches(S: string, me: TokenIdentity | null | undefined, home: string | null): boolean {
  if (!me) return false;
  if (me.workspace) return me.workspace.tenantSlug === S;
  return me.isPlatformOperator === true && home !== null && S === home;
}
