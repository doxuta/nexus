// Sample project-ledger rows for /dev/kit and for the tests of the ledger components (specs/073 so1a: sample data lives
// ONLY here, never in a product page). Same shape as GET /admin/projects.
import type { ProjectRow } from "@/lib/api";

export function projectRow(over: Partial<ProjectRow> = {}): ProjectRow {
  return {
    tenantId: "t-1",
    slug: "pk-ban-nho",
    name: "Phòng khám Bạn Nhỏ",
    tenantStatus: "active",
    industry: "Thú y",
    origin: "pack",
    originPack: "vet",
    stage: "building",
    createdAt: "2026-09-01T00:00:00Z",
    trialSince: null,
    deliveredAt: null,
    branded: false,
    health: { status: "unknown", critical: 0, warn: 0, checks: [], appsNotReady: [], checkedAt: null },
    counts: { apps: 1, entities: 0, customLayouts: 0, workflows: 0, approvals: 0, roles: 0, users: 1, staffUsers: 0, reports: 0, dashboards: 0 },
    lastActiveAt: null,
    ...over,
  };
}

const BUILT = { apps: 1, entities: 4, customLayouts: 1, workflows: 2, approvals: 0, roles: 2, users: 5, staffUsers: 4, reports: 2, dashboards: 1 };

/** One project per stage, plus problems for the navy band. */
export const SAMPLE_ROWS: ProjectRow[] = [
  projectRow({ tenantId: "t-1", slug: "pk-ban-nho", name: "Phòng khám Bạn Nhỏ", industry: "Thú y", counts: { ...BUILT, entities: 3, roles: 0, staffUsers: 0, reports: 0, dashboards: 0, workflows: 0, customLayouts: 0 } }),
  projectRow({ tenantId: "t-2", slug: "gach-viet", name: "Gạch Việt", industry: "Vật liệu xây dựng", stage: "building", branded: true, counts: BUILT }),
  projectRow({ tenantId: "t-3", slug: "nha-khoa-tam-duc", name: "Nha khoa Tâm Đức", industry: "Nha khoa", stage: "trial", branded: true, trialSince: "2026-10-01T00:00:00Z", counts: BUILT }),
  projectRow({
    tenantId: "t-4",
    slug: "minh-anh",
    name: "Phòng khám Minh Anh",
    industry: "Phòng khám",
    stage: "live",
    branded: true,
    trialSince: "2026-09-10T00:00:00Z",
    deliveredAt: "2026-09-20T00:00:00Z",
    counts: BUILT,
    lastActiveAt: "2026-10-08T07:30:00Z",
    usableOwnerCount: 0,
  }),
  projectRow({
    tenantId: "t-5",
    slug: "an-binh",
    name: "Công ty An Bình",
    industry: null,
    stage: "needs_attention",
    branded: true,
    trialSince: "2026-08-10T00:00:00Z",
    deliveredAt: "2026-08-20T00:00:00Z",
    counts: BUILT,
    lastActiveAt: "2026-10-09T01:00:00Z",
    health: { status: "critical", critical: 2, warn: 1, checks: [], appsNotReady: [{ appId: "a1", name: "Bán hàng", reasons: [] }], checkedAt: "2026-10-09T02:15:00Z" },
    backup: { lastReadyAt: "2026-10-01T00:00:00Z", lastFailed: true },
  }),
];
