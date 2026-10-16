// lib/assistant.ts — pure model + helpers for the ⌘K "Hỏi AI" ask-data panel. NO fetch here (that
// lives in lib/api.ts assistantAsk/…Session) — side-effect-free so it stays unit-testable, same
// split as lib/aiBuilder.
//
// assistantNotice maps a failed ask-turn into an HONEST notice, mirroring the assistant panel (M4) FE
// policy: 503 AI_DISABLED → composer disabled ("Trợ lý AI đang tắt"); 429 throttles reuse
// aiLimitMessage verbatim (static "~Ns" from Retry-After when readable — never a fake countdown);
// everything else falls back to the central errorMessage() map. Callers must NOT console.log the
// question/reply/err payloads — user data stays out of the console.
//
// ⌘K-can-act (EASE Nước 7): POST /assistant/ask now also returns drafts[] — HITL record proposals
// (kinds "record_create"/"record_update" ONLY, backend internal/ai/tools.go DraftItem). NOTHING
// was written server-side: the palette renders confirm cards and applies via the NORMAL
// createRecord/updateRecord under the user's own token.

import { ApiError } from "@/lib/api";
import { aiLimitMessage } from "@/lib/aiBuilder";
import { errorMessage } from "@/lib/errorMessages";

// kind steers rendering: "disabled" = feature off (disable the composer, wrench-honest),
// "limit" = transient 429 (warning tone, composer stays usable — resend is always safe),
// "error" = everything else (danger tone, composer stays usable).
export interface AssistantNotice {
  kind: "disabled" | "limit" | "error";
  text: string;
}

export function assistantNotice(err: unknown): AssistantNotice {
  if (err instanceof ApiError) {
    if (err.code === "AI_DISABLED") {
      return { kind: "disabled", text: "Trợ lý AI đang tắt trên máy chủ." };
    }
    if (err.status === 429) {
      // aiLimitMessage knows the three AI throttle codes; an unknown 429 falls back to the map.
      return { kind: "limit", text: aiLimitMessage(err.code, err.details.retryAfter) ?? errorMessage(err) };
    }
  }
  return { kind: "error", text: errorMessage(err) };
}

// ── Record drafts (⌘K-can-act) ───────────────────────────────────────────────────────────────────

// Payload of a proposed NEW record — applied via POST /:entity (createRecord).
export interface AssistantDraftRecordCreate {
  entity: string;
  data: Record<string, unknown>;
}

// Payload of a proposed PARTIAL update — applied via PUT /:entity/:id (updateRecord). `data` holds
// ONLY the fields to change; `before` echoes their CURRENT values (field-ACL already applied by the
// read engine) so the confirm card can render an honest old→new diff.
export interface AssistantDraftRecordUpdate {
  entity: string;
  recordId: string;
  data: Record<string, unknown>;
  before?: Record<string, unknown>;
}

// One draft item off the wire. kind stays open (like AiOutcome) so an unknown future kind degrades
// to the honest fallback card instead of type-erroring.
export interface AssistantDraft {
  kind: "record_create" | "record_update" | (string & {});
  recordCreate?: AssistantDraftRecordCreate | null;
  recordUpdate?: AssistantDraftRecordUpdate | null;
}

const isMap = (v: unknown): v is Record<string, unknown> =>
  v !== null && typeof v === "object" && !Array.isArray(v);

// Tolerant payload read (same policy as lib/aiBuilder draftPayload): accept the snake_case twin key
// too — drafts persist in draft_json, so a key-spelling drift must degrade to null, never crash.
function draftPayload(d: AssistantDraft, camelKey: string, snakeKey: string): Record<string, unknown> | null {
  const rec = d as unknown as Record<string, unknown>;
  const v = rec[camelKey] ?? rec[snakeKey];
  return isMap(v) ? v : null;
}

// null when the payload is absent/malformed (missing entity/recordId, non-object data) — the card
// then renders an honest "không đọc được" fallback with NO apply button.
export function getRecordCreateDraft(d: AssistantDraft): AssistantDraftRecordCreate | null {
  const p = draftPayload(d, "recordCreate", "record_create");
  if (!p || typeof p.entity !== "string" || !p.entity || !isMap(p.data)) return null;
  return { entity: p.entity, data: p.data };
}

export function getRecordUpdateDraft(d: AssistantDraft): AssistantDraftRecordUpdate | null {
  const p = draftPayload(d, "recordUpdate", "record_update");
  if (!p || typeof p.entity !== "string" || !p.entity) return null;
  if (typeof p.recordId !== "string" || !p.recordId || !isMap(p.data)) return null;
  return { entity: p.entity, recordId: p.recordId, data: p.data, before: isMap(p.before) ? p.before : undefined };
}

// Short display text for one draft value. Strings pass through verbatim (never JSON-quoted — it's
// the user's own text); only non-scalars fall back to JSON.
export function assistantDraftValueText(v: unknown): string {
  if (v === null || v === undefined || v === "") return "—";
  if (typeof v === "string") return v;
  if (typeof v === "boolean") return v ? "Có" : "Không";
  if (typeof v === "number") return String(v);
  try { return JSON.stringify(v); } catch { return String(v); }
}

// Compact one-line summary: "<entity> · N trường · k: v, k: v, k: v…". null for a malformed draft.
export function assistantDraftSummary(d: AssistantDraft, maxPairs = 3): string | null {
  const payload = getRecordCreateDraft(d) ?? getRecordUpdateDraft(d);
  if (!payload) return null;
  const entries = Object.entries(payload.data);
  const head = entries
    .slice(0, maxPairs)
    .map(([k, v]) => `${k}: ${assistantDraftValueText(v)}`)
    .join(", ");
  const base = `${payload.entity} · ${entries.length} trường`;
  return head ? `${base} · ${head}${entries.length > maxPairs ? "…" : ""}` : base;
}

// ── Sessions (rail feature='assistant' — user-scoped, NOT the builder's /admin rail) ─────────────

export interface AssistantSessionSummary {
  id: string;
  title: string;
  updatedAt: string; // ISO 8601
}

// One persisted message row (GET /assistant/sessions/:id → messages[]). drafts is already decoded
// server-side so a resumed thread re-renders its confirm cards.
export interface AssistantServerMessage {
  seq: number;
  role: string; // server-written "user" | "assistant"; coerced on the way in
  content: string;
  drafts: AssistantDraft[] | null;
  createdAt: string;
}

export interface AssistantSessionDetail {
  id: string;
  title: string;
  createdAt: string;
  updatedAt: string;
  messages: AssistantServerMessage[];
}

// A turn as held by the palette's ask panel. Only assistant turns carry drafts.
export interface AssistantTurn {
  id: string;
  role: "user" | "assistant";
  content: string;
  drafts?: AssistantDraft[] | null;
}

// Rebuild the ask-panel turns from a session's persisted rows (mirror of aiBuilder's
// serverMessagesToTurns). Keys derive from the monotonic seq → stable across resumes; drafts stay
// attached so a resumed confirm card is still actionable.
export function assistantMessagesToTurns(messages: AssistantServerMessage[]): AssistantTurn[] {
  return (messages ?? []).map((m) => {
    const role: AssistantTurn["role"] = m.role === "assistant" ? "assistant" : "user";
    const turn: AssistantTurn = { id: `m_${m.seq}`, role, content: m.content ?? "" };
    if (role === "assistant" && Array.isArray(m.drafts) && m.drafts.length > 0) turn.drafts = m.drafts;
    return turn;
  });
}
