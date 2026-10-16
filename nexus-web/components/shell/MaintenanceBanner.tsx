"use client";

// MaintenanceBanner — the strip above the customer app while the workspace is write-frozen
// (specs/068-tenant-backup-restore lát B). It has no polling of its own: lib/api.ts raises
// `nexus:tenant-maintenance` the moment the server refuses a write with 503 TENANT_MAINTENANCE, and
// this strip stays until the deadline the server named (details.until) passes, then hides itself.
// SLDS Alert (warning); the refused Save itself already shows the server's own sentence as a toast.
import { useEffect, useState } from "react";
import { Alert } from "@/components/slds/Alert";
import { TENANT_MAINTENANCE_EVENT } from "@/lib/api";
import { formatTime } from "@/lib/format";
import { t, tf } from "@/lib/i18n";

/** Fallback when the server gave no deadline: keep the strip a few minutes, then let a new refusal re-raise it. */
const NO_DEADLINE_MS = 5 * 60_000;

export function MaintenanceBanner() {
  // null = hidden; otherwise the deadline (ms since epoch) and whether the server really named it.
  const [state, setState] = useState<{ until: number; named: boolean } | null>(null);

  useEffect(() => {
    const onRefused = (e: Event) => {
      const raw = (e as CustomEvent<{ until: string | null }>).detail?.until;
      const parsed = raw ? new Date(raw).getTime() : NaN;
      setState(Number.isNaN(parsed) ? { until: Date.now() + NO_DEADLINE_MS, named: false } : { until: parsed, named: true });
    };
    window.addEventListener(TENANT_MAINTENANCE_EVENT, onRefused);
    return () => window.removeEventListener(TENANT_MAINTENANCE_EVENT, onRefused);
  }, []);

  // Hide by itself when the freeze lapses.
  useEffect(() => {
    if (!state) return;
    const id = window.setTimeout(() => setState(null), Math.max(0, state.until - Date.now()));
    return () => window.clearTimeout(id);
  }, [state]);

  if (!state) return null;
  return (
    <div className="shrink-0 print:hidden">
      <Alert variant="warning">
        {state.named ? tf("maintenance.banner", { time: formatTime(new Date(state.until)) }) : t("maintenance.bannerNoTime")}
      </Alert>
    </div>
  );
}
