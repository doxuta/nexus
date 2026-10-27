// Issue #3: a saved conversation (the thread an MCP agent writes its proposals to) opens in the docked panel as a
// resumed session: its turns, its cards ready to apply, and the panel open. The provider, the panel and ProposalCard
// are the REAL ones; the session read and the identity seams are mocked.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("next/navigation", () => ({ useSearchParams: () => new URLSearchParams("") }));
vi.mock("@/lib/studioContext", () => ({
  useStudioIdentity: () => ({ status: "ready", me: undefined, home: "northwind", sessionReadable: true, matches: () => true, retry: vi.fn() }),
  transitionPhase: () => "idle",
  workspaceOf: () => null,
  homeSlug: () => "northwind",
  sessionReadable: () => true,
}));
vi.mock("@/lib/previewSession", () => ({ getPreviewToken: () => null }));
const pub = { q: {} as Record<string, unknown> };
vi.mock("../usePublishState", () => ({ usePublishState: () => pub.q }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual, aiHealth: vi.fn(async () => false), getAiSession: vi.fn(),
    addField: vi.fn(async () => ({})), settleAiProposal: vi.fn(async () => undefined),
    createWorkflow: vi.fn(async () => ({})), listDraftChanges: vi.fn(), discardDraft: vi.fn(async () => undefined),
  };
});

import { addField, createWorkflow, discardDraft, getAiSession, listDraftChanges, settleAiProposal } from "@/lib/api";
import { resetAssistantOpenForTests, useAssistantOpen } from "@/lib/assistantOpen";
import { parseStudioPath } from "@/lib/studioRoutes";
import { AssistantMount } from "./AssistantMount";
import { AssistantProvider, useOpenThread } from "./AssistantProvider";

const THREAD = {
  id: "sess-1",
  title: "Agent qua MCP",
  createdAt: "2026-10-17T03:00:00Z",
  updatedAt: "2026-10-17T03:05:00Z",
  messages: [
    { seq: 0, role: "user", content: "[MCP] Kết nối agent bên ngoài", draft: null, createdAt: "2026-10-17T03:00:00Z" },
    { seq: 1, role: "assistant", content: "Đề xuất của agent hiện ở đây.", draft: null, createdAt: "2026-10-17T03:00:00Z" },
    { seq: 2, role: "user", content: "[MCP] Agent gọi nexus_propose_field", draft: null, createdAt: "2026-10-17T03:05:00Z" },
    {
      seq: 3, role: "assistant", content: "Agent đề xuất qua MCP: field \"website\".", createdAt: "2026-10-17T03:05:00Z",
      draft: [{ kind: "field", field: { entity: "benh_nhan", name: "website", label: "Website", fieldType: "text" } }],
    },
  ],
};

/** The expiry of the agent key bound to the thread, as McpConnect passes it. */
const liveUntil = { at: "2999-01-01T00:00:00Z" };

function Opener() {
  const open = useOpenThread();
  const shown = useAssistantOpen();
  return (
    <>
      <button type="button" disabled={!open} onClick={() => void open?.("sess-1")}>mo</button>
      <button type="button" disabled={!open} onClick={() => void open?.("sess-1", liveUntil.at)}>mo-key</button>
      <span data-testid="open-state">{String(shown)}</span>
    </>
  );
}

function mount(path = "/studio/du-an/northwind/van-hanh/tu-dong/khoa-truy-cap") {
  const p = parseStudioPath(path)!;
  return render(
    <QueryClientProvider client={new QueryClient()}>
      <AssistantProvider slug="northwind" projectName="Xưởng Nexus" route={p.route} params={p.params} panel={<AssistantMount />}>
        <Opener />
      </AssistantProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  pub.q = { data: { delivered: false, draftableTypes: [] } };
  vi.mocked(addField).mockClear();
  vi.mocked(settleAiProposal).mockClear();
  vi.mocked(getAiSession).mockReset();
  vi.mocked(getAiSession).mockResolvedValue(THREAD as never);
  window.localStorage.clear();
  resetAssistantOpenForTests();
  // narrow screen: the panel is an overlay, closed on load, so "it opens" is something the test can see change
  window.matchMedia = vi.fn().mockImplementation((q: string) => ({ matches: false, media: q, addEventListener: () => {}, removeEventListener: () => {} })) as unknown as typeof window.matchMedia;
});

