// Tests for the ⌘K AI assistant client surface: lib/api.ts assistantAsk + the /assistant/sessions
// CRUD (endpoint + body + response parsing) and lib/assistant.ts assistantNotice (honest
// 503/429/error mapping, mirroring the assistant panel M4 FE policy) + the ⌘K-can-act draft model
// (tolerant accessors, display summary, session→turn mapper).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  assistantAsk, listAssistantSessions, getAssistantSession, deleteAssistantSession,
  logout, ApiError,
} from "@/lib/api";
import {
  assistantNotice, getRecordCreateDraft, getRecordUpdateDraft,
  assistantDraftSummary, assistantDraftValueText, assistantMessagesToTurns,
  type AssistantDraft,
} from "@/lib/assistant";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });
}

const OK_BODY = {
  data: {
    sessionId: "sess-1",
    reply: "Có **3** cơ hội đang mở.",
    usage: { promptTokens: 100, outputTokens: 20, totalTokens: 120 },
  },
};

describe("assistantAsk (lib/api)", () => {
  let originalFetch: FetchFn;

  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    // Reset the module-level token cache (same pattern as api-mfa.test.ts): logout() clears it.
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout();
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("POSTs /assistant/ask with {message} only when no sessionId, and parses the reply envelope", async () => {
    const fetchMock = vi.fn(async () => jsonRes(OK_BODY, 200));
    global.fetch = fetchMock as unknown as typeof fetch;

    const res = await assistantAsk("có bao nhiêu cơ hội đang mở?");

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toMatch(/\/assistant\/ask$/);
    expect(init.method).toBe("POST");
    // sessionId must be ABSENT (not null/undefined-serialized) on a fresh thread.
    expect(JSON.parse(init.body as string)).toEqual({ message: "có bao nhiêu cơ hội đang mở?" });

    expect(res.sessionId).toBe("sess-1");
    expect(res.reply).toBe("Có **3** cơ hội đang mở.");
    expect(res.usage.totalTokens).toBe(120);
  });

  it("passes sessionId through for a follow-up turn", async () => {
    const fetchMock = vi.fn(async () => jsonRes(OK_BODY, 200));
    global.fetch = fetchMock as unknown as typeof fetch;

    await assistantAsk("còn tháng trước?", "sess-1");

    const [, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(JSON.parse(init.body as string)).toEqual({ message: "còn tháng trước?", sessionId: "sess-1" });
  });

  it("503 AI_DISABLED → ApiError with code AI_DISABLED", async () => {
    global.fetch = vi.fn(async () =>
      jsonRes({ error: "AI đang tắt", code: "AI_DISABLED" }, 503),
    ) as unknown as typeof fetch;

    await expect(assistantAsk("hi")).rejects.toMatchObject({ status: 503, code: "AI_DISABLED" });
  });

  it("429 AI_RATE_LIMITED → ApiError carries details.retryAfter from the Retry-After header", async () => {
    global.fetch = vi.fn(async () =>
      jsonRes({ error: "quá nhanh", code: "AI_RATE_LIMITED" }, 429, { "Retry-After": "42" }),
    ) as unknown as typeof fetch;

    await expect(assistantAsk("hi")).rejects.toMatchObject({
      status: 429,
      code: "AI_RATE_LIMITED",
      details: { retryAfter: 42 },
    });
  });

  it("passes drafts[] through the reply envelope (⌘K-can-act)", async () => {
    const drafts = [
      { kind: "record_create", recordCreate: { entity: "contacts", data: { name: "Anh A" } } },
    ];
    global.fetch = vi.fn(async () =>
      jsonRes({ data: { ...OK_BODY.data, drafts } }, 200),
    ) as unknown as typeof fetch;

    const res = await assistantAsk("tạo liên hệ Anh A");
    expect(res.drafts).toEqual(drafts);
  });

  it("listAssistantSessions GETs /assistant/sessions and unwraps data ([] when null)", async () => {
    const rows = [{ id: "as1", title: "Đếm cơ hội", updatedAt: "2026-07-26T08:00:00Z" }];
    const fetchMock = vi.fn(async () => jsonRes({ data: rows }, 200));
    global.fetch = fetchMock as unknown as typeof fetch;

    expect(await listAssistantSessions()).toEqual(rows);
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toMatch(/\/assistant\/sessions$/);
    expect(init.method).toBe("GET");

    global.fetch = vi.fn(async () => jsonRes({ data: null }, 200)) as unknown as typeof fetch;
    expect(await listAssistantSessions()).toEqual([]);
  });

  it("getAssistantSession GETs /assistant/sessions/:id and returns messages with drafts", async () => {
    const detail = {
      id: "as1", title: "Soạn liên hệ", createdAt: "2026-07-26T08:00:00Z", updatedAt: "2026-07-26T08:05:00Z",
      messages: [
        { seq: 1, role: "user", content: "tạo liên hệ", drafts: null, createdAt: "2026-07-26T08:00:00Z" },
        {
          seq: 2, role: "assistant", content: "Nháp đây.", createdAt: "2026-07-26T08:00:05Z",
          drafts: [{ kind: "record_create", recordCreate: { entity: "contacts", data: { name: "Anh A" } } }],
        },
      ],
    };
    const fetchMock = vi.fn(async () => jsonRes({ data: detail }, 200));
    global.fetch = fetchMock as unknown as typeof fetch;

    expect(await getAssistantSession("as1")).toEqual(detail);
    const [url] = fetchMock.mock.calls[0] as unknown as [string];
    expect(url).toMatch(/\/assistant\/sessions\/as1$/);
  });

  it("deleteAssistantSession DELETEs /assistant/sessions/:id (204, no body parse)", async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 204 }));
    global.fetch = fetchMock as unknown as typeof fetch;

    await deleteAssistantSession("as1");
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toMatch(/\/assistant\/sessions\/as1$/);
    expect(init.method).toBe("DELETE");
  });
});

