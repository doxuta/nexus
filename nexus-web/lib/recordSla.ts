// recordSla — GET /:entity/:id/approvals?lane=independent (specs/043 T273/T282).
// Pure mapping + wording for the SLA card on the record page; no engine codes reach the screen.

export interface RecordSla {
  dueAt: string | null;
  basis?: string;
  paused: boolean;
  remainingSeconds?: number;
  met: boolean;
  breached: boolean;
}
export interface RecordSlaItem {
  id: string;
  stepName: string;
  status: string;
  sla: RecordSla;
}

export function fromSlaItem(raw: Record<string, unknown>): RecordSlaItem {
  const s = (raw.sla ?? {}) as Partial<RecordSla>;
  return {
    id: String(raw.id ?? ""),
    stepName: String(raw.stepName ?? ""),
    status: String(raw.status ?? ""),
    sla: {
      dueAt: s.dueAt ?? null,
      basis: s.basis,
      paused: !!s.paused,
      remainingSeconds: s.remainingSeconds,
      met: !!s.met,
      breached: !!s.breached,
    },
  };
}

export type SlaState = { kind: "met" | "overdue" | "paused" | "running"; theme: "success" | "warning" | "error" | undefined };

/** met wins (answered in time or late, the clock is closed); then breached; then paused; then past due. */
export function slaState(it: RecordSlaItem, now: number = Date.now()): SlaState {
  const { sla } = it;
  if (sla.met) return { kind: "met", theme: "success" };
  if (sla.breached) return { kind: "overdue", theme: "error" };
  if (sla.paused) return { kind: "paused", theme: "warning" };
  if (sla.dueAt && new Date(sla.dueAt).getTime() < now) return { kind: "overdue", theme: "error" };
  return { kind: "running", theme: "success" };
}

/** Basis codes (bpm.SLABasis) → customer words. */
export function slaBasisText(basis?: string): string {
  switch (basis) {
    case "business": return "giờ làm việc";
    case "wall": return "giờ đồng hồ";
    case "wall_no_calendar": return "giờ đồng hồ — chưa khai lịch làm việc";
    case "wall_calendar_error": return "giờ đồng hồ — lịch làm việc đang lỗi";
    default: return "";
  }
}
