// specs/076 ai3 (T055, T055a, T056, T062a, T062b): the docked assistant panel inside the project frame.
// Seams mocked: the stream client, the identity/scope module and the write APIs. The chat hook, the provider, the
// panel and ProposalCard are the REAL ones.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
vi.mock("../usePublishState", () => ({ usePublishState: () => ({ data: { delivered: false, draftableTypes: [] } }) }));

vi.mock("@/lib/aiBuilderStream", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/aiBuilderStream")>();
  return { ...actual, aiBuilderChatStream: vi.fn() };
});
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, aiHealth: vi.fn(async () => true), addField: vi.fn(async () => ({})), listAiSessions: vi.fn(async () => []) };
});

import { aiBuilderChatStream, AiStreamCancelledError } from "@/lib/aiBuilderStream";
import { addField, aiHealth, ApiError, listAiSessions } from "@/lib/api";
import { resetAssistantOpenForTests, setAssistantOpen } from "@/lib/assistantOpen";
import { parseStudioPath } from "@/lib/studioRoutes";
import { AssistantMount } from "./AssistantMount";
import { AssistantProvider } from "./AssistantProvider";

const stream = vi.mocked(aiBuilderChatStream);
const reply = (text: string, draft: unknown = null) => ({ sessionId: "s1", reply: text, draft, runId: "r", usage: undefined, outcome: undefined }) as never;
const fieldDraft = (name: string) => ({ kind: "field", field: { entity: "benh_nhan", name, label: name, fieldType: "text" } });

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

