// specs/076 ai9 (#815): the DOCKED assistant carries the same delivery-aware card as the old AI builder page (label, Áp ngay, fail-closed, apply-all).
// Seams mocked: the stream client, the identity/scope module and the write APIs. The chat hook, the provider, the
// panel and ProposalCard are the REAL ones.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
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
  return { ...actual, aiHealth: vi.fn(async () => true), addField: vi.fn(async () => ({})), listAiSessions: vi.fn(async () => []), listEntities: vi.fn(async () => []) };
});

import { aiBuilderChatStream } from "@/lib/aiBuilderStream";
import { addField, aiHealth, ApiError, listAiSessions, listEntities } from "@/lib/api";
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

// Verifier round 2: the access card's question says "the table stays hidden until Xuất bản" only while its table is
// still hidden from the customer (the Studio's table list: draftState "new"), not whenever the project is delivered.
describe("AssistantPanel — the access card's question follows its table", () => {
  const ACCESS = {
    kind: "access",
    access: { role: "crm_sales_rep", roleLabel: "Nhân viên kinh doanh", labels: { tickets: "Phiếu hỗ trợ" },
      grants: [{ entityName: "tickets", action: "view", accessLevel: "all" }] },
    change: { docType: "role", docKey: "r1", op: "update", label: "Vai", target: { name: "crm_sales_rep", entity: "tickets" },
      before: { grants: [], labels: { tickets: "Phiếu hỗ trợ" } }, after: { grants: [], labels: { tickets: "Phiếu hỗ trợ" } } },
  };
  async function ask(tickets: Record<string, unknown>) {
    pub.q = { data: STATE };
    vi.mocked(listEntities).mockResolvedValue([{ name: "tickets", label: "Phiếu hỗ trợ", tableName: "obj_tickets", ...tickets }]);
    stream.mockResolvedValue(reply("Cấp quyền.", [ACCESS]));
    mount();
    await say("nhân viên kinh doanh dùng bảng này");
    await waitFor(() => expect(listEntities).toHaveBeenCalled());
    await act(async () => {});
    fireEvent.click(await screen.findByRole("button", { name: "Áp ngay" }));
  }

  it("its table is still new: the question says it stays hidden until Xuất bản", async () => {
    await ask({ draftState: "new" });
    expect(await screen.findByText("Quyền này áp ngay cho người dùng thật: Nhân viên kinh doanh được xem tất cả trên Phiếu hỗ trợ. Bảng vẫn ẩn với khách cho tới khi anh Xuất bản.")).toBeTruthy();
  });

  it("its table is published already: the grant goes live, the question says nothing of a wait", async () => {
    await ask({});
    expect(await screen.findByText("Quyền này áp ngay cho người dùng thật: Nhân viên kinh doanh được xem tất cả trên Phiếu hỗ trợ.")).toBeTruthy();
    expect(screen.queryByText(/Bảng vẫn ẩn/)).toBeNull();
  });
});
