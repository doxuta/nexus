// lib/aiBuilderStream.ts — SSE client for POST /admin/ai/builder/chat/stream (Phase B AI-MODULES).
// The stream endpoint is the /chat twin with live progress: pre-admission refusals (429 guards,
// 400/404) are PLAIN JSON exactly like /chat; an admitted turn is text/event-stream emitting
// `step`×N → ONE `result` (data = the exact /chat 200 body) → `done`, or ONE `err` mid-stream.
// This module owns fetch + parse only; it deliberately reuses lib/api's ApiError (thrown with the
// same status/code/details shape req() produces, so the assistant's error branches keep working
// unchanged) and falls back to the req()-backed chatWithAiBuilder for old backends / expired
// tokens — it NEVER duplicates the auth-refresh machinery.

import { getToken, ApiError, chatWithAiBuilder } from "@/lib/api";
import { transitionPhase } from "@/lib/studioContext";
import { buildAiChatBody, type AiChatResult, type AiTurnContextInput } from "@/lib/aiBuilder";

// BASE is not exported by lib/api (private const) — mirror the one-liner instead of touching it.
const BASE = process.env.NEXT_PUBLIC_API_BASE || "http://localhost:8080/api/v1";

// One `step` progress frame. phase "calling_tool"/"tool_done" also carry `tool` (registry name).
export interface AiStepEvent {
  step: number;
  maxSteps: number;
  phase: "thinking" | "calling_tool" | "tool_done" | "final";
  tool?: string;
}

export interface AiStreamOpts {
  onStep: (e: AiStepEvent) => void;
  signal: AbortSignal;
  /** specs/067: the screen the maker is in; sent on the stream AND on all three JSON fallbacks. */
  context?: AiTurnContextInput;
}

// Thrown when the caller aborted the turn (Huỷ button) — distinct from a real failure so the UI
// can render a neutral "Đã huỷ." instead of a red error notice.
export class AiStreamCancelledError extends Error {
  constructor() {
    super("cancelled");
    this.name = "AiStreamCancelledError";
  }
}

// Old-backend memo: after ONE bare 404/405 at the stream endpoint (no JSON error envelope — a
// route-level miss, NOT a 404 SESSION_NOT_FOUND refusal), every later turn goes straight to the
// non-streaming /chat client instead of re-probing per turn.
let streamUnsupported = false;

// Test-only: reset the module memo between tests (module instances persist within a test file).
export function resetStreamFallbackForTests(): void {
  streamUnsupported = false;
}

function isAbort(err: unknown, signal: AbortSignal): boolean {
  if (signal.aborted) return true;
  return typeof err === "object" && err !== null && (err as { name?: unknown }).name === "AbortError";
}

// Same 3-liner as lib/api's private retryAfterFromHeader (not exported — mirrored, not moved).
function retryAfterFrom(res: Response): number | undefined {
  const raw = res.headers.get("Retry-After");
  const n = raw ? Number(raw) : NaN;
  return Number.isFinite(n) && n > 0 ? n : undefined;
}

interface ErrPayload {
  status?: unknown;
  error?: unknown;
  code?: unknown;
  details?: unknown;
}

// An `err` event / refusal payload → the same ApiError shape req() throws, so errorMessage() and
// the code-keyed branches in the assistant keep working. Missing fields degrade to gateway-generic.
function toApiError(status: number, p: ErrPayload, retryAfter?: number): ApiError {
  const details = p.details !== null && typeof p.details === "object" ? { ...(p.details as Record<string, unknown>) } : {};
  if (retryAfter !== undefined) details.retryAfter = retryAfter;
  return new ApiError(
    status,
    typeof p.code === "string" ? p.code : "AI_GATEWAY_ERROR",
    typeof p.error === "string" ? p.error : "Lỗi khi stream phản hồi AI",
    details,
  );
}