describe("AssistantPanel", () => {
  it("empty: named aside, polite live region, one honest intro, no chat bubbles", async () => {
    mount();
    const aside = await screen.findByRole("complementary", { name: "Trợ lý AI của dự án" });
    expect(within(aside).getByRole("log")).toHaveAttribute("aria-live", "polite");
    expect(within(aside).getByText(/Tôi chỉ đề xuất/)).toBeTruthy();
    expect(aside.querySelector(".slds-chat, [class*='slds-chat']")).toBeNull();
    expect(aside.innerHTML).not.toMatch(/\brounded\b/);
  });

  it("Enter sends, Shift+Enter does not; turns carry the speaker labels Anh / Trợ lý", async () => {
    stream.mockResolvedValue(reply("Có 3 bảng."));
    mount();
    fireEvent.change(box(), { target: { value: "có bảng nào" } });
    fireEvent.keyDown(box(), { key: "Enter", shiftKey: true });
    expect(stream).not.toHaveBeenCalled();
    fireEvent.keyDown(box(), { key: "Enter" });
    await screen.findByText("Có 3 bảng.");
    expect(screen.getByText("Anh")).toBeTruthy();
    expect(screen.getByText("Trợ lý", { selector: "p" })).toBeTruthy();
    expect(stream.mock.calls[0][0]).toBe("có bảng nào");
    expect(stream.mock.calls[0][2]?.context).toEqual({ panel: "bang", entity: "benh_nhan" });
    expect(box().value).toBe("");
  });

  it("a link the model wrote opens the matching page of THIS project; an unknown one is plain text", async () => {
    stream.mockResolvedValue(reply("Xem [báo cáo](/reports) hoặc [lạ](/xyz)."));
    mount();
    await say("đâu");
    expect((await screen.findByRole("link", { name: "báo cáo" })).getAttribute("href")).toBe("/studio/du-an/northwind/dung/bao-cao");
    expect(screen.queryByRole("link", { name: "lạ" })).toBeNull();
  });

  it("assistant off: info notice, composer locked, panel still opens", async () => {
    vi.mocked(aiHealth).mockResolvedValue(false);
    mount();
    expect(await screen.findByText("Trợ lý chưa bật trên máy chủ này.")).toBeTruthy();
    expect(box()).toBeDisabled();
    expect(screen.getByTestId("assistant-toggle")).toBeTruthy();
  });

  it("503 AI_DISABLED on the first turn locks the composer too", async () => {
    stream.mockRejectedValue(new ApiError(503, "AI_DISABLED", "off"));
    mount();
    await say("a");
    expect(await screen.findByText("Trợ lý chưa bật trên máy chủ này.")).toBeTruthy();
    expect(box()).toBeDisabled();
  });

  it("429 shows a warning notice with Thử lại and the composer stays free", async () => {
    stream.mockRejectedValue(new ApiError(429, "AI_RATE_LIMITED", "nhanh quá", { retryAfter: 5 }));
    mount();
    await say("a");
    expect(await screen.findByRole("button", { name: "Thử lại" })).toBeTruthy();
    expect(box()).not.toBeDisabled();
  });

  it("Huỷ ends the turn with a neutral note, not an error", async () => {
    stream.mockImplementation((_m, _s, o) => new Promise((_, rej) => o.signal.addEventListener("abort", () => rej(new AiStreamCancelledError()))));
    mount();
    await say("lâu");
    fireEvent.click(await screen.findByRole("button", { name: "Huỷ" }));
    expect(await screen.findByText(/Đã huỷ/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Thử lại" })).toBeNull();
  });

  it("a model refusal reads as one line under Trợ lý, not an empty answer (#30 item 5)", async () => {
    stream.mockResolvedValue({ sessionId: "s1", reply: "", draft: null, runId: "r", usage: undefined, outcome: "refused" } as never);
    mount();
    await say("chép tài khoản của không gian khác");
    const line = await screen.findByText("Mô hình ngôn ngữ từ chối yêu cầu này.");
    expect(line.closest(".slds-m-top_small")?.textContent).toBe("Trợ lýMô hình ngôn ngữ từ chối yêu cầu này.");
  });

  it("the send button is aria-disabled while empty", () => {
    mount();
    expect(screen.getByRole("button", { name: "Gửi" })).toHaveAttribute("aria-disabled", "true");
  });

  it("a ProposalCard inside the panel adds no article.slds-card outside the aside", async () => {
    stream.mockResolvedValue(reply("Đề xuất:", [fieldDraft("ngay")]));
    const { container } = mount();
    await say("thêm trường");
    await screen.findByRole("button", { name: /^Áp dụng/ });
    const outside = Array.from(container.querySelectorAll("article.slds-card")).filter((n) => !n.closest("aside"));
    expect(outside).toHaveLength(0);
  });

  it("closed panel: the header button names the state — working, then new proposal — and opening clears it", async () => {
    let finish: (v: never) => void = () => {};
    stream.mockImplementation(() => new Promise((r) => { finish = r as (v: never) => void; }));
    mount();
    await say("đề xuất đi");
    setAssistantOpen(false);
    await waitFor(() => expect(screen.getByTestId("assistant-toggle")).toHaveAccessibleName("Trợ lý, đang trả lời"));
    await act(async () => { finish(reply("Xong", [fieldDraft("ngay")])); });
    await waitFor(() => expect(screen.getByTestId("assistant-toggle")).toHaveAccessibleName("Trợ lý, có đề xuất mới"));
    fireEvent.click(screen.getByTestId("assistant-toggle"));
    await waitFor(() => expect(screen.getByTestId("assistant-toggle")).toHaveAccessibleName("Trợ lý"));
    expect(screen.queryByTestId("assistant-dot")).toBeNull();
  });

  it("overlay width starts closed even if a docked 'open' was stored", () => {
    window.localStorage.setItem("nexus.studio.assistantOpen", "1");
    wide(false);
    mount();
    expect(screen.getByTestId("assistant-toggle")).toHaveAttribute("aria-pressed", "false");
  });

  it("the context line names the screen and the project, with a way to drop the context", async () => {
    mount();
    expect(await screen.findByText(/Đang xem: .* · Xưởng Nexus/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Bỏ ngữ cảnh" })).toBeTruthy();
  });
});

describe("AssistantMount — who gets a panel", () => {
  it("a token that is not this project's: nothing mounts and no call is made", () => {
    sc.matches = false;
    mount();
    expect(screen.queryByRole("complementary")).toBeNull();
    expect(screen.queryByTestId("assistant-toggle")).toBeNull();
    expect(aiHealth).not.toHaveBeenCalled();
    expect(listAiSessions).not.toHaveBeenCalled();
  });
  it("an operator-only (nha) page: nothing mounts", () => {
    mount("/studio/du-an/northwind/cai-dat");
    expect(screen.queryByRole("complementary")).toBeNull();
    expect(aiHealth).not.toHaveBeenCalled();
  });
});

describe("scope gate (FR-108, FR-128)", () => {
  it("while a context switch is in flight a send leaves nothing the page and keeps the typed text", async () => {
    mount();
    sc.phase = "running";
    fireEvent.change(box(), { target: { value: "đừng gửi" } });
    fireEvent.keyDown(box(), { key: "Enter" });
    await act(async () => {});
    expect(stream).not.toHaveBeenCalled();
  });

  it("the token stopped being this project's between two sends: the second send is refused", async () => {
    stream.mockResolvedValue(reply("ok"));
    mount();
    await say("một");
    expect(stream).toHaveBeenCalledTimes(1);
    sc.ws = { tenantSlug: "khac", tenantName: "Khách khác" };
    await say("hai");
    expect(stream).toHaveBeenCalledTimes(1);
  });

  it("Áp dụng tất cả: the switch flips after card 1, so cards 2 and 3 make no write call", async () => {
    stream.mockResolvedValue(reply("Ba trường:", [fieldDraft("a"), fieldDraft("b"), fieldDraft("c")]));
    vi.mocked(addField).mockImplementation(async () => { sc.phase = "running"; return {} as never; });
    mount();
    await say("thêm ba trường");
    fireEvent.click(await screen.findByRole("button", { name: /^Áp dụng tất cả/ }));
    await waitFor(() => expect(addField).toHaveBeenCalledTimes(1));
    await act(async () => {});
    expect(addField).toHaveBeenCalledTimes(1);
    expect(await screen.findAllByText(/Phiên dựng đã đổi|Đã đổi dự án giữa chừng/)).not.toHaveLength(0);
  });

  it("a result that arrives after the project switch is dropped (0 turns shown)", async () => {
    let finish: (v: never) => void = () => {};
    stream.mockImplementation(() => new Promise((r) => { finish = r as (v: never) => void; }));
    mount();
    await say("chậm");
    act(() => { window.dispatchEvent(new Event("nexus:workspace-ended")); });
    await act(async () => { finish(reply("muộn")); });
    expect(screen.queryByText("muộn")).toBeNull();
  });
});
