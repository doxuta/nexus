import type { QueryClient } from "@tanstack/react-query";

// Keys an applied proposal can never change; everything else (metadata, records, apps, reports,
// dashboards, studio counts, publish state, current project…) is refreshed (specs/076 FR-005, D-AI-14).
// Refreshing all-but-a-few means a kind added later cannot silently fall off a hand-kept list.
const UNRELATED = new Set(["me", "permissions", "notifications"]);

export function shouldRefreshAfterApply(key: readonly unknown[]): boolean {
  if (typeof key[0] === "string" && UNRELATED.has(key[0])) return false;
  return !(key[0] === "ai" && key[1] === "sessions");
}

export function invalidateAfterApply(qc: QueryClient): void {
  void qc.invalidateQueries({ predicate: (q) => shouldRefreshAfterApply(q.queryKey) });
}
