// Tests for lib/aiBuilderStream — the SSE client of POST /admin/ai/builder/chat/stream.
// fetch is mocked with hand-rolled Response fakes (a custom reader fed from byte chunks) so the
// parser is exercised over REAL chunk boundaries: cut mid-line, cut mid-UTF-8-character, err
// events, plain-JSON refusals, the old-backend 404 fallback memo, and abort → cancelled error.
// chatWithAiBuilder is mocked at the lib/api seam (ApiError stays REAL — instanceof + status/code
// assertions must behave exactly as in production).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  aiBuilderChatStream, AiStreamCancelledError, resetStreamFallbackForTests, type AiStepEvent,
} from "@/lib/aiBuilderStream";
import { ApiError, chatWithAiBuilder } from "@/lib/api";
import type { AiChatResult } from "@/lib/aiBuilder";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, chatWithAiBuilder: vi.fn() };
});

const OK_RESULT: AiChatResult = {
  sessionId: "s1",
  reply: "Đề xuất của tôi đây.",
  draft: null,
  runId: "r1",
  usage: { promptTokens: 10, outputTokens: 5, totalTokens: 15 },
  outcome: "ok",
};

// ── Response fakes ────────────────────────────────────────────────────────────────────────────
const enc = new TextEncoder();

function readerFrom(chunks: Uint8Array[]) {
  let i = 0;
  return {
    read: async () =>
      i < chunks.length
        ? { done: false as const, value: chunks[i++] }
        : { done: true as const, value: undefined },
    cancel: async () => {},
  };
}

// An admitted SSE response whose body arrives in EXACTLY the given chunks.
function sseRes(chunks: Array<string | Uint8Array>): Response {
  const bytes = chunks.map((c) => (typeof c === "string" ? enc.encode(c) : c));
  return {
    status: 200,
    ok: true,
    headers: new Headers({ "Content-Type": "text/event-stream" }),
    body: { getReader: () => readerFrom(bytes) },
  } as unknown as Response;
}

// A pre-admission plain-JSON refusal, exactly like /chat's error envelope.
function jsonRes(body: unknown, status: number, headers: Record<string, string> = {}): Response {
  return {
    status,
    ok: status >= 200 && status < 300,
    headers: new Headers({ "Content-Type": "application/json", ...headers }),
    json: async () => body,
  } as unknown as Response;
}

// A bare route-level 404/405 (old backend, no JSON envelope — json() rejects like a text page).
function bareRes(status: number): Response {
  return {
    status,
    ok: false,
    headers: new Headers({ "Content-Type": "text/plain" }),
    json: async () => {
      throw new Error("not json");
    },
  } as unknown as Response;
}

function mockFetch(...responses: Response[]) {
  const fn = vi.fn();
  for (const r of responses) fn.mockResolvedValueOnce(r);
  global.fetch = fn as unknown as typeof fetch;
  return fn;
}

const RESULT_BLOCK = `event:result\ndata:${JSON.stringify({ data: OK_RESULT })}\n\n`;

function runStream(chunks: Array<string | Uint8Array>, onStep: (e: AiStepEvent) => void = () => {}) {
  mockFetch(sseRes(chunks));
  return aiBuilderChatStream("Tạo đối tượng hợp đồng", undefined, {
    onStep,
    signal: new AbortController().signal,
  });
}

let originalFetch: typeof fetch;

beforeEach(() => {
  originalFetch = global.fetch;
  resetStreamFallbackForTests();
  vi.mocked(chatWithAiBuilder).mockReset();
  window.localStorage.clear();
  window.sessionStorage.clear();
});

afterEach(() => {
  global.fetch = originalFetch;
});

