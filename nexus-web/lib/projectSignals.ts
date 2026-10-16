// lib/projectSignals.ts — what the navy "Cần anh xem" band says, as a pure function of the project rows
// (specs/073 FR-091/FR-094, architecture 3.2). It reads only fields the row already carries: a field the server has not
// sent is a line that does not appear (never a guess, never a per-customer request). The Chạy thử switch is NOT here.
import type { ProjectRow } from "@/lib/api";
import { buildSteps } from "@/lib/buildSteps";
import { t, tf } from "@/lib/i18n";
import type { RouteId } from "@/lib/studioRoutes";

export type AttentionLevel = "error" | "warning" | "info";

export interface AttentionItem {
  level: AttentionLevel;
  projectSlug: string;
  projectName: string;
  text: string;
  /** Where the action opens inside the project (the caller builds the link / enters the project). */
  action: { label: string; route: RouteId };
}

const RANK: Record<AttentionLevel, number> = { error: 0, warning: 1, info: 2 };
const BACKUP_MAX_AGE_MS = 48 * 60 * 60 * 1000;

const isOperating = (r: ProjectRow) => r.stage === "live" || r.stage === "needs_attention";
const clock = (iso: string) => new Date(iso).toLocaleTimeString("vi-VN", { hour: "2-digit", minute: "2-digit", hour12: false });
const stamp = (iso: string) => new Date(iso).toLocaleString("vi-VN", { hour: "2-digit", minute: "2-digit", hour12: false, day: "2-digit", month: "2-digit" });

/** Every required step before "Chạy thử" is enough (optional steps and the trial / deliver marks do not count). */
function builtForTrial(r: ProjectRow): boolean {
  return buildSteps(r, { home: false })
    .filter((s) => !s.optional && s.key !== "trial" && s.key !== "deliver")
    .every((s) => s.complete === true);
}

function itemsOf(r: ProjectRow, now: Date): AttentionItem[] {
  const out: AttentionItem[] = [];
  const add = (level: AttentionLevel, text: string, labelKey: Parameters<typeof t>[0], route: RouteId) =>
    out.push({ level, projectSlug: r.slug, projectName: r.name, text, action: { label: t(labelKey), route } });

  if (r.health.status === "critical") {
    const when = r.health.checkedAt ? stamp(r.health.checkedAt) : "";
    add("error", tf("studio.attention.critical", { n: r.health.critical, when }), "studio.attention.act.openCheck", "bao-tri/kham");
  }
  if (r.health.ops?.status === "critical") add("error", t("studio.attention.ops"), "studio.attention.act.openJobs", "van-hanh/tu-dong");
  if (r.backup?.lastFailed) add("error", t("studio.attention.backupFailed"), "studio.attention.act.openBackups", "bao-tri/sao-luu");

  if (r.maintenanceUntil && new Date(r.maintenanceUntil).getTime() > now.getTime()) {
    add("warning", tf("studio.attention.locked", { time: clock(r.maintenanceUntil) }), "studio.attention.act.openBackups", "bao-tri/sao-luu");
  }
  if (isOperating(r) && r.backup) {
    const last = r.backup.lastReadyAt ? new Date(r.backup.lastReadyAt).getTime() : null;
    if (last === null || now.getTime() - last > BACKUP_MAX_AGE_MS) add("warning", t("studio.attention.noBackup"), "studio.attention.act.backup", "bao-tri/sao-luu");
  }
  if (isOperating(r) && r.usableOwnerCount === 0) add("warning", t("studio.attention.noOwner"), "studio.attention.act.openUsers", "van-hanh/nguoi-dung");
  for (const app of r.health.appsNotReady) {
    add("warning", tf("studio.attention.appNotReady", { name: app.name }), "studio.attention.act.openCheck", "bao-tri/kham");
  }

  if (r.stage === "building" && builtForTrial(r)) add("info", t("studio.attention.builtNotTrial"), "studio.attention.act.openTrial", "dung/chay-thu");
  if (r.stage === "trial" && builtForTrial(r)) add("info", t("studio.attention.readyToDeliver"), "studio.attention.act.deliver", "dung/giao-cho-khach");
  return out;
}

/**
 * The lines of the band, most severe first; within a level the order of `rows` is kept. `now` is a parameter so the
 * backup / write-lock windows are testable.
 */
export function attentionItems(rows: readonly ProjectRow[], now: Date = new Date()): AttentionItem[] {
  return rows
    .flatMap((r) => itemsOf(r, now))
    .sort((a, b) => RANK[a.level] - RANK[b.level]); // Array.sort is stable: rows keep their order within a level
}
