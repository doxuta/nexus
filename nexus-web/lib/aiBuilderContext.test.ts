// specs/067-ai-context L10-1 (T013): the ONE request-body builder, the URL→context reader, the
// Go↔FE panel referee, the three stream fallbacks carrying the same context, and a source scan that
// keeps every "/admin/ai/builder/chat" caller on buildAiChatBody (an unwired caller = a context
// silently dropped — the "đoạn nối không ai viết" bug-class).
import fs from "node:fs";
import path from "node:path";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { buildAiChatBody, needsLiveConfirm, appliedFootKey } from "@/lib/aiBuilder";
import { PANEL_LABELS } from "@/lib/workshopPanels";
import { aiBuilderChatStream, resetStreamFallbackForTests } from "@/lib/aiBuilderStream";
import { ApiError, chatWithAiBuilder } from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, chatWithAiBuilder: vi.fn() };
});

describe("buildAiChatBody", () => {
  it("no context → just {message}", () => {
    expect(buildAiChatBody("hi")).toEqual({ message: "hi" });
  });
  it("sessionId is carried", () => {
    expect(buildAiChatBody("hi", "s1")).toEqual({ message: "hi", sessionId: "s1" });
  });
  it("an all-empty context has NO context key", () => {
    expect(buildAiChatBody("hi", undefined, {})).toEqual({ message: "hi" });
    expect(buildAiChatBody("hi", undefined, { app: "", panel: "  " })).toEqual({ message: "hi" });
    expect("context" in buildAiChatBody("hi", undefined, { app: "" })).toBe(false);
  });
  it("only non-empty keys are sent", () => {
    expect(buildAiChatBody("hi", "s1", { app: "clinic", panel: "" })).toEqual({
      message: "hi", sessionId: "s1", context: { app: "clinic" },
    });
    expect(buildAiChatBody("hi", undefined, { app: "clinic", panel: "tu-dong" }).context).toEqual({ app: "clinic", panel: "tu-dong" });
  });
});

describe("buildAiChatBody entity context", () => {
  it("buildAiChatBody sends entity + recordId only when non-empty", () => {
    expect(buildAiChatBody("hi", undefined, { app: "clinic", entity: "x", recordId: "id" }).context)
      .toEqual({ app: "clinic", entity: "x", recordId: "id" });
    expect(buildAiChatBody("hi", undefined, { entity: " " })).toEqual({ message: "hi" });
  });
});

describe("needsLiveConfirm (specs/067 T038a)", () => {
  const appTable = { kind: "entity", entity: { name: "t", label: "T", fields: [], appName: "clinic" } } as never;
  const plainTable = { kind: "entity", entity: { name: "t", label: "T", fields: [] } } as never;
  const appCard = { kind: "app", app: { name: "a", label: "A", navEntities: [] } } as never;
  it("only when delivered, and only for app-scoped tables and app cards", () => {
    expect(needsLiveConfirm(appTable, true)).toBe(true);
    expect(needsLiveConfirm(appCard, true)).toBe(true);
    expect(needsLiveConfirm(plainTable, true)).toBe(false);
    expect(needsLiveConfirm(appTable, false)).toBe(false);
    expect(needsLiveConfirm(appCard, false)).toBe(false);
  });
});

describe("PANEL_LABELS ↔ Go studio_panels.json", () => {
  it("same token set (the Go side compares its studioPanelHints to the same file)", () => {
    const golden = JSON.parse(
      fs.readFileSync(path.join(process.cwd(), "../nexus-platform/internal/ai/testdata/studio_panels.json"), "utf8"),
    ) as string[];
    expect(golden.length).toBeGreaterThan(0);
    expect(Object.keys(PANEL_LABELS).sort()).toEqual([...golden].sort());
  });
});

