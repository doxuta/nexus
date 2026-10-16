// lib/buildSteps.ts — the eight build steps of a project, derived (never ticked by hand) from ONE project row
// (specs/073 FR-040, architecture 6.3). Pure. The stage is `row.stage` from the server; this file never computes it.
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import type { RouteId, StudioRoute } from "@/lib/studioRoutes";

export type BuildStepKey = "data" | "screens" | "automation" | "access" | "reports" | "branding" | "trial" | "deliver";

export interface BuildStepState {
  key: BuildStepKey;
  /** true = enough, false = not yet, null = not known (no row): no tick, no "Đã đủ / Chưa đủ". */
  complete: boolean | null;
  optional: boolean;
  sublabel: string;
}

/**
 * The part of GET /admin/projects (ProjectRow, 062 L5) the steps read. Structural on purpose: the full row type
 * arrives with the project registry slice (so1a) and satisfies this one.
 */
export interface BuildStepsRow {
  branded: boolean;
  trialSince: string | null;
  deliveredAt: string | null;
  counts: {
    entities: number;
    customLayouts: number;
    workflows: number;
    approvals: number;
    roles: number;
    staffUsers: number;
    reports: number;
    dashboards: number;
  };
}

const ORDER: BuildStepKey[] = ["data", "screens", "automation", "access", "reports", "branding", "trial", "deliver"];
const OPTIONAL = new Set<BuildStepKey>(["screens", "automation"]);

const day = (iso: string | null) => (iso ? new Date(iso).toLocaleDateString("vi-VN") : "");
const key = (step: BuildStepKey, tail: string) => `studio.step.${step}.${tail}` as I18nKey;

function evaluate(row: BuildStepsRow): Record<BuildStepKey, { complete: boolean; sublabel: string }> {
  const c = row.counts;
  return {
    data: { complete: c.entities >= 1, sublabel: c.entities >= 1 ? tf(key("data", "sub.some"), { n: c.entities }) : t(key("data", "sub.none")) },
    screens: {
      complete: c.customLayouts >= 1,
      sublabel: c.customLayouts >= 1 ? tf(key("screens", "sub.some"), { n: c.customLayouts }) : t(key("screens", "sub.none")),
    },
    automation: {
      complete: c.workflows + c.approvals >= 1,
      sublabel: c.workflows + c.approvals >= 1 ? tf(key("automation", "sub.some"), { n: c.workflows, m: c.approvals }) : t(key("automation", "sub.none")),
    },
    // Roles designed by the builder AND at least one active non-admin staff member (062 FR-027a).
    access: { complete: c.roles >= 1 && c.staffUsers >= 1, sublabel: tf(key("access", "sub.some"), { n: c.roles, m: c.staffUsers }) },
    reports: { complete: c.reports + c.dashboards >= 1, sublabel: tf(key("reports", "sub.some"), { n: c.reports, m: c.dashboards }) },
    branding: { complete: row.branded, sublabel: t(key("branding", row.branded ? "sub.some" : "sub.none")) },
    // Delivered counts too: a project delivered straight away has trialSince = null and would otherwise stay "not yet" forever.
    trial: {
      complete: row.trialSince !== null || row.deliveredAt !== null,
      sublabel: row.trialSince !== null ? tf(key("trial", "sub.some"), { date: day(row.trialSince) }) : t(key("trial", "sub.none")),
    },
    deliver: {
      complete: row.deliveredAt !== null,
      sublabel: row.deliveredAt !== null ? tf(key("deliver", "sub.some"), { date: day(row.deliveredAt) }) : t(key("deliver", "sub.none")),
    },
  };
}

/** `home` (Nexus workshop) drops the "deliver" step regardless of `row`; `row = null` means nothing is known. */
export function buildSteps(row: BuildStepsRow | null, opts: { home: boolean }): BuildStepState[] {
  const keys = opts.home ? ORDER.filter((k) => k !== "deliver") : ORDER;
  const ev = row ? evaluate(row) : null;
  return keys.map((k) => ({
    key: k,
    complete: ev ? ev[k].complete : null,
    optional: OPTIONAL.has(k),
    sublabel: ev ? ev[k].sublabel : "",
  }));
}

/** Where each step opens (the step bar's links). */
export const STEP_ROUTE: Record<BuildStepKey, RouteId> = {
  data: "dung/du-lieu",
  screens: "dung/man-hinh",
  automation: "dung/tu-dong",
  access: "dung/nguoi-dung-quyen",
  reports: "dung/bao-cao",
  branding: "dung/thuong-hieu",
  trial: "dung/chay-thu",
  deliver: "dung/giao-cho-khach",
};

/** The step a route belongs to (every `dung/*` page sits under one step); null for Vận hành / Bảo trì / Cài đặt. */
export function stepOfRoute(route: StudioRoute | null): BuildStepKey | null {
  if (!route || route.segments[0] !== "dung") return null;
  return (Object.keys(STEP_ROUTE) as BuildStepKey[]).find((k) => STEP_ROUTE[k].split("/")[1] === route.segments[1]) ?? null;
}

/** First required step that is known to be not enough yet. */
export function nextStep(steps: BuildStepState[]): BuildStepKey | null {
  return steps.find((s) => !s.optional && s.complete === false)?.key ?? null;
}
