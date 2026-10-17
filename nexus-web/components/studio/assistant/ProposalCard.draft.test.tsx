// specs/076 ai9 (T172, T172a): the card's draft behaviour — label, in-place question, Hoàn tác, sealed, "Mở bản nháp".
import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { ProposalCard, type CardDraftProps, type ProposalState } from "./ProposalCard";
import { cardMode, type Delivery } from "@/lib/aiApply";
import type { AiDraftItem } from "@/lib/aiBuilder";

const RULE: AiDraftItem = {
  kind: "workflow",
  workflow: { name: "Nhắc hẹn", entityName: "lich_hen", trigger: "after_update", actions: [] },
  change: { docType: "workflow", op: "update", label: "Nhắc hẹn", target: { entity: "lich_hen" } },
};
const DELIVERED: Delivery = {
  kind: "delivered",
  state: { delivered: true, headVersion: null, pendingCount: 0, changesetHash: "h", draftableTypes: ["workflow"], directTypes: [], propagationSeconds: 5 },
};

function setup(state: ProposalState | undefined, over: Partial<CardDraftProps> = {}, delivery: Delivery = DELIVERED) {
  const onApply = vi.fn();
  const draft: CardDraftProps = {
    mode: cardMode(RULE, delivery),
    onRecheck: vi.fn(),
    onUndoPlan: vi.fn().mockResolvedValue({ labels: ["Nhắc hẹn"] }),
    onUndo: vi.fn().mockResolvedValue("undone"),
    canOpenDrafts: true,
    onOpenDrafts: vi.fn(),
    ...over,
  };
  render(<ProposalCard item={RULE} state={state} asSample onAsSampleChange={() => {}} onApply={onApply} onDiscard={() => {}} link={(id) => `/studio/${id}`} draft={draft} />);
  return { draft, onApply };
}
const DONE: ProposalState = { status: "done", changeIds: ["r1"], headVersionNo: 0, foot: "page.studio.ai.foot.draft" };