describe("assistantNotice (lib/assistant)", () => {
  it("503 AI_DISABLED → kind 'disabled' with the honest off message", () => {
    const n = assistantNotice(new ApiError(503, "AI_DISABLED", "AI đang tắt"));
    expect(n.kind).toBe("disabled");
    expect(n.text).toBe("Trợ lý AI đang tắt trên máy chủ.");
  });

  it("429 AI_RATE_LIMITED with retryAfter → kind 'limit' with the ~Ns wording", () => {
    const n = assistantNotice(new ApiError(429, "AI_RATE_LIMITED", "quá nhanh", { retryAfter: 42 }));
    expect(n.kind).toBe("limit");
    expect(n.text).toBe("Đang gửi quá nhanh — thử lại sau ~42s.");
  });

  it("429 AI_RATE_LIMITED without retryAfter → seconds-free wording (never a fake countdown)", () => {
    const n = assistantNotice(new ApiError(429, "AI_RATE_LIMITED", "quá nhanh"));
    expect(n.kind).toBe("limit");
    expect(n.text).toBe("Đang gửi quá nhanh — chờ một chút rồi gửi lại.");
  });

  it("429 AI_BUDGET_EXCEEDED → kind 'limit' with the per-tenant daily budget message", () => {
    const n = assistantNotice(new ApiError(429, "AI_BUDGET_EXCEEDED", "hết ngân sách"));
    expect(n.kind).toBe("limit");
    expect(n.text).toMatch(/ngân sách token AI/);
  });

  it("unknown 429 code → kind 'limit' falling back to the central error map / message", () => {
    const n = assistantNotice(new ApiError(429, "SOME_FUTURE_LIMIT", "raw server text"));
    expect(n.kind).toBe("limit");
    expect(n.text).toBe("raw server text");
  });

  it("other ApiError (e.g. 502 AI_GATEWAY_ERROR) → kind 'error' via errorMessage map", () => {
    const n = assistantNotice(new ApiError(502, "AI_GATEWAY_ERROR", "bad gateway"));
    expect(n.kind).toBe("error");
    expect(n.text).toBe("Cổng LLM gặp lỗi. Vui lòng thử lại sau giây lát.");
  });

  it("non-ApiError → kind 'error' with a generic fallback", () => {
    const n = assistantNotice(new TypeError("Failed to fetch"));
    expect(n.kind).toBe("error");
    expect(n.text).toBe("Failed to fetch");
  });
});

