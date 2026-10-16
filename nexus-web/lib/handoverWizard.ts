// lib/handoverWizard.ts — pure step-state logic for the "Bàn giao app này" wizard
// (EASE Nước 4, ../specs/027-handover-wizard/spec.md §4). Kept React-free so the gating
// rules, entity preselection and the graceful-degrade error classifier are unit-testable.
// The component layer (components/studio/HandoverWizard.tsx) stays a thin renderer.

import { ApiError } from "@/lib/api";
import { errorMessage } from "@/lib/errorMessages";
import type { I18nKey } from "@/lib/i18n/vi";

// ── Steps ────────────────────────────────────────────────────────────────────────

export type HandoverStepKey = "export" | "workspace" | "plan" | "apply";

/** Wizard steps in order — i18n keys feed the progress indicator (spec §4: Xuất → Workspace → Plan → Apply). */
export const HANDOVER_STEPS: ReadonlyArray<{ key: HandoverStepKey; label: I18nKey }> = [
  { key: "export", label: "page.handover.step.export" },
  { key: "workspace", label: "page.handover.step.workspace" },
  { key: "plan", label: "page.handover.step.plan" },
  { key: "apply", label: "page.handover.step.apply" },
];

/** The facts the gating logic needs — the component owns the actual useState. */
export interface HandoverGate {
  /** Step 1 produced a pack (kept in state — no forced file download). */
  hasPack: boolean;
  /** Step 2 picked or created a target workspace (slug). */
  targetSlug: string | null;
  /** Step 3's dry-run against the TARGET succeeded. */
  planReady: boolean;
}

/**
 * canAdvance — may the wizard move FORWARD from step `index`? Strictly cumulative: a later step
 * never runs without every earlier step's artifact (no pack → no target choice matters; no
 * target → no dry-run; no plan → no apply). Back-navigation is not gated here — the component
 * allows it freely before the real install.
 */
export function canAdvance(index: number, g: HandoverGate): boolean {
  switch (index) {
    case 0: return g.hasPack;
    case 1: return g.hasPack && !!g.targetSlug;
    case 2: return g.hasPack && !!g.targetSlug && g.planReady;
    default: return false; // apply is terminal — "Xong" closes, never "next"
  }
}

// ── Step 1 (Xuất) helpers ────────────────────────────────────────────────────────

/**
 * Apply a table row-selection map (entity name → checked) to the export selection: names that stay
 * checked keep their order, newly checked ones are appended (stable order across toggles and
 * select-all), unchecked ones drop out.
 */
export function applySelection(list: string[], selection: Record<string, boolean>): string[] {
  const kept = list.filter((n) => selection[n]);
  return [...kept, ...Object.keys(selection).filter((n) => selection[n] && !list.includes(n))];
}

/**
 * preselectEntities — the wizard opens with the app's nav entities selected (spec §3.3 scoped
 * export). Deduped and intersected with the tenant's live entities so an orphan nav ref can
 * never put a ghost name into the export call.
 */
export function preselectEntities(
  navEntityNames: string[], available: Array<{ name: string }>,
): string[] {
  const avail = new Set(available.map((e) => e.name));
  return [...new Set(navEntityNames)].filter((n) => avail.has(n));
}

/** Download filename for the "Tải bản sao" copy of the exported pack. */
export function packFileName(appSlug: string | null | undefined, includeData: boolean): string {
  const base = appSlug ? `pack-${appSlug}` : "vertical-pack";
  return `${base}${includeData ? "-with-data" : ""}.json`;
}

// ── Step 3/4 — target-error classification (graceful degrade, feature-detect) ───

export type TargetErrorKind = "unsupported" | "target-gone" | "forbidden" | "other";

export interface TargetErrorInfo {
  kind: TargetErrorKind;
  message: string;
}

/**
 * explainTargetError — classify an install/dry-run failure from a request that SENT
 * ?targetTenant=. Feature-detect, no version sniffing:
 *   • TENANT_NOT_FOUND → the BE UNDERSTOOD the param and rejected the slug (workspace deleted /
 *     deactivated between listing and install — anti-enumeration 404, spec §3.1). NOT the
 *     "unsupported" note: that would misreport a live MS-C backend.
 *   • FORBIDDEN → the operator gate said no (role changed after the probe). Honest, retryable.
 *   • any other 400/404 → an older backend that doesn't know ?targetTenant= yet — the honest
 *     "chưa hỗ trợ" note. Never silently fall back to a same-tenant install.
 *   • everything else (409/422/5xx/network) → normal error copy via errorMessage.
 */
export function explainTargetError(e: unknown): TargetErrorInfo {
  if (e instanceof ApiError) {
    if (e.code === "TENANT_NOT_FOUND") {
      return {
        kind: "target-gone",
        message: "Workspace đích không tồn tại hoặc đã ngừng hoạt động — quay lại bước Workspace chọn lại.",
      };
    }
    if (e.code === "FORBIDDEN") {
      return {
        kind: "forbidden",
        message: "Tài khoản này không còn quyền điều phối (operator) để cài vào workspace khác.",
      };
    }
    if (e.status === 400 || e.status === 404) {
      return {
        kind: "unsupported",
        message: "Backend chưa hỗ trợ cài chéo workspace (?targetTenant=) — cập nhật server rồi thử lại.",
      };
    }
  }
  return { kind: "other", message: errorMessage(e) };
}