describe("ProposalCard — label and question", () => {
  it("delivered draft class reads 'Áp dụng vào bản nháp' and applies at once", () => {
    const { onApply } = setup(undefined);
    fireEvent.click(screen.getByRole("button", { name: "Áp dụng vào bản nháp" }));
    expect(onApply).toHaveBeenCalledTimes(1);
  });
  it("a live class asks in place first; Huỷ does not apply, the safe button gets focus first", () => {
    const { onApply } = setup(undefined, {}, { kind: "unknown" });
    fireEvent.click(screen.getByRole("button", { name: "Áp ngay" }));
    expect(screen.getByText(/^Áp ngay cho người dùng thật: /)).toBeInTheDocument();
    expect(onApply).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Huỷ" }));
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(onApply).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Áp ngay" }));
  });
  it("loading: Áp dụng disabled with 'Đang kiểm tra bản nháp…'", () => {
    setup(undefined, {}, { kind: "loading" });
    expect(screen.getByRole("button", { name: "Áp dụng" })).toBeDisabled();
    expect(screen.getByText("Đang kiểm tra bản nháp…")).toBeInTheDocument();
  });
  it("unknown: the sentence plus 'Kiểm tra lại' that re-reads", () => {
    const { draft } = setup(undefined, {}, { kind: "unknown" });
    expect(screen.getByText(/^Chưa xác định được dự án đã giao hay chưa/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Kiểm tra lại" }));
    expect(draft.onRecheck).toHaveBeenCalledTimes(1);
  });
});

describe("ProposalCard — Hoàn tác", () => {
  it("shows the link only with stored changeIds; asking names the rows and defaults to 'Giữ lại' with 0 deletes", async () => {
    const { draft } = setup(DONE);
    fireEvent.click(screen.getByRole("button", { name: "Hoàn tác" }));
    expect(await screen.findByText("Bỏ khỏi bản nháp: Nhắc hẹn?")).toBeInTheDocument();
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Giữ lại" })));
    expect(draft.onUndo).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Giữ lại" }));
    expect(draft.onUndo).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Hoàn tác" })).toBeInTheDocument();
  });
  it("confirming runs the undo once", async () => {
    const { draft } = setup(DONE);
    fireEvent.click(screen.getByRole("button", { name: "Hoàn tác" }));
    fireEvent.click(await screen.findByRole("button", { name: "Bỏ khỏi bản nháp" }));
    await waitFor(() => expect(draft.onUndo).toHaveBeenCalledTimes(1));
  });
  it("no changeIds ⇒ no Hoàn tác", () => {
    setup({ status: "done", foot: "page.studio.ai.foot.draft" });
    expect(screen.queryByRole("button", { name: "Hoàn tác" })).toBeNull();
  });
  it("stale at ask time: the sentence + 'Mở bản nháp…', no confirm, no delete", async () => {
    const { draft } = setup(DONE, { onUndoPlan: vi.fn().mockResolvedValue("stale") });
    fireEvent.click(screen.getByRole("button", { name: "Hoàn tác" }));
    expect(await screen.findByText("Thay đổi này đã được sửa tiếp trong bản nháp; mở bản nháp để xem.")).toBeInTheDocument();
    expect(draft.onUndo).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Mở bản nháp để bỏ thay đổi này" }));
    expect(draft.onOpenDrafts).toHaveBeenCalledTimes(1);
  });
  it("a failed delete keeps the card and says so", async () => {
    setup(DONE, { onUndo: vi.fn().mockResolvedValue("error") });
    fireEvent.click(screen.getByRole("button", { name: "Hoàn tác" }));
    fireEvent.click(await screen.findByRole("button", { name: "Bỏ khỏi bản nháp" }));
    expect(await screen.findByText(/^Chưa bỏ được khỏi bản nháp/)).toBeInTheDocument();
    expect(screen.getByText("Đã áp dụng")).toBeInTheDocument();
    // a transient failure must leave a way to try again
    expect(screen.getByRole("button", { name: "Hoàn tác" })).toBeInTheDocument();
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Hoàn tác" })));
  });
  it("'Giữ lại' hands focus back to the Hoàn tác button", async () => {
    setup(DONE);
    fireEvent.click(screen.getByRole("button", { name: "Hoàn tác" }));
    fireEvent.click(await screen.findByRole("button", { name: "Giữ lại" }));
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Hoàn tác" })));
  });
});

describe("ProposalCard — builder already had a draft (FR-082, FR-127)", () => {
  const SEEN: ProposalState = { status: "done", noUndo: "seen", foot: "page.studio.ai.foot.draft" };
  it("no Hoàn tác; 'Mở bản nháp…' is a real button when the review can open", () => {
    const { draft } = setup(SEEN);
    expect(screen.queryByRole("button", { name: "Hoàn tác" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Mở bản nháp để bỏ thay đổi này" }));
    expect(draft.onOpenDrafts).toHaveBeenCalledTimes(1);
  });
  it("when nothing can open it, no promise: only the plain sentence", () => {
    setup(SEEN, { canOpenDrafts: false });
    expect(screen.queryByRole("button", { name: /Mở bản nháp/ })).toBeNull();
    expect(screen.getByText("Thay đổi này đã nằm trong bản nháp cùng phần anh đã sửa trước đó.")).toBeInTheDocument();
  });
});

describe("ProposalCard — sealed and undone", () => {
  it("sealed: 'Đã xuất bản trong phiên bản N', no Hoàn tác", () => {
    setup({ ...DONE, status: "sealed", sealedVersion: 4 });
    expect(screen.getByText("Đã xuất bản trong phiên bản 4")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Hoàn tác" })).toBeNull();
  });
  it("undone: lead says 'Đã hoàn tác' and the change list is gone", () => {
    setup({ status: "undone" });
    expect(screen.getByText("Đã hoàn tác")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Hoàn tác" })).toBeNull();
  });
});

// Issue #2, verifier round 1 (LOW): the question asked before an access card applies says what really happens — the
// access applies at once to real users, and (on a published project) the new table stays hidden from them until Publish.
describe("ProposalCard — access card question", () => {
  const ACCESS: AiDraftItem = {
    kind: "access",
    access: { role: "crm_sales_rep", roleLabel: "Nhân viên kinh doanh", labels: { tickets: "Phiếu hỗ trợ" },
      grants: [{ entityName: "tickets", action: "view", accessLevel: "all" }] },
    change: { docType: "role", docKey: "r1", op: "update", label: "Vai", target: { name: "crm_sales_rep", entity: "tickets" },
      before: { grants: [], labels: { tickets: "Phiếu hỗ trợ" } }, after: { grants: [], labels: { tickets: "Phiếu hỗ trợ" } } },
  };
  const ask = (delivery: Delivery, delivered: boolean, tableHidden?: boolean) => {
    const onApply = vi.fn();
    render(<ProposalCard item={ACCESS} asSample onAsSampleChange={() => {}} onApply={onApply} onDiscard={() => {}} link={(id) => `/studio/${id}`}
      delivered={delivered} tableHidden={tableHidden}
      draft={{ mode: cardMode(ACCESS, delivery), onRecheck: vi.fn(), onUndoPlan: vi.fn(), onUndo: vi.fn(), canOpenDrafts: false, onOpenDrafts: vi.fn() }} />);
    fireEvent.click(screen.getByRole("button", { name: "Áp ngay" }));
    return onApply;
  };
  const delivered: Delivery = { kind: "delivered", state: { ...(DELIVERED as Extract<Delivery, { kind: "delivered" }>).state, directTypes: ["role"] } };

  it("its table is still hidden from customers: applies now, the table stays hidden until Xuất bản", () => {
    const onApply = ask(delivered, true, true);
    expect(screen.getByText("Quyền này áp ngay cho người dùng thật: Nhân viên kinh doanh được xem tất cả trên Phiếu hỗ trợ. Bảng vẫn ẩn với khách cho tới khi anh Xuất bản.")).toBeInTheDocument();
    expect(onApply).not.toHaveBeenCalled();
    fireEvent.click(screen.getAllByRole("button", { name: "Áp ngay" }).at(-1)!);
    expect(onApply).toHaveBeenCalledTimes(1);
  });

  // Verifier round 2: the question followed the project's delivery, so a card applied after its table was published
  // still said "Bảng vẫn ẩn…" while the grant went live on a table the customer has.
  it("delivered, but its table is published already: no 'stays hidden'", () => {
    ask(delivered, true, false);
    expect(screen.getByText("Quyền này áp ngay cho người dùng thật: Nhân viên kinh doanh được xem tất cả trên Phiếu hỗ trợ.")).toBeInTheDocument();
    expect(screen.queryByText(/Bảng vẫn ẩn/)).toBeNull();
  });

  it("never published: asked too, without a publish to wait for", () => {
    ask({ kind: "undelivered" }, false);
    expect(screen.getByText("Quyền này áp ngay cho người dùng thật: Nhân viên kinh doanh được xem tất cả trên Phiếu hỗ trợ.")).toBeInTheDocument();
  });
});
