// Per-row result of "Khám lại" on the Dự án khách board (specs/073 so1b T453). Kept in page state, never cached.
import type { ProjectHealthStatus } from "@/lib/api";

export type CheckState =
  | { kind: "running" }
  | { kind: "done"; status: ProjectHealthStatus; at: string }
  | { kind: "failed"; why: string };

const DAY_MS = 24 * 60 * 60 * 1000;

/** A stored snapshot older than 24h (or never taken) is re-run when the page opens. */
export const isStaleCheck = (checkedAt: string | null, now = Date.now()): boolean =>
  checkedAt === null || now - new Date(checkedAt).getTime() > DAY_MS;