// aiBuilderChatStream — one agentic turn over SSE. onStep fires per progress frame; resolves with
// the exact AiChatResult /chat returns; throws ApiError on refusal/err (same shapes as /chat) and
// AiStreamCancelledError when opts.signal aborts. Falls back to chatWithAiBuilder on old backends
// (memoized) and on a 401 (the raw fetch has no silent-refresh — delegate THIS turn to the
// req()-backed client, which refreshes + retries; the next turn streams again with a fresh token).
export async function aiBuilderChatStream(
  message: string,
  sessionId: string | undefined,
  opts: AiStreamOpts,
): Promise<AiChatResult> {
  if (transitionPhase() !== "idle") throw new AiStreamCancelledError(); // project switch in progress: no token is valid, send nothing (FR-128)
  if (streamUnsupported) return chatWithAiBuilder(message, sessionId, opts.context);

  const body = buildAiChatBody(message, sessionId, opts.context);
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  const t = getToken();
  if (t) headers["Authorization"] = `Bearer ${t}`;

  let res: Response;
  try {
    res = await fetch(`${BASE}/admin/ai/builder/chat/stream`, {
      method: "POST",
      headers,
      body: JSON.stringify(body),
      signal: opts.signal,
    });
  } catch (err) {
    if (isAbort(err, opts.signal)) throw new AiStreamCancelledError();
    throw err;
  }

  const ct = res.headers.get("Content-Type") ?? "";
  if (!ct.includes("text/event-stream")) {
    // Pre-admission refusal — plain JSON exactly like /chat (429 keeps Retry-After).
    const json = (await res.json().catch(() => ({}))) as ErrPayload;
    if ((res.status === 404 || res.status === 405) && typeof json.code !== "string") {
      // Bare route miss (no envelope) = old backend without the endpoint. A 404 WITH a code
      // (SESSION_NOT_FOUND) is a real refusal and must NOT flip the memo.
      streamUnsupported = true;
      return chatWithAiBuilder(message, sessionId, opts.context);
    }
    if (res.status === 401) return chatWithAiBuilder(message, sessionId, opts.context); // token expired → req() refresh path
    if (!res.ok) throw toApiError(res.status, json, retryAfterFrom(res));
    throw new ApiError(502, "AI_GATEWAY_ERROR", "Phản hồi stream không hợp lệ");
  }

  if (!res.body) throw new ApiError(502, "AI_GATEWAY_ERROR", "Stream không có nội dung");

  const reader = res.body.getReader();
  // stream:true keeps a multibyte UTF-8 sequence split across chunks buffered inside the decoder.
  const decoder = new TextDecoder("utf-8");
  let buf = "";
  let result: AiChatResult | null = null;
  let sawDone = false;

  // One "event:…\ndata:{…}" block. Returns "done" on the terminal event. Throws on event:err.
  const handleBlock = (block: string): "done" | undefined => {
    let event = "";
    const dataLines: string[] = [];
    for (const rawLine of block.split("\n")) {
      const line = rawLine.endsWith("\r") ? rawLine.slice(0, -1) : rawLine;
      if (line.startsWith("event:")) event = line.slice(6).trim();
      else if (line.startsWith("data:")) dataLines.push(line.slice(5).replace(/^ /, ""));
    }
    const data = dataLines.join("\n");
    if (event === "step") {
      try {
        opts.onStep(JSON.parse(data) as AiStepEvent);
      } catch {
        /* a malformed progress frame must not kill the turn */
      }
    } else if (event === "result") {
      try {
        // data = the exact /chat 200 body: {"data":{sessionId, reply, draft, runId, usage, outcome}}
        result = (JSON.parse(data) as { data: AiChatResult }).data ?? null;
      } catch {
        /* falls through to the no-result guard below */
      }
    } else if (event === "err") {
      let p: ErrPayload = {};
      try {
        p = JSON.parse(data) as ErrPayload;
      } catch {
        /* gateway-generic below */
      }
      throw toApiError(typeof p.status === "number" ? p.status : 500, p);
    } else if (event === "done") {
      return "done";
    }
    return undefined;
  };

  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      // The parser splits on the contract's \n\n frame boundary and tolerates a chunk cut
      // MID-line / MID-character: the tail stays in buf / the decoder until the next chunk.
      buf += decoder.decode(value, { stream: true });
      let idx: number;
      while ((idx = buf.indexOf("\n\n")) !== -1) {
        const block = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        if (handleBlock(block) === "done") {
          sawDone = true;
          break;
        }
      }
      if (sawDone) break;
    }
  } catch (err) {
    if (isAbort(err, opts.signal)) throw new AiStreamCancelledError();
    throw err;
  } finally {
    // Release the connection on every exit (done before EOF, err mid-stream, abort).
    void reader.cancel().catch(() => {});
  }

  if (!result) {
    // Stream closed early with no result (proxy cut, backend died mid-turn) — honest generic.
    throw new ApiError(502, "AI_GATEWAY_ERROR", "Stream kết thúc sớm — thử lại.");
  }
  return result;
}