// ── Happy path ────────────────────────────────────────────────────────────────────────────────
describe("aiBuilderChatStream — admitted stream", () => {
  it("step×N → result → done: onStep fires per frame, resolves with the exact /chat body data", async () => {
    const steps: AiStepEvent[] = [];
    const res = await runStream(
      [
        'event:step\ndata:{"step":1,"maxSteps":14,"phase":"thinking"}\n\n',
        'event:step\ndata:{"step":2,"maxSteps":14,"phase":"calling_tool","tool":"list_packs"}\n\n',
        'event:step\ndata:{"step":2,"maxSteps":14,"phase":"tool_done","tool":"list_packs"}\n\n',
        RESULT_BLOCK,
        "event:done\ndata:{}\n\n",
      ],
      (e) => steps.push(e),
    );

    expect(res).toEqual(OK_RESULT);
    expect(steps).toEqual([
      { step: 1, maxSteps: 14, phase: "thinking" },
      { step: 2, maxSteps: 14, phase: "calling_tool", tool: "list_packs" },
      { step: 2, maxSteps: 14, phase: "tool_done", tool: "list_packs" },
    ]);
    // The stream endpoint was hit with the /chat body shape (message only — no sessionId sent).
    const [url, init] = vi.mocked(global.fetch).mock.calls[0] as [string, RequestInit];
    expect(url).toContain("/admin/ai/builder/chat/stream");
    expect(JSON.parse(init.body as string)).toEqual({ message: "Tạo đối tượng hợp đồng" });
  });

  it("a chunk cut MID-line and MID-UTF-8-character still parses (buffer + streaming decoder)", async () => {
    // Cut point 1: inside the literal "event:step" line. Cut point 2: inside the multibyte "Đ"
    // of the result reply — the byte sequence of the WHOLE tail is split at an odd offset.
    const head = 'event:step\ndata:{"step":1,"maxSteps":14,"phase":"thinking"}\n\nevent:st';
    const tailBytes = enc.encode("ep\ndata:" + '{"step":2,"maxSteps":14,"phase":"final"}' + "\n\n" + RESULT_BLOCK + "event:done\ndata:{}\n\n");
    // Find a split offset INSIDE a multibyte char: "Đ" (0xC4 0x90) appears in the reply text.
    let cut = -1;
    for (let i = 0; i < tailBytes.length; i++) {
      if (tailBytes[i] === 0xc4) {
        cut = i + 1; // split BETWEEN the 2 bytes of the character
        break;
      }
    }
    expect(cut).toBeGreaterThan(0);

    const steps: AiStepEvent[] = [];
    const res = await runStream(
      [head, tailBytes.slice(0, cut), tailBytes.slice(cut)],
      (e) => steps.push(e),
    );

    expect(steps.map((s) => s.phase)).toEqual(["thinking", "final"]);
    expect(res.reply).toBe("Đề xuất của tôi đây."); // multibyte survived the odd cut
  });

  it("event:err mid-stream throws the payload as ApiError (same shape as a /chat failure)", async () => {
    const p = runStream([
      'event:step\ndata:{"step":1,"maxSteps":14,"phase":"thinking"}\n\n',
      'event:err\ndata:{"error":"Cổng LLM quá chậm","code":"AI_TIMEOUT"}\n\n',
    ]);
    await expect(p).rejects.toMatchObject({ name: "ApiError", code: "AI_TIMEOUT", message: "Cổng LLM quá chậm" });
  });

  it("stream closing without a result throws gateway-generic", async () => {
    await expect(
      runStream(['event:step\ndata:{"step":1,"maxSteps":14,"phase":"thinking"}\n\n']),
    ).rejects.toMatchObject({ code: "AI_GATEWAY_ERROR" });
  });
});

