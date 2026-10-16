// useAssistantChat — the ask/answer flow on its own (specs/076 lát ai3a, T054). The stream client is
// the mocked seam; ApiError and AiStreamCancelledError stay REAL so the hook's instanceof branches run.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/react";

vi.mock("@/lib/aiBuilderStream", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/aiBuilderStream")>();
  return { ...actual, aiBuilderChatStream: vi.fn() };
});

import { aiBuilderChatStream, AiStreamCancelledError } from "@/lib/aiBuilderStream";
import { ApiError } from "@/lib/api";
import type { AiChatResult } from "@/lib/aiBuilder";
import { useAssistantChat, stepLabel } from "./useAssistantChat";

const stream = vi.mocked(aiBuilderChatStream);
const ok = (over: Partial<AiChatResult> = {}): AiChatResult =>
  ({ sessionId: "s1", reply: "Xong.", draft: null, runId: "r1", usage: undefined, outcome: undefined, ...over }) as AiChatResult;

beforeEach(() => { stream.mockReset(); });

describe("useAssistantChat", () => {
  it("a result becomes an assistant turn, adopts the sessionId, and asks the host to refresh sessions", async () => {
    stream.mockResolvedValue(ok());
    const changed = vi.fn();
    const { result } = renderHook(() => useAssistantChat({ onSessionsChanged: changed }));
    await act(async () => { await result.current.runTurn("xin chào"); });
    expect(result.current.turns.map((t) => t.content)).toEqual(["Xong."]);
    expect(result.current.sessionId).toBe("s1");
    expect(changed).toHaveBeenCalledTimes(1);
    expect(result.current.sending).toBe(false);
  });

  it("429 shows a limit notice and the hook is free to send again", async () => {
    stream.mockRejectedValue(new ApiError(429, "AI_RATE_LIMITED", "quá nhanh", { retryAfter: 5 }));
    const { result } = renderHook(() => useAssistantChat());
    await act(async () => { await result.current.runTurn("a"); });
    expect(result.current.turnError?.kind).toBe("limit");
    expect(result.current.sending).toBe(false);
    stream.mockResolvedValue(ok());
    await act(async () => { await result.current.runTurn("b"); });
    expect(result.current.turnError).toBeNull();
  });

  // Ported from the deleted page tests (specs/076 ai11): the honest wording per 429 code.
  it.each([
    ["AI_BUDGET_EXCEEDED", undefined, "Đã chạm ngân sách token AI hôm nay của tenant — reset lúc 00:00 UTC (07:00 giờ Việt Nam)."],
    ["AI_RATE_LIMITED", { retryAfter: 30 }, "Đang gửi quá nhanh — thử lại sau ~30s."],
    ["AI_RATE_LIMITED", undefined, "Đang gửi quá nhanh — chờ một chút rồi gửi lại."],
    ["AI_SOME_FUTURE_LIMIT", undefined, "Giới hạn mới phía máy chủ — thử lại sau"],
  ] as const)("429 %s %j reads %s", async (code, details, text) => {
    stream.mockRejectedValue(new ApiError(429, code, "Giới hạn mới phía máy chủ — thử lại sau", details as never));
    const { result } = renderHook(() => useAssistantChat());
    await act(async () => { await result.current.runTurn("a"); });
    expect(result.current.turnError).toEqual({ kind: "limit", text });
  });

  it("500 is an error notice; 503 AI_DISABLED switches the assistant off", async () => {
    stream.mockRejectedValueOnce(new ApiError(500, "INTERNAL", "lỗi", {}));
    const { result } = renderHook(() => useAssistantChat());
    await act(async () => { await result.current.runTurn("a"); });
    expect(result.current.turnError?.kind).toBe("error");
    stream.mockRejectedValueOnce(new ApiError(503, "AI_DISABLED", "tắt", {}));
    await act(async () => { await result.current.runTurn("b"); });
    expect(result.current.aiDisabled).toBe(true);
  });

  it("SESSION_NOT_FOUND detaches the session so the next send starts fresh", async () => {
    stream.mockResolvedValueOnce(ok());
    const { result } = renderHook(() => useAssistantChat());
    await act(async () => { await result.current.runTurn("a"); });
    stream.mockRejectedValueOnce(new ApiError(404, "SESSION_NOT_FOUND", "mất", {}));
    await act(async () => { await result.current.runTurn("b"); });
    expect(result.current.sessionId).toBeNull();
    expect(result.current.turnError?.kind).toBe("error");
  });

  it("Huỷ sets the neutral note, not the error notice, and frees the lock", async () => {
    stream.mockImplementation((_m, _s, o) => new Promise((_res, rej) => {
      o.signal.addEventListener("abort", () => rej(new AiStreamCancelledError()));
    }));
    const { result } = renderHook(() => useAssistantChat());
    let p!: Promise<boolean>;
    act(() => { p = result.current.runTurn("a"); });
    expect(result.current.sending).toBe(true);
    act(() => result.current.cancel());
    await act(async () => { await p; });
    expect(result.current.cancelled).toBe(true);
    expect(result.current.turnError).toBeNull();
    expect(result.current.sending).toBe(false);
  });

  it("a second turn while one is in flight is refused (single in-flight lock)", async () => {
    let settle!: (r: AiChatResult) => void;
    stream.mockImplementation(() => new Promise((res) => { settle = res; }));
    const { result } = renderHook(() => useAssistantChat());
    let first!: Promise<boolean>;
    act(() => { first = result.current.runTurn("a"); });
    let second = true;
    await act(async () => { second = await result.current.runTurn("b"); });
    expect(second).toBe(false);
    expect(stream).toHaveBeenCalledTimes(1);
    await act(async () => { settle(ok()); await first; });
  });

  it("assertScope false: nothing is sent (FR-128)", async () => {
    const { result } = renderHook(() => useAssistantChat({ assertScope: () => false }));
    let sent = true;
    await act(async () => { sent = await result.current.runTurn("a"); });
    expect(sent).toBe(false);
    expect(stream).not.toHaveBeenCalled();
    expect(result.current.sending).toBe(false);
  });

  it("nexus:workspace-ended orphans the in-flight result: no turn, no error, no second notice", async () => {
    let settle!: (r: AiChatResult) => void;
    stream.mockImplementation(() => new Promise((res) => { settle = res; }));
    const { result } = renderHook(() => useAssistantChat());
    let p!: Promise<boolean>;
    act(() => { p = result.current.runTurn("a"); });
    act(() => { window.dispatchEvent(new Event("nexus:workspace-ended")); });
    await act(async () => { settle(ok()); await p; });
    expect(result.current.turns).toEqual([]);
    expect(result.current.sessionId).toBeNull();
    expect(result.current.turnError).toBeNull();
    expect(result.current.cancelled).toBe(false); // no stray "Đã huỷ" note either
  });

  it("reset (new conversation) orphans a late error too", async () => {
    let fail!: (e: unknown) => void;
    stream.mockImplementation(() => new Promise((_r, rej) => { fail = rej; }));
    const { result } = renderHook(() => useAssistantChat());
    let p!: Promise<boolean>;
    act(() => { p = result.current.runTurn("a"); });
    act(() => result.current.reset());
    await act(async () => { fail(new ApiError(500, "X", "x", {})); await p; });
    expect(result.current.turnError).toBeNull();
    expect(result.current.cancelled).toBe(false);
  });

  it("reportApplyFailure sends one system turn carrying code and server message, not the card payload", async () => {
    stream.mockResolvedValue(ok());
    const { result } = renderHook(() => useAssistantChat());
    await act(async () => {
      await result.current.reportApplyFailure(
        { kind: "entity", entity: { name: "khach" } } as never,
        new ApiError(422, "VALIDATION", "trùng tên", {}),
      );
    });
    expect(stream).toHaveBeenCalledTimes(1);
    expect(stream.mock.calls[0][0]).toContain("VALIDATION");
    expect(stream.mock.calls[0][0]).toContain("trùng tên");
  });

  it("step summary collapses finished tools into business labels", async () => {
    stream.mockImplementation(async (_m, _s, o) => {
      o.onStep({ step: 1, maxSteps: 14, phase: "tool_done", tool: "list_roles" });
      o.onStep({ step: 2, maxSteps: 14, phase: "final" });
      return ok();
    });
    const { result } = renderHook(() => useAssistantChat());
    await act(async () => { await result.current.runTurn("a"); });
    await waitFor(() => expect(Object.values(result.current.stepSummaries)[0]).toEqual({ count: 2, tools: ["Đang xem các vai"] }));
  });

  it("stepLabel: business words for read tools and propose_* kinds; unknown tools keep their raw name", () => {
    expect(stepLabel("list_relationships")).toBe("Đang xem các liên kết giữa bảng");
    expect(stepLabel("propose_workqueue")).toBe("Đang soạn đề xuất hàng việc");
    expect(stepLabel("mystery_tool")).toBe("mystery_tool");
  });
});