describe("every chat caller goes through buildAiChatBody", () => {
  it("each '/admin/ai/builder/chat' literal in lib/ sits in a file that builds its body with buildAiChatBody", () => {
    const libDir = path.join(process.cwd(), "lib");
    const offenders: string[] = [];
    let seen = 0;
    for (const f of fs.readdirSync(libDir)) {
      if (!/\.tsx?$/.test(f) || /\.test\./.test(f)) continue;
      const src = fs.readFileSync(path.join(libDir, f), "utf8");
      const hits = src.match(/["'`]\/admin\/ai\/builder\/chat(\/stream)?["'`]|\/admin\/ai\/builder\/chat\/stream`/g) ?? [];
      seen += hits.length;
      if (hits.length > 0 && !src.includes("buildAiChatBody(")) offenders.push(f);
    }
    expect(seen).toBeGreaterThanOrEqual(2); // a renamed route must not turn this scan into zero hits
    expect(offenders).toEqual([]);
  });
});

describe("aiBuilderChatStream carries the context", () => {
  const enc = new TextEncoder();
  const ctx = { app: "clinic", panel: "tu-dong" };
  const OK = { sessionId: "s1", reply: "ok", draft: null, runId: "r", usage: { promptTokens: 1, outputTokens: 1, totalTokens: 2 }, outcome: "ok" };

  beforeEach(() => {
    resetStreamFallbackForTests();
    vi.mocked(chatWithAiBuilder).mockReset();
    vi.mocked(chatWithAiBuilder).mockResolvedValue(OK as never);
  });

  const opts = () => ({ onStep: () => {}, signal: new AbortController().signal });

  it("stream body has the context", async () => {
    const body = enc.encode(`event:result\ndata:${JSON.stringify({ data: OK })}\n\nevent:done\ndata:{}\n\n`);
    let i = 0;
    global.fetch = vi.fn().mockResolvedValue({
      status: 200, ok: true, headers: new Headers({ "Content-Type": "text/event-stream" }),
      body: { getReader: () => ({ read: async () => (i++ === 0 ? { done: false, value: body } : { done: true, value: undefined }), cancel: async () => {} }) },
    }) as unknown as typeof fetch;
    await aiBuilderChatStream("m", "s1", { ...opts(), context: ctx });
    const [, init] = vi.mocked(global.fetch).mock.calls[0] as [string, RequestInit];
    expect(JSON.parse(init.body as string)).toEqual({ message: "m", sessionId: "s1", context: ctx });
  });

  it("bare 404 fallback, 401 fallback and the memoized path all pass the same context", async () => {
    const bare = { status: 404, ok: false, headers: new Headers({ "Content-Type": "text/plain" }), json: async () => { throw new Error("x"); } };
    global.fetch = vi.fn().mockResolvedValueOnce(bare) as unknown as typeof fetch;
    await aiBuilderChatStream("m", undefined, { ...opts(), context: ctx });
    expect(chatWithAiBuilder).toHaveBeenLastCalledWith("m", undefined, ctx); // bare 404

    await aiBuilderChatStream("m2", undefined, { ...opts(), context: ctx }); // memo: no fetch at all
    expect(chatWithAiBuilder).toHaveBeenLastCalledWith("m2", undefined, ctx);

    resetStreamFallbackForTests();
    const unauth = { status: 401, ok: false, headers: new Headers({ "Content-Type": "application/json" }), json: async () => ({ code: "UNAUTHORIZED" }) };
    global.fetch = vi.fn().mockResolvedValueOnce(unauth) as unknown as typeof fetch;
    await aiBuilderChatStream("m3", undefined, { ...opts(), context: ctx });
    expect(chatWithAiBuilder).toHaveBeenLastCalledWith("m3", undefined, ctx);
  });

  it("a coded 422 context refusal is thrown as ApiError, not swallowed by a fallback", async () => {
    const refusal = { status: 422, ok: false, headers: new Headers({ "Content-Type": "application/json" }),
      json: async () => ({ error: "x", code: "AI_CONTEXT_NOT_FOUND", details: { field: "context.app" } }) };
    global.fetch = vi.fn().mockResolvedValueOnce(refusal) as unknown as typeof fetch;
    await expect(aiBuilderChatStream("m", undefined, { ...opts(), context: ctx })).rejects.toMatchObject({
      status: 422, code: "AI_CONTEXT_NOT_FOUND",
    });
    expect(ApiError).toBeDefined();
    expect(chatWithAiBuilder).not.toHaveBeenCalled();
  });
});

describe("appliedFootKey order (specs/067 T085, plan §9)", () => {
  const k = (item: Record<string, unknown>, types: string[]) =>
    appliedFootKey(item as never, { delivered: true, draftableTypes: types });
  it("first match wins and unknown state says nothing", () => {
    expect(k({ kind: "records", records: {} }, ["field"])).toBe("page.studio.ai.foot.records");
    expect(k({ kind: "entity", entity: { name: "a" } }, ["field"])).toBe("page.studio.ai.foot.createdHidden");
    expect(k({ kind: "entity", entity: { name: "a", appName: "x" } }, ["field"])).toBe("page.studio.ai.foot.createdHiddenApp");
    expect(k({ kind: "field", field: { name: "a" } }, ["field"])).toBe("page.studio.ai.foot.createdHidden");
    expect(k({ kind: "relationship", relationship: { name: "a" } }, ["field"])).toBe("page.studio.ai.foot.createdHidden");
    expect(k({ kind: "app", app: {} }, ["app"])).toBe("page.studio.ai.foot.appLive");
    expect(k({ kind: "workflow" }, ["workflow"])).toBe("page.studio.ai.foot.draft");
    expect(k({ kind: "edit_field" }, ["field"])).toBe("page.studio.ai.foot.draft");
    expect(k({ kind: "delete_field" }, ["field"])).toBe("page.studio.ai.foot.live");
    expect(k({ kind: "relationship" }, ["workflow"])).toBe("page.studio.ai.foot.live");
    expect(k({ kind: "entity", entity: { name: "a" } }, ["workflow"])).toBe("page.studio.ai.foot.live");
    expect(appliedFootKey({ kind: "workflow" } as never, null)).toBeNull();
    expect(appliedFootKey({ kind: "workflow" } as never, { delivered: false, draftableTypes: ["workflow"] })).toBeNull();
  });
});