// ── Pre-admission refusals (plain JSON) ───────────────────────────────────────────────────────
describe("aiBuilderChatStream — JSON refusals", () => {
  it("429 refusal → ApiError carrying status, code and details.retryAfter from the header", async () => {
    mockFetch(jsonRes({ error: "Quá nhanh", code: "AI_RATE_LIMITED" }, 429, { "Retry-After": "30" }));

    const p = aiBuilderChatStream("x", "s1", { onStep: () => {}, signal: new AbortController().signal });
    await expect(p).rejects.toMatchObject({
      status: 429,
      code: "AI_RATE_LIMITED",
      details: { retryAfter: 30 },
    });
    await expect(p.catch((e) => e instanceof ApiError)).resolves.toBe(true);
    expect(vi.mocked(chatWithAiBuilder)).not.toHaveBeenCalled();
  });

  it("a 404 WITH a JSON code (SESSION_NOT_FOUND) is a real refusal — NO old-backend fallback", async () => {
    mockFetch(jsonRes({ error: "Phiên không tồn tại", code: "SESSION_NOT_FOUND" }, 404));

    await expect(
      aiBuilderChatStream("x", "dead-session", { onStep: () => {}, signal: new AbortController().signal }),
    ).rejects.toMatchObject({ status: 404, code: "SESSION_NOT_FOUND" });
    expect(vi.mocked(chatWithAiBuilder)).not.toHaveBeenCalled();
  });
});

// ── Old-backend fallback memo ─────────────────────────────────────────────────────────────────
describe("aiBuilderChatStream — old-backend fallback (bare 404)", () => {
  it("bare 404 → falls back to chatWithAiBuilder AND memoizes: the next turn skips the probe", async () => {
    const fetchFn = mockFetch(bareRes(404));
    vi.mocked(chatWithAiBuilder).mockResolvedValue(OK_RESULT);
    const opts = { onStep: () => {}, signal: new AbortController().signal };

    const r1 = await aiBuilderChatStream("lượt 1", undefined, opts);
    const r2 = await aiBuilderChatStream("lượt 2", "s1", opts);

    expect(r1).toEqual(OK_RESULT);
    expect(r2).toEqual(OK_RESULT);
    expect(fetchFn).toHaveBeenCalledTimes(1); // probed exactly once
    expect(vi.mocked(chatWithAiBuilder)).toHaveBeenNthCalledWith(1, "lượt 1", undefined, undefined);
    expect(vi.mocked(chatWithAiBuilder)).toHaveBeenNthCalledWith(2, "lượt 2", "s1", undefined);
  });
});

// ── Abort ─────────────────────────────────────────────────────────────────────────────────────
describe("aiBuilderChatStream — abort", () => {
  it("aborting mid-stream throws AiStreamCancelledError, never a plain error", async () => {
    const ctrl = new AbortController();
    // Reader: first chunk arrives, then the read hangs until the signal aborts (like a real fetch).
    let served = false;
    const body = {
      getReader: () => ({
        read: () =>
          served
            ? new Promise<never>((_resolve, reject) => {
                ctrl.signal.addEventListener("abort", () =>
                  reject(Object.assign(new Error("aborted"), { name: "AbortError" })),
                );
              })
            : ((served = true),
              Promise.resolve({
                done: false as const,
                value: enc.encode('event:step\ndata:{"step":1,"maxSteps":14,"phase":"thinking"}\n\n'),
              })),
        cancel: async () => {},
      }),
    };
    mockFetch({
      status: 200,
      ok: true,
      headers: new Headers({ "Content-Type": "text/event-stream" }),
      body,
    } as unknown as Response);

    const steps: AiStepEvent[] = [];
    const p = aiBuilderChatStream("x", undefined, { onStep: (e) => steps.push(e), signal: ctrl.signal });
    // Let the first read/step land, then cancel.
    await new Promise((r) => setTimeout(r, 0));
    ctrl.abort();

    await expect(p).rejects.toBeInstanceOf(AiStreamCancelledError);
    expect(steps).toHaveLength(1);
  });
});

describe("aiBuilderChatStream — project switch in progress (FR-128)", () => {
  it("sends nothing while the studio is changing project", async () => {
    const { beginTransition, endTransition } = await import("@/lib/studioContext");
    const fetchSpy = vi.fn();
    global.fetch = fetchSpy as unknown as typeof fetch;
    beginTransition();
    try {
      await expect(
        aiBuilderChatStream("x", undefined, { onStep: () => {}, signal: new AbortController().signal }),
      ).rejects.toBeInstanceOf(AiStreamCancelledError);
    } finally {
      endTransition();
    }
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(chatWithAiBuilder).not.toHaveBeenCalled();
  });
});
