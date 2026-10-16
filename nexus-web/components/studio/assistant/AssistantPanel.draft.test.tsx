// specs/076 ai9 (#815): the DOCKED assistant carries the same delivery-aware card as the old AI builder page (label, Áp ngay, fail-closed, apply-all).
// Seams mocked: the stream client, the identity/scope module and the write APIs. The chat hook, the provider, the
// panel and ProposalCard are the REAL ones.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

let search = "";
vi.mock("next/navigation", () => ({ useSearchParams: () => new URLSearchParams(search) }));

const sc = { phase: "idle" as "idle" | "running" | "failed", matches: true, ws: null as { tenantSlug: string; tenantName: string } | null, home: "northwind" };
vi.mock("@/lib/studioContext", () => ({
  useStudioIdentity: () => ({ status: "ready", me: undefined, home: sc.home, sessionReadable: true, matches: () => sc.matches, retry: vi.fn() }),
  transitionPhase: () => sc.phase,
  workspaceOf: () => sc.ws,
  homeSlug: () => sc.home,
  sessionReadable: () => true,
}));
vi.mock("@/lib/previewSession", () => ({ getPreviewToken: () => null }));
const pub = { q: {} as Record<string, unknown> };
vi.mock("../usePublishState", () => ({ usePublishState: () => pub.q }));

vi.mock("@/lib/aiBuilderStream", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/aiBuilderStream")>();
  return { ...actual, aiBuilderChatStream: vi.fn() };
});
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, aiHealth: vi.fn(async () => true), addField: vi.fn(async () => ({})), listAiSessions: vi.fn(async () => []) };
});

import { aiBuilderChatStream } from "@/lib/aiBuilderStream";
import { addField, aiHealth, ApiError, listAiSessions } from "@/lib/api";
import { resetAssistantOpenForTests } from "@/lib/assistantOpen";
import { parseStudioPath } from "@/lib/studioRoutes";
import { AssistantMount } from "./AssistantMount";
import { AssistantProvider } from "./AssistantProvider";

const stream = vi.mocked(aiBuilderChatStream);
const reply = (text: string, draft: unknown = null) => ({ sessionId: "s1", reply: text, draft, runId: "r", usage: undefined, outcome: undefined }) as never;

function wide(on: boolean) {
  window.matchMedia = vi.fn().mockImplementation((q: string) => ({
    matches: on, media: q, addEventListener: () => {}, removeEventListener: () => {},
  })) as unknown as typeof window.matchMedia;
}
function mount(path = "/studio/du-an/northwind/dung/du-lieu/benh_nhan") {
  const p = parseStudioPath(path)!;
  return render(
    <QueryClientProvider client={new QueryClient()}>
      <AssistantProvider slug="northwind" projectName="Xưởng Nexus" route={p.route} params={p.params} panel={<AssistantMount />}>
        <p>trang</p>
      </AssistantProvider>
    </QueryClientProvider>,
  );
}
const box = () => screen.getByRole("textbox") as HTMLTextAreaElement;
async function say(text: string) {
  fireEvent.change(box(), { target: { value: text } });
  fireEvent.keyDown(box(), { key: "Enter" });
  await act(async () => {});
}

beforeEach(() => {
  search = ""; stream.mockReset(); vi.mocked(aiHealth).mockClear(); vi.mocked(aiHealth).mockResolvedValue(true); vi.mocked(addField).mockClear(); vi.mocked(listAiSessions).mockClear();
  Object.assign(sc, { phase: "idle", matches: true, ws: null, home: "northwind" });
  window.localStorage.clear(); resetAssistantOpenForTests(); wide(true);
});


const RULE = { kind: "workflow", workflow: { name: "Nhắc hẹn", entityName: "lich_hen", trigger: "after_update", actions: [] }, change: { docType: "workflow", op: "update", label: "Nhắc hẹn", target: { entity: "lich_hen" } } };
const STATE = { delivered: true, headVersion: { versionNo: 3 }, pendingCount: 0, changesetHash: "h", draftableTypes: ["workflow", "field"], directTypes: ["role"], propagationSeconds: 5 };

describe("AssistantPanel on a delivered project (ai9)", () => {
  it("delivered + draftable kind: button says it goes to the draft, no old ConfirmDialog", async () => {
    pub.q = { data: STATE };
    stream.mockResolvedValue(reply("Sửa luật.", [RULE]));
    mount();
    await say("sửa luật nhắc hẹn");
    expect(await screen.findByRole("button", { name: "Áp dụng vào bản nháp" })).toBeTruthy();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("delivery unknown (publish/state 500): fail closed — Áp ngay + Kiểm tra lại, never 'not delivered'", async () => {
    pub.q = { error: new ApiError(500, "INTERNAL", "x") };
    stream.mockResolvedValue(reply("Sửa luật.", [RULE]));
    mount();
    await say("sửa luật nhắc hẹn");
    expect(await screen.findByRole("button", { name: "Áp ngay" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Kiểm tra lại" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Áp dụng vào bản nháp" })).toBeNull();
  });

  it("undelivered control: plain Áp dụng, no draft wording", async () => {
    pub.q = { data: { delivered: false, draftableTypes: [] } };
    stream.mockResolvedValue(reply("Sửa luật.", [RULE]));
    mount();
    await say("sửa luật nhắc hẹn");
    expect(await screen.findByRole("button", { name: "Áp dụng" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Áp dụng vào bản nháp" })).toBeNull();
  });
});
