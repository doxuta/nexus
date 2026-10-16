"use client";

// AppShell — the customer face's frame (specs/038 T017, contracts/shell.md): SLDS GlobalHeader
// (search/⌘K, pins, help, Setup, bell, account) + GlobalNavigation (app launcher, app name, tabs)
// stacked over the page. Used by the (app) route group and by app/admin/layout.tsx.
// The breadcrumb + "lên cấp" button sit in a thin strip under the nav until each screen moves them
// into its own PageHeader.
//
// E3 W3a (shell consolidation): this shell now also covers the dynamic entity family
// (/[entity]/*) plus /approvals /notifications /profile /settings /entities, which previously
// each mounted their own copy of Sidebar+Topbar (or, for LayoutList/Detail/Form screens, computed
// entity-aware crumbs from metadata). Crumb + search-placeholder derivation below reproduces that
// per-screen logic exactly (byte-identical strings) so consolidating the shell changes nothing
// visible — see entityCrumbs()/SEARCH_PLACEHOLDERS.
import { useEffect } from "react";
import { usePathname } from "next/navigation";
import { ArrowLeft } from "lucide-react";
import { parentPath, type Crumb } from "@/components/shell/Topbar";
import { Breadcrumbs } from "@/components/slds/Breadcrumbs";
import { ShellGlobalHeader } from "@/components/slds/shell/GlobalHeader";
import { MaintenanceBanner } from "@/components/shell/MaintenanceBanner";
import { PreviewBanner } from "@/components/shell/PreviewBanner";
import { ShellGlobalNavigation } from "@/components/slds/shell/GlobalNavigation";
import { t } from "@/lib/i18n";
import { useEntityLabel } from "@/lib/metadata/hooks";
import { useActiveApp } from "@/lib/apps";
import { useGuardedRouter } from "@/lib/navigation-blocker";
import { recordRouteVisit } from "@/lib/recents";

// segment[0] values that are fixed (app)-group routes, not dynamic entity slugs.
// Phase 4 IA cleanup: dead mappings removed — "dashboard"/"audit" were legacy top-level
// redirect stubs (deleted) and "/studio/*" mounts its own layout, so none of those segments
// ever reach this shell.
const STATIC_LABELS: Record<string, string> = {
  home: "Trang chủ",
  dashboards: "Bảng điều khiển",
  reports: "Báo cáo",
  approvals: "Phê duyệt",
  notifications: "Thông báo",
  profile: "Hồ sơ của tôi",
  settings: "Cài đặt",
  entities: "Danh mục đối tượng",
  about: "Giới thiệu Nexus",
};

// Phase 4 IA cleanup: /admin/* renders inside this SAME shell (app/admin/layout.tsx wraps
// children in <AppShell> behind its RequireAdmin gate) so there is exactly ONE App face.
// Labels mirror the Setup menu items (GlobalHeader) so nav text and breadcrumb text never disagree.
// Kept OUT of STATIC_LABELS on purpose: keys like "users"/"roles" would otherwise shadow a
// tenant entity of the same name in isEntitySegment().
const ADMIN_LABELS: Record<string, string> = {
  users: "Người dùng",
  roles: "Vai trò & Phân quyền",
  teams: "Nhóm",
  "field-acl": "Phân quyền Field",
  sso: "SSO / Đăng nhập một lần",
  credentials: t("slds.globalHeader.credentials"),
  "business-calendars": "Lịch làm việc",
  audit: "Nhật ký",
  doctor: "Bác sĩ hệ thống",
  sharing: "Chia sẻ",
  jobs: t("shell.nav.jobs"),
  // Without this segmentLabel would capitalize the machine name: "Email-log".
  "email-log": t("shell.nav.emailLog"),
  usage: t("shell.nav.usage"),
  health: t("shell.nav.health"),
};

// Children of /settings (the customer owner's area, specs/065). Kept OUT of STATIC_LABELS for the same reason as
// ADMIN_LABELS: a key like "users" there would shadow a tenant entity of that name in isEntitySegment().
const SETTINGS_LABELS: Record<string, string> = {
  users: t("settings.users.crumb"),
};

// Per-route search placeholder fidelity — matches the text each screen passed to its own
// <Topbar> before shell consolidation. Entity routes get a generic "Tìm trong <label>…" below
// (entityCrumbs' caller); routes absent here fall back to GlobalHeader's own default.
const SEARCH_PLACEHOLDERS: Record<string, string> = {
  approvals: "Tìm yêu cầu phê duyệt…",
  notifications: "Tìm trong thông báo…",
  profile: "Tìm cài đặt hồ sơ…",
};