describe("opening a saved conversation", () => {
  it("loads the thread into the panel with its cards and opens it, even with the assistant off", async () => {
    mount();
    expect(screen.getByTestId("open-state").textContent).toBe("false");
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "mo" })); });
    expect(getAiSession).toHaveBeenCalledWith("sess-1");
    await screen.findByText("[MCP] Agent gọi nexus_propose_field");
    expect(screen.getByText(/Agent đề xuất qua MCP/)).toBeInTheDocument();
    // the proposal is a card a human applies, as in a live turn
    await waitFor(() => expect(screen.getByRole("button", { name: "Áp dụng" })).toBeInTheDocument());
    expect(screen.getByTestId("open-state").textContent).toBe("true");
  });

  // Issue #42: a card of a saved thread that is applied or discarded is told to the server (turn seq 3, card 0), so the
  // thread's MCP agent stops replaying it into its next proposal.
  it("tells the thread when a card is applied", async () => {
    mount();
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "mo" })); });
    fireEvent.click(await screen.findByRole("button", { name: "Áp dụng" }));
    await waitFor(() => expect(addField).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(settleAiProposal).toHaveBeenCalledWith("sess-1", 3, 0));
    expect(settleAiProposal).toHaveBeenCalledTimes(1);
  });

  it("tells the thread when a card is discarded, and writes nothing else", async () => {
    mount();
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "mo" })); });
    fireEvent.click(await screen.findByRole("button", { name: "Bỏ" }));
    await waitFor(() => expect(settleAiProposal).toHaveBeenCalledWith("sess-1", 3, 0));
    expect(addField).not.toHaveBeenCalled();
  });

  it("tells the thread again when an applied card is undone (Hoàn tác)", async () => {
    // A delivered project: the rule card lands in the draft and offers Hoàn tác, which discards the draft row it wrote.
    pub.q = { data: { delivered: true, headVersion: { versionNo: 3 }, pendingCount: 0, changesetHash: "h", draftableTypes: ["workflow", "field"], directTypes: ["role"], propagationSeconds: 5 } };
    const rule = { kind: "workflow", workflow: { name: "Nhắc hẹn", entityName: "lich_hen", trigger: "after_update", actions: [] },
      change: { docType: "workflow", op: "update", label: "Nhắc hẹn", target: { entity: "lich_hen" } } };
    vi.mocked(getAiSession).mockResolvedValue({ ...THREAD, messages: [...THREAD.messages.slice(0, 3), { ...THREAD.messages[3], draft: [rule] }] } as never);
    const row = { id: "d1", docType: "workflow", docKey: "wf-1", label: "Nhắc hẹn", op: "update", entityName: "lich_hen",
      updatedAt: "2026-10-21T00:00:00Z", updatedBy: "u1", conflict: false, changedPaths: [], moreChanged: 0 };
    vi.mocked(listDraftChanges).mockReset();
    vi.mocked(listDraftChanges).mockResolvedValueOnce({ changes: [], changesetHash: "a" }).mockResolvedValue({ changes: [row], changesetHash: "b" } as never);
    vi.mocked(discardDraft).mockClear();
    mount();
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "mo" })); });
    fireEvent.click(await screen.findByRole("button", { name: "Áp dụng vào bản nháp" }));
    await waitFor(() => expect(createWorkflow).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(settleAiProposal).toHaveBeenCalledTimes(1));
    fireEvent.click(await screen.findByRole("button", { name: "Hoàn tác" }));
    fireEvent.click(await screen.findByRole("button", { name: "Bỏ khỏi bản nháp" }));
    await waitFor(() => expect(discardDraft).toHaveBeenCalledWith("d1"));
    await waitFor(() => expect(settleAiProposal).toHaveBeenCalledTimes(2));
    expect(vi.mocked(settleAiProposal).mock.calls).toEqual([["sess-1", 3, 0], ["sess-1", 3, 0]]);
  });

  it("is not offered where the panel cannot mount", () => {
    mount("/studio/du-an/northwind/cai-dat");
    expect(screen.getByRole("button", { name: "mo" })).toBeDisabled();
  });
});

// Issue #22: while the agent key bound to the thread is live, the open thread shows what the agent adds without a reopen:
// it re-reads itself every 5 s while visible, stops after 10 minutes with nothing new, and a focus wakes it again.
describe("an open thread an agent key is bound to", () => {
  const LATER = {
    ...THREAD,
    messages: [
      ...THREAD.messages,
      { seq: 4, role: "user", content: "[MCP] Agent gọi nexus_propose_entity", draft: null, createdAt: "2026-10-17T03:06:00Z" },
      { seq: 5, role: "assistant", content: "Agent đề xuất qua MCP: entity \"kho\".", createdAt: "2026-10-17T03:06:00Z",
        draft: [{ kind: "entity", entity: { name: "kho", label: "Kho", fields: [] } }] },
    ],
  };
  const tick = (ms: number) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });
  const openWith = async (name: string) => { await act(async () => { fireEvent.click(screen.getByRole("button", { name })); }); };

  beforeEach(() => { vi.useFakeTimers(); liveUntil.at = "2999-01-01T00:00:00Z"; });
  afterEach(() => { vi.useRealTimers(); });

  it("shows the agent's new card without a reopen, and keeps what was applied", async () => {
    mount();
    await openWith("mo-key");
    fireEvent.click(screen.getByRole("button", { name: "Áp dụng" }));
    await tick(0);
    expect(addField).toHaveBeenCalledTimes(1);
    vi.mocked(getAiSession).mockResolvedValue(LATER as never);
    await tick(5_000);
    expect(screen.getByText("[MCP] Agent gọi nexus_propose_entity")).toBeInTheDocument();
    // the field card applied before the re-read is still applied: one Áp dụng left, the new table's
    expect(screen.getAllByRole("button", { name: "Áp dụng" })).toHaveLength(1);
  });

  it("stops reading after ten quiet minutes; a focus reads again", async () => {
    mount();
    await openWith("mo-key");
    await tick(10 * 60_000 + 5_000);
    const reads = vi.mocked(getAiSession).mock.calls.length;
    await tick(60_000);
    expect(getAiSession).toHaveBeenCalledTimes(reads);
    vi.mocked(getAiSession).mockResolvedValue(LATER as never);
    await act(async () => { window.dispatchEvent(new Event("focus")); });
    await tick(0);
    expect(screen.getByText("[MCP] Agent gọi nexus_propose_entity")).toBeInTheDocument();
  });

  it("is not followed without a live key: no key, or an expired one", async () => {
    mount();
    await openWith("mo");
    await tick(60_000);
    expect(getAiSession).toHaveBeenCalledTimes(1);
    liveUntil.at = "2000-01-01T00:00:00Z";
    await openWith("mo-key");
    await tick(60_000);
    expect(getAiSession).toHaveBeenCalledTimes(2);
  });
});
