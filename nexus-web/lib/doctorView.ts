// Pure view logic of the health-check checklist (specs/079 f7-kham-ve-lai): what the lede says, which rows show in
// which order, which checks the "fix all (safe)" batch may carry, and when to warn that a fix reaches real users at once.
import type { DoctorFinding, DoctorScanData, DoctorSeverity } from "@/lib/api";
import { t, tf } from "@/lib/i18n";

/** Whether the project's changes reach real users at once. `unknown` (still loading, failed) is treated as delivered. */
export type DeliveredState = "delivered" | "not-delivered" | "no-concept" | "unknown";

const RANK: Record<DoctorSeverity, number> = { critical: 0, warn: 1, info: 2 };

/** HH:MM in the viewer's zone; the raw text when the server sent something unparsable. */
export function clockTime(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleTimeString("vi-VN", { hour: "2-digit", minute: "2-digit", hour12: false });
}

/** count 0 (or absent) is clean by contract: only real drift is a row. */
export function findingRows(findings: DoctorFinding[]): DoctorFinding[] {
  return findings
    .filter((f) => f.count > 0)
    .sort((a, b) => RANK[a.severity] - RANK[b.severity] || b.count - a.count);
}

export function fixableIds(findings: DoctorFinding[]): string[] {
  return findings.filter((f) => f.autoFixSafe && !f.reportOnly && f.count > 0).map((f) => f.checkId);
}

/** The lede sentence; `**…**` marks the bold numbers (the component renders them). */
export function doctorLede(scan: DoctorScanData | null): string {
  if (!scan) return t("studio.maintain.kham.ledeNone");
  const time = clockTime(scan.scannedAt);
  const rows = findingRows(scan.findings);
  if (rows.length === 0) return tf("studio.maintain.kham.ledeClean", { time });
  const n = (sev: DoctorSeverity) => rows.filter((r) => r.severity === sev).length;
  const parts = (["critical", "warn", "info"] as const)
    .filter((s) => n(s) > 0)
    .map((s) => `**${tf(`studio.maintain.kham.count.${s}` as const, { n: n(s) })}**`);
  return tf("studio.maintain.kham.ledeDrift", { time, parts: parts.join(", ") });
}

/** One sentence when nothing drifted (instead of one line per clean check); null when there is drift. */
export function cleanRow(scan: DoctorScanData, total: number): string | null {
  return findingRows(scan.findings).length === 0 ? tf("studio.maintain.kham.clean", { n: total }) : null;
}

/** Fails closed: only a project known not to be delivered (or one without the concept) goes without the warning. */
export function deliveredAdvice(state: DeliveredState): string {
  return state === "not-delivered" || state === "no-concept" ? "" : t("studio.maintain.kham.deliveredAdvice");
}