// /[entity]/<segment2> label — mirrors RecycleBinScreen / ImportScreen crumbs.
const ENTITY_SUBROUTE_LABELS: Record<string, string> = {
  deleted: "Thùng rác",
  import: "Nhập từ CSV",
  batch: "Thêm nhiều",
};

function isEntitySegment(segment: string): boolean {
  return segment !== "" && !(segment in STATIC_LABELS);
}

// A record-id path segment (dashboards/:id, reports/:id, admin detail routes) is an opaque UUID —
// capitalizing it leaks the raw "6307638f-…" into the crumb. Label it "Chi tiết" like entityCrumbs
// does for /[entity]/[id]. The 8-4-4-4-12 hex shape never collides with a real route label.
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
function segmentLabel(segment: string, dict: Record<string, string>): string {
  if (dict[segment]) return dict[segment];
  if (UUID_RE.test(segment)) return "Chi tiết";
  return segment.charAt(0).toUpperCase() + segment.slice(1);
}

// W0-QW7: crumbs mang href (Crumb[]) — mỗi cấp là link sống. Labels giữ byte-identical
// với bản string[] cũ; href tích luỹ theo path segment.
export function staticCrumbs(segments: string[]): Crumb[] {
  if (segments.length === 0) return [{ label: "Trang chủ", href: "/home" }];
  return segments.map((s, i) => ({
    // Children of /settings read their own dictionary (specs/065): "Cài đặt / Người dùng".
    label: segmentLabel(s, i > 0 && segments[0] === "settings" ? SETTINGS_LABELS : STATIC_LABELS),
    href: "/" + segments.slice(0, i + 1).join("/"),
  }));
}

// /admin/* crumbs: "Quản trị / <screen>". /admin itself redirects to /admin/users, so the
// root crumb link is always safe; unknown deeper segments fall back to capitalize.
export function adminCrumbs(segments: string[]): Crumb[] {
  return [
    { label: "Quản trị", href: "/admin" },
    ...segments.slice(1).map((s, i) => ({
      label: segmentLabel(s, ADMIN_LABELS),
      href: "/" + segments.slice(0, i + 2).join("/"),
    })),
  ];
}

// Mirrors LayoutListScreen/LayoutDetailScreen/LayoutFormScreen/RecycleBinScreen/ImportScreen crumbs:
//   /[entity]            → ["Dữ liệu", label]
//   /[entity]/new        → ["Dữ liệu", label, "Tạo mới"]
//   /[entity]/[id]       → ["Dữ liệu", label, "Chi tiết"]
//   /[entity]/[id]/edit  → ["Dữ liệu", label, "Sửa"]
//   /[entity]/deleted    → ["Dữ liệu", label, "Thùng rác"]
//   /[entity]/import     → ["Dữ liệu", label, "Nhập từ CSV"]
//   /[entity]/batch      → ["Dữ liệu", label, "Thêm nhiều"]
// "Dữ liệu" → /entities (danh bạ đối tượng — route thật gần nghĩa nhất); label → /[entity].
// DP-4.2: khi một app đang là lens VÀ entity thuộc nav của app, cấp gốc là TÊN APP → /home
// ("Quản lý Bán hàng / Khách hàng") — đúng ngữ cảnh người dùng đang đứng, thay vì "Dữ liệu" chung.
function entityCrumbs(entityName: string, label: string, rest: string[], appRoot?: Crumb): Crumb[] {
  const base: Crumb[] = [
    appRoot ?? { label: "Dữ liệu", href: "/entities" },
    { label, href: `/${entityName}` },
  ];
  if (rest.length === 0) return base;
  const [seg1, seg2] = rest;
  if (seg1 === "new") return [...base, { label: "Tạo mới" }];
  // Labels giữ nguyên chuỗi cũ ("Sửa" không chèn thêm cấp "Chi tiết") — nút back đã lo đường
  // lên /[entity]/[id].
  if (seg2 === "edit") return [...base, { label: "Sửa" }];
  if (ENTITY_SUBROUTE_LABELS[seg1]) return [...base, { label: ENTITY_SUBROUTE_LABELS[seg1] }];
  return [...base, { label: "Chi tiết" }]; // /[entity]/[id]
}

