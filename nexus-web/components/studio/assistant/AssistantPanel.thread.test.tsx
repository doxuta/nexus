// Issue #3: a saved conversation (the thread an MCP agent writes its proposals to) opens in the docked panel as a
// resumed session: its turns, its cards ready to apply, and the panel open. The provider, the panel and ProposalCard
// are the REAL ones; the session read and the identity seams are mocked.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("next/navigation", () => ({ useSearchParams: () => new URLSearchParams("") }));
vi.mock("@/lib/studioContext", () => ({
  useStudioIdentity: () => ({ status: "ready", me: undefined, home: "northwind", sessionReadable: true, matches: () => true, retry: vi.fn() }),
  transitionPhase: () => "idle",
  workspaceOf: () => null,
  homeSlug: () => "northwind",
  sessionReadable: () => true,
}));
vi.mock("@/lib/previewSession", () => ({ getPreviewToken: () => null }));
vi.mock("../usePublishState", () => ({ usePublishState: () => ({ data: { delivered: false, draftableTypes: [] } }) }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, aiHealth: vi.fn(async () => false), getAiSession: vi.fn() };
});

import { getAiSession } from "@/lib/api";
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

function Opener() {
  const open = useOpenThread();
  const shown = useAssistantOpen();
  return (
    <>
      <button type="button" disabled={!open} onClick={() => void open?.("sess-1")}>mo</button>
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

  it("is not offered where the panel cannot mount", () => {
    mount("/studio/du-an/northwind/cai-dat");
    expect(screen.getByRole("button", { name: "mo" })).toBeDisabled();
  });
});
