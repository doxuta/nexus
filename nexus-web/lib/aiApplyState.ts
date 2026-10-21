// Per-session memory of which assistant cards were already applied (specs/076 FR-004).
// A resumed session must not re-offer a live "Áp dụng" on an applied card — for a records draft
// that is a second insert. Stored per browser; only the final outcome is kept, never the text
// (table names, server messages) — F1 FR-025(d): nothing customer-specific in localStorage.
import type { I18nKey } from "@/lib/i18n/vi";

export type CardStatus = "idle" | "applying" | "done" | "warn" | "error" | "discarded" | "undone" | "sealed";

/** What survives a reload. */
export interface CardState {
  status: CardStatus;
  changeIds?: string[];
  headVersionNo?: number;
  updatedAt?: string;
}

/** In-memory card state: the persisted slice plus text that only lives for this tab. */
export interface ApplyState extends CardState {
  message?: string;
  entityName?: string;
  createdId?: string;
  /** i18n key of the where-did-it-go line, FROZEN at apply time (a later Publish must not rewrite history). */
  foot?: I18nKey | null;
  /** Per draft row at apply time (id → who/when), to tell a later edit from our own write. Not persisted. */
  stamps?: Record<string, { at: string; by: string }>;
  /** Why there is no Hoàn tác: the builder already had a draft of the same document, or the rows could not be matched. */
  noUndo?: "seen" | "nomatch";
  /** Set at render when a Publish went out after this card was applied (not persisted: derived from publish/state). */
  sealedVersion?: number;
}

const KEY = "nexus.ai-apply-state.v1";
const TERMINAL = new Set<CardStatus>(["done", "warn", "discarded", "undone", "sealed"]);

/** Applied, discarded or undone: the card is settled ("error" stays retryable, "applying" is in flight). */
export const isTerminal = (s: CardStatus): boolean => TERMINAL.has(s);

function pick(v: CardState): CardState {
  return {
    status: v.status,
    ...(Array.isArray(v.changeIds) ? { changeIds: v.changeIds } : {}),
    ...(typeof v.headVersionNo === "number" ? { headVersionNo: v.headVersionNo } : {}),
    ...(typeof v.updatedAt === "string" ? { updatedAt: v.updatedAt } : {}),
  };
}

function read(key: string): Record<string, CardState> | null {
  const raw = window.localStorage.getItem(key);
  const parsed = raw ? JSON.parse(raw) : null;
  if (!parsed || typeof parsed !== "object") return null;
  const out: Record<string, CardState> = {};
  for (const [k, v] of Object.entries(parsed as Record<string, CardState>)) {
    if (v && TERMINAL.has(v.status)) out[k] = pick(v);
  }
  return out;
}

export function loadApplyState(slug: string, sessionId: string): Record<string, ApplyState> {
  if (typeof window === "undefined" || !sessionId) return {};
  try {
    // Fall back to the pre-slug key: dropping it would re-offer Apply on cards applied before this release.
    return read(`${KEY}:${slug}:${sessionId}`) ?? read(`${KEY}:${sessionId}`) ?? {};
  } catch {
    return {};
  }
}

export function saveApplyState(slug: string, sessionId: string, map: Record<string, ApplyState>): void {
  if (typeof window === "undefined" || !sessionId) return;
  try {
    // "applying" is a state of this tab and "error" is retryable by design — neither is worth restoring.
    const kept = Object.fromEntries(
      Object.entries(map).filter(([, v]) => TERMINAL.has(v.status)).map(([k, v]) => [k, pick(v)]),
    );
    window.localStorage.setItem(`${KEY}:${slug}:${sessionId}`, JSON.stringify(kept));
  } catch {
    /* quota/private mode — convenience, never load-bearing */
  }
}
