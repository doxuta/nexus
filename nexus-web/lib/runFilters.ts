// lib/runFilters.ts — W4 run-overlay filter model (dashboard run · report run · report export).
// Backend contract (api-conventions §4.4): ?dateFrom=YYYY-MM-DD & ?dateTo=YYYY-MM-DD & ?ownerId=<userId>
// — ANDed onto every widget/report's saved filters at request time, nothing persisted.
// This module is PURE (no React, no fetch): preset ranges, URL (de)serialisation, chip labels.

export interface RunFilters {
  /** created_at >= dateFrom 00:00:00 (inclusive) — YYYY-MM-DD */
  dateFrom?: string;
  /** created_at < dateTo+1d (whole day inclusive) — YYYY-MM-DD */
  dateTo?: string;
  /** created_by = ownerId (exact user id) */
  ownerId?: string;
}

// ── Date helpers (local time — the user picks calendar days in their own zone) ──

/** Format a Date as local YYYY-MM-DD (NOT toISOString — that would shift across UTC midnight). */
export function toISODate(d: Date): string {
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${y}-${m}-${day}`;
}

const ISO_DATE_RE = /^\d{4}-\d{2}-\d{2}$/;

/** True when s is a plausible YYYY-MM-DD calendar date (backend 422s impossible ones). */
export function isISODate(s: string): boolean {
  if (!ISO_DATE_RE.test(s)) return false;
  const [y, m, d] = s.split("-").map(Number);
  if (m < 1 || m > 12 || d < 1 || d > 31) return false;
  const dt = new Date(y, m - 1, d);
  return dt.getFullYear() === y && dt.getMonth() === m - 1 && dt.getDate() === d;
}

/** Parse YYYY-MM-DD into a local-midnight Date, or null when invalid. */
export function parseISODate(s: string): Date | null {
  if (!isISODate(s)) return null;
  const [y, m, d] = s.split("-").map(Number);
  return new Date(y, m - 1, d);
}

// ── Presets — Hôm nay / 7 ngày / 30 ngày / Quý này / Năm nay ────────────────

export type DatePresetId = "today" | "7d" | "30d" | "quarter" | "year";

export const DATE_PRESETS: { id: DatePresetId; label: string }[] = [
  { id: "today", label: "Hôm nay" },
  { id: "7d", label: "7 ngày qua" },
  { id: "30d", label: "30 ngày qua" },
  { id: "quarter", label: "Quý này" },
  { id: "year", label: "Năm nay" },
];

/** Resolve a preset to a concrete {dateFrom, dateTo} pair, relative to `today`. */
export function presetRange(preset: DatePresetId, today: Date): { dateFrom: string; dateTo: string } {
  const to = toISODate(today);
  switch (preset) {
    case "today":
      return { dateFrom: to, dateTo: to };
    case "7d": {
      const d = new Date(today);
      d.setDate(d.getDate() - 6); // 7 calendar days inclusive of today
      return { dateFrom: toISODate(d), dateTo: to };
    }
    case "30d": {
      const d = new Date(today);
      d.setDate(d.getDate() - 29);
      return { dateFrom: toISODate(d), dateTo: to };
    }
    case "quarter": {
      const qStartMonth = Math.floor(today.getMonth() / 3) * 3;
      return { dateFrom: toISODate(new Date(today.getFullYear(), qStartMonth, 1)), dateTo: to };
    }
    case "year":
      return { dateFrom: toISODate(new Date(today.getFullYear(), 0, 1)), dateTo: to };
  }
}

/** Which preset (if any) the current filters exactly match — highlights the active preset button. */
export function matchPreset(f: RunFilters, today: Date): DatePresetId | null {
  if (!f.dateFrom || !f.dateTo) return null;
  for (const p of DATE_PRESETS) {
    const r = presetRange(p.id, today);
    if (r.dateFrom === f.dateFrom && r.dateTo === f.dateTo) return p.id;
  }
  return null;
}

// ── URL sync — ?dateFrom=…&dateTo=…&ownerId=… (same names as the API params) ──

/** Serialise filters into query-param entries (only the active ones — clean URLs). */
export function filtersToParams(f: RunFilters): Record<string, string> {
  const r: Record<string, string> = {};
  if (f.dateFrom) r.dateFrom = f.dateFrom;
  if (f.dateTo) r.dateTo = f.dateTo;
  if (f.ownerId) r.ownerId = f.ownerId;
  return r;
}

/** Read filters back from a URLSearchParams-like source; malformed dates are DROPPED
    (never forward garbage to the API — it would 422 the whole run). */
export function parseFilters(sp: { get(name: string): string | null }): RunFilters {
  const f: RunFilters = {};
  const from = sp.get("dateFrom");
  const to = sp.get("dateTo");
  const owner = sp.get("ownerId");
  if (from && isISODate(from)) f.dateFrom = from;
  if (to && isISODate(to)) f.dateTo = to;
  // dateFrom > dateTo would 422 RUN_DATE_RANGE_INVALID — drop the pair rather than half-apply.
  if (f.dateFrom && f.dateTo && f.dateFrom > f.dateTo) {
    delete f.dateFrom;
    delete f.dateTo;
  }
  if (owner && owner.trim()) f.ownerId = owner.trim();
  return f;
}

/** Stable key-shape for TanStack Query cache keys. */
export function filtersToQueryKey(f: RunFilters): Record<string, string> {
  return filtersToParams(f);
}

export function countActiveFilters(f: RunFilters): number {
  return (f.dateFrom || f.dateTo ? 1 : 0) + (f.ownerId ? 1 : 0);
}

// ── Chips — removable summary of active filters ─────────────────────────────

export interface FilterChip {
  /** Which filter this chip removes: the date pair or the owner. */
  kind: "date" | "owner";
  label: string;
}

/** Render "03/07/2026" from an ISO date (vi-VN reading order). */
export function formatDateVN(iso: string): string {
  const d = parseISODate(iso);
  if (!d) return iso;
  return `${String(d.getDate()).padStart(2, "0")}/${String(d.getMonth() + 1).padStart(2, "0")}/${d.getFullYear()}`;
}

/**
 * Build removable chips from active filters. Date range collapses into ONE chip
 * (removing it clears both bounds); the owner chip shows the resolved display name
 * that the caller passes in (fullName for admins, "Chỉ của tôi" for self-scope).
 */
export function filterChips(f: RunFilters, ownerLabel?: string): FilterChip[] {
  const chips: FilterChip[] = [];
  if (f.dateFrom && f.dateTo) {
    chips.push({
      kind: "date",
      label: f.dateFrom === f.dateTo
        ? formatDateVN(f.dateFrom)
        : `${formatDateVN(f.dateFrom)} – ${formatDateVN(f.dateTo)}`,
    });
  } else if (f.dateFrom) {
    chips.push({ kind: "date", label: `Từ ${formatDateVN(f.dateFrom)}` });
  } else if (f.dateTo) {
    chips.push({ kind: "date", label: `Đến ${formatDateVN(f.dateTo)}` });
  }
  if (f.ownerId) {
    chips.push({ kind: "owner", label: ownerLabel ?? `Người tạo: ${f.ownerId.slice(0, 8)}…` });
  }
  return chips;
}

/** Remove one chip's worth of filters (immutably). */
export function removeChip(f: RunFilters, kind: FilterChip["kind"]): RunFilters {
  const next = { ...f };
  if (kind === "date") {
    delete next.dateFrom;
    delete next.dateTo;
  } else {
    delete next.ownerId;
  }
  return next;
}
