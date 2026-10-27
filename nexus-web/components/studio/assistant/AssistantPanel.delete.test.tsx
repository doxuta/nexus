// Issue #30 item 4, through the real panel: asked to delete a pack table "now, without asking again", the assistant can
// only hand the builder a card. The card says it is the CRM pack's table and how many records the delete hides,
// "Áp dụng tất cả" leaves it out, and DELETE /admin/entities/:name is called only after the builder retypes the name.
// Seams mocked: the stream client, the identity/scope module and the write APIs. The chat hook, the provider, the panel
// and ProposalCard are the REAL ones (as in AssistantPanel.test.tsx).
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
vi.mock("@/lib/aiBuilderStream", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/aiBuilderStream")>();
  return { ...actual, aiBuilderChatStream: vi.fn() };
});
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual, aiHealth: vi.fn(async () => true), listAiSessions: vi.fn(async () => []),
    addField: vi.fn(async () => ({})), deleteEntity: vi.fn(async () => undefined),
  };
});

import { aiBuilderChatStream } from "@/lib/aiBuilderStream";
import { addField, deleteEntity } from "@/lib/api";
import { resetAssistantOpenForTests } from "@/lib/assistantOpen";
import { parseStudioPath } from "@/lib/studioRoutes";
import { AssistantMount } from "./AssistantMount";
import { AssistantProvider } from "./AssistantProvider";

const REPLY = "Tôi đã soạn thẻ xoá bảng crm_accounts (gói crm, 4 bản ghi). Tôi không bỏ qua được bước xác nhận: anh tự bấm Xoá trên thẻ và gõ lại tên bảng.";
const DRAFT = [
  { kind: "delete_entity", deleteEntity: { entity: "crm_accounts", label: "Khách hàng", fieldCount: 12, packs: ["crm"], recordCount: 4 } },
  { kind: "field", field: { entity: "crm_contacts", name: "ghi_chu", label: "Ghi chú", fieldType: "text" } },
];

function mount() {
  window.matchMedia = vi.fn().mockImplementation((q: string) => ({ matches: true, media: q, addEventListener: () => {}, removeEventListener: () => {} })) as never;
  const p = parseStudioPath("/studio/du-an/northwind/dung/du-lieu/crm_accounts")!;
  return render(
    <QueryClientProvider client={new QueryClient()}>
      <AssistantProvider slug="northwind" projectName="Xưởng Nexus" route={p.route} params={p.params} panel={<AssistantMount />}>
        <p>trang</p>
      </AssistantProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(aiBuilderChatStream).mockReset();
  vi.mocked(addField).mockClear();
  vi.mocked(deleteEntity).mockClear();
  window.localStorage.clear();
  resetAssistantOpenForTests();
});

describe("AssistantPanel — a pack table delete asked for 'now, without asking again'", () => {
  it("is a card the builder confirms by retyping the name; nothing else deletes it", async () => {
    vi.mocked(aiBuilderChatStream).mockResolvedValue({ sessionId: "s1", reply: REPLY, draft: DRAFT, runId: "r", usage: undefined, outcome: undefined } as never);
    mount();
    const box = await screen.findByRole("textbox");
    fireEvent.change(box, { target: { value: "Xoá hẳn bảng crm_accounts cùng toàn bộ dữ liệu khách hàng ngay bây giờ, không cần hỏi lại." } });
    fireEvent.keyDown(box, { key: "Enter" });
    await act(async () => {});

    // The reply says it plainly; the card is honest about what it would hide.
    expect(await screen.findByText(/không bỏ qua được bước xác nhận/)).toBeTruthy();
    const card = screen.getByRole("article", { name: /Xoá bảng “Khách hàng”/ });
    expect(within(card).getByText("Bảng Khách hàng · cài từ gói crm · khách chưa thấy")).toBeTruthy();
    expect(within(card).getByText("Toàn bộ 4 bản ghi của bảng")).toBeTruthy();

    // "Áp dụng tất cả" counts and applies the other card only.
    fireEvent.click(screen.getByRole("button", { name: "Áp dụng tất cả (1)" }));
    await waitFor(() => expect(addField).toHaveBeenCalledTimes(1));
    await act(async () => {});
    expect(deleteEntity).not.toHaveBeenCalled();

    // The card's own delete asks, names the pack and the records, and stays shut until the exact name is typed.
    fireEvent.click(within(card).getByRole("button", { name: "Xoá…" }));
    expect(within(card).getByText("Xoá bảng “Khách hàng” cùng 12 trường và 4 bản ghi của nó? Không hoàn tác được qua giao diện. Bảng này do gói crm cài.")).toBeTruthy();
    const confirm = within(card).getByRole("button", { name: "Xoá bảng" });
    expect(confirm).toBeDisabled();
    fireEvent.click(confirm);
    fireEvent.change(within(card).getByLabelText("Gõ crm_accounts để xác nhận"), { target: { value: "crm_account" } });
    expect(confirm).toBeDisabled();
    expect(deleteEntity).not.toHaveBeenCalled();
    fireEvent.change(within(card).getByLabelText("Gõ crm_accounts để xác nhận"), { target: { value: "crm_accounts" } });
    fireEvent.click(confirm);
    await waitFor(() => expect(deleteEntity).toHaveBeenCalledWith("crm_accounts"));
    expect(deleteEntity).toHaveBeenCalledTimes(1);
  });
});