describe("record draft accessors (⌘K-can-act — tolerant, null on malformed)", () => {
  const CREATE: AssistantDraft = {
    kind: "record_create",
    recordCreate: { entity: "contacts", data: { name: "Anh A", email: "a@vd.vn" } },
  };
  const UPDATE: AssistantDraft = {
    kind: "record_update",
    recordUpdate: {
      entity: "deals", recordId: "rec-77", data: { stage: "won" }, before: { stage: "open" },
    },
  };

  it("reads a valid record_create payload", () => {
    expect(getRecordCreateDraft(CREATE)).toEqual({ entity: "contacts", data: { name: "Anh A", email: "a@vd.vn" } });
  });

  it("accepts the snake_case twin container key (persisted draft_json drift)", () => {
    const snake = { kind: "record_create", record_create: { entity: "contacts", data: { name: "B" } } } as unknown as AssistantDraft;
    expect(getRecordCreateDraft(snake)?.entity).toBe("contacts");
    const snakeUp = {
      kind: "record_update",
      record_update: { entity: "deals", recordId: "r1", data: { stage: "won" } },
    } as unknown as AssistantDraft;
    expect(getRecordUpdateDraft(snakeUp)?.recordId).toBe("r1");
  });

  it("missing/malformed payload → null, never a crash", () => {
    expect(getRecordCreateDraft({ kind: "record_create" })).toBeNull();
    expect(getRecordCreateDraft({ kind: "record_create", recordCreate: null })).toBeNull();
    expect(getRecordCreateDraft({ kind: "record_create", recordCreate: { entity: "", data: {} } })).toBeNull();
    expect(getRecordCreateDraft({
      kind: "record_create",
      recordCreate: { entity: "contacts", data: "oops" as unknown as Record<string, unknown> },
    })).toBeNull();
    expect(getRecordUpdateDraft({ kind: "record_update" })).toBeNull();
    expect(getRecordUpdateDraft({
      kind: "record_update",
      recordUpdate: { entity: "deals", recordId: "", data: {} },
    })).toBeNull();
  });

  it("reads a valid record_update payload incl. the before diff; non-object before is dropped", () => {
    expect(getRecordUpdateDraft(UPDATE)).toEqual({
      entity: "deals", recordId: "rec-77", data: { stage: "won" }, before: { stage: "open" },
    });
    const noBefore = {
      kind: "record_update",
      recordUpdate: { entity: "deals", recordId: "r2", data: { stage: "won" }, before: 7 },
    } as unknown as AssistantDraft;
    expect(getRecordUpdateDraft(noBefore)?.before).toBeUndefined();
  });

  it("assistantDraftValueText: —/Có/Không/verbatim strings/JSON for non-scalars", () => {
    expect(assistantDraftValueText(null)).toBe("—");
    expect(assistantDraftValueText(undefined)).toBe("—");
    expect(assistantDraftValueText("")).toBe("—");
    expect(assistantDraftValueText(true)).toBe("Có");
    expect(assistantDraftValueText(false)).toBe("Không");
    expect(assistantDraftValueText("Anh A")).toBe("Anh A"); // no JSON quotes on user text
    expect(assistantDraftValueText(12.5)).toBe("12.5");
    expect(assistantDraftValueText({ amount: 5 })).toBe('{"amount":5}');
  });

  it("assistantDraftSummary: entity + field count + first pairs, '…' past the cap, null on malformed", () => {
    expect(assistantDraftSummary(CREATE)).toBe("contacts · 2 trường · name: Anh A, email: a@vd.vn");
    const wide: AssistantDraft = {
      kind: "record_create",
      recordCreate: { entity: "deals", data: { a: 1, b: 2, c: 3, d: 4 } },
    };
    expect(assistantDraftSummary(wide)).toBe("deals · 4 trường · a: 1, b: 2, c: 3…");
    expect(assistantDraftSummary(UPDATE)).toBe("deals · 1 trường · stage: won");
    expect(assistantDraftSummary({ kind: "record_create" })).toBeNull();
  });
});

describe("assistantMessagesToTurns (resume mapper)", () => {
  it("coerces roles, keys by seq, and keeps drafts only on assistant rows with items", () => {
    const drafts = [{ kind: "record_create", recordCreate: { entity: "contacts", data: {} } }];
    const turns = assistantMessagesToTurns([
      { seq: 1, role: "user", content: "tạo liên hệ", drafts: null, createdAt: "t1" },
      { seq: 2, role: "assistant", content: "Nháp đây.", drafts, createdAt: "t2" },
      { seq: 3, role: "weird", content: "x", drafts, createdAt: "t3" }, // coerced to user → drafts dropped
      { seq: 4, role: "assistant", content: "hết", drafts: [], createdAt: "t4" }, // empty → no drafts key
    ]);
    expect(turns.map((t) => t.id)).toEqual(["m_1", "m_2", "m_3", "m_4"]);
    expect(turns[0]).toEqual({ id: "m_1", role: "user", content: "tạo liên hệ" });
    expect(turns[1].drafts).toEqual(drafts);
    expect(turns[2].role).toBe("user");
    expect(turns[2].drafts).toBeUndefined();
    expect(turns[3].drafts).toBeUndefined();
  });
});