export function AppShell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const segments = pathname.split("/").filter(Boolean);
  const first = segments[0] ?? "";
  const isAdminArea = first === "admin";
  const isEntity = !isAdminArea && isEntitySegment(first);

  // Rules of hooks: always call. For static routes this looks up an entity named e.g. "home"
  // (never found) and harmlessly falls back to the segment itself — value is unused in that case.
  const entityLabel = useEntityLabel(first);

  // DP-4.2: entity crumbs root at the ACTIVE APP when the entity belongs to its nav —
  // "Quản lý Bán hàng / Khách hàng" instead of the generic "Dữ liệu / Khách hàng".
  const { activeMeta, activeNav } = useActiveApp();
  const appRoot: Crumb | undefined =
    activeMeta && (activeNav ?? []).some((i) => i.type === "entity" && i.href === `/${first}`)
      ? { label: activeMeta.label, href: "/home" }
      : undefined;

  const crumbs = isAdminArea
    ? adminCrumbs(segments)
    : isEntity
      ? entityCrumbs(first, entityLabel, segments.slice(1), appRoot)
      : staticCrumbs(segments);
  const searchPlaceholder = isAdminArea
    ? "Tìm người dùng, vai trò, nhóm…" // same string the old admin layout passed to Topbar
    : isEntity
      ? `Tìm trong ${entityLabel.toLowerCase()}…`
      : SEARCH_PLACEHOLDERS[first];

  // W3: nạp route vừa ghé vào "Gần đây" của ⌘K (per-user localStorage, cap 5). Bỏ /home
  // (đích mặc định, palette đã có entry tĩnh). Label = chuỗi breadcrumb cho dễ nhận;
  // deps kèm label để khi entity label load xong (async) thì entry tự cập nhật tên thật.
  const crumbLabel = crumbs.map((c) => c.label).join(" / ");
  useEffect(() => {
    if (pathname === "/home") return;
    recordRouteVisit({ href: pathname, label: crumbLabel });
  }, [pathname, crumbLabel]);

  const parent = parentPath(pathname);
  const atRoot = parent === pathname;

  return (
    // MOT-XUONG W3 #6 (in / xuất chứng từ): print:h-auto + print:overflow-visible on the root and
    // <main> — both are otherwise clipped to one viewport height (h-screen/overflow-hidden,
    // overflow-y-auto), which would cut off a printed document taller than one screen. The chrome
    // above <main> is print:hidden; a printed page is just <main>'s content, unclipped.
    <div className="flex h-screen flex-col overflow-hidden print:h-auto print:overflow-visible">
      {/* specs/064 S9-2: renders only while THIS tab previews a role; above the header so it is never missed. */}
      <PreviewBanner />
      {/* specs/068 lát B: appears when the server refuses a write because the workspace is write-frozen. */}
      <MaintenanceBanner />
      <ShellGlobalHeader searchPlaceholder={searchPlaceholder} className="shrink-0 print:hidden" />
      <ShellGlobalNavigation id="nx-global-nav" className="shrink-0 print:hidden" />
      <CrumbStrip crumbs={crumbs} pathname={pathname} parent={atRoot ? undefined : parent} />
      {/* flex+min-h-0+flex-col: bounds height for content that manages its own internal
          scroll region (LayoutList/Detail/Form, Approvals, Dashboards…); overflow-y-auto is
          the fallback scroll owner for simple content (HomeScreen) that doesn't. */}
      <main id="nx-main" className="flex min-h-0 flex-1 flex-col overflow-y-auto print:h-auto print:overflow-visible">
        {children}
      </main>
    </div>
  );
}

/**
 * Temporary strip under the nav: "lên cấp" (W0-QW7 — goes UP to the parent route, never
 * history.back()) + breadcrumbs. Moves into each screen's PageHeader as screens migrate (T017).
 * The current page is the last crumb; without its own href it links to itself.
 */
function CrumbStrip({ crumbs, pathname, parent }: { crumbs: Crumb[]; pathname: string; parent?: string }) {
  const router = useGuardedRouter();
  const up = t("slds.globalNav.up");
  return (
    <div className="slds-grid slds-grid_vertical-align-center slds-p-horizontal_medium slds-p-top_x-small shrink-0 print:hidden">
      <button
        type="button"
        className="slds-button slds-button_icon slds-button_icon-border-filled slds-button_icon-x-small slds-m-right_small"
        onClick={() => parent && router.push(parent)}
        disabled={!parent}
        title={parent ? `${up}: ${parent}` : up}
      >
        <ArrowLeft className="slds-button__icon nx-lucide" aria-hidden="true" />
        <span className="slds-assistive-text">{up}</span>
      </button>
      <Breadcrumbs items={crumbs.map((c) => ({ label: c.label, href: c.href ?? pathname }))} />
    </div>
  );
}
