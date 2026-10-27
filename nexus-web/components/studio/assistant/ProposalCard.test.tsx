import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent, within } from "@testing-library/react";
import { ProposalCard, type ProposalState } from "./ProposalCard";
import type { AiDraftItem } from "@/lib/aiBuilder";

const link = (id: string) => `/studio/${id}`;
const FIELD: AiDraftItem = { kind: "field", field: { entity: "benh_nhan", name: "ngay", label: "Ngày tái khám", fieldType: "date" } };
const DEL_FIELD: AiDraftItem = { kind: "delete_field", deleteField: { entity: "benh_nhan", field: "fax", label: "Số fax", fieldType: "text" } };
const DEL_ENTITY: AiDraftItem = { kind: "delete_entity", deleteEntity: { entity: "khuyen_mai", label: "Khuyến mãi", fieldCount: 7 } };
const RECORDS: AiDraftItem = { kind: "records", records: { entity: "benh_nhan", rows: [{ ten: "An" }, { ten: "Bình" }] } };

function setup(item: AiDraftItem, state?: ProposalState) {
  const onApply = vi.fn();
  const onDiscard = vi.fn();
  const onAsSampleChange = vi.fn();
  const el = (s?: ProposalState) => (
    <ProposalCard item={item} state={s} asSample onAsSampleChange={onAsSampleChange} onApply={onApply} onDiscard={onDiscard} link={link} />
  );
  const utils = render(el(state));
  return { onApply, onDiscard, onAsSampleChange, setState: (s: ProposalState) => utils.rerender(el(s)), ...utils };
}

describe("ProposalCard — open", () => {
  it("shows lead line, title, where line, a + row and Áp dụng · Bỏ", () => {
    const { onApply, onDiscard } = setup(FIELD);
    const card = screen.getByRole("article");
    expect(within(card).getByText("Đề xuất")).toBeInTheDocument();
    expect(within(card).getByRole("heading", { name: "Thêm trường “Ngày tái khám”" })).toBeInTheDocument();
    expect(within(card).getByText("Bảng benh_nhan")).toBeInTheDocument();
    expect(within(card).getByText("Ngày tái khám (Ngày)")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Áp dụng" }));
    fireEvent.click(screen.getByRole("button", { name: "Bỏ" }));
    expect(onApply).toHaveBeenCalledTimes(1);
    expect(onDiscard).toHaveBeenCalledTimes(1);
  });

  it("Tab order is Áp dụng then Bỏ", () => {
    setup(FIELD);
    expect(screen.getAllByRole("button").map((b) => b.textContent)).toEqual(["Áp dụng", "Bỏ"]);
  });

  it("never uses Badge/Pill/chat markup", () => {
    const { container } = setup(FIELD);
    expect(container.querySelector(".slds-badge, .slds-pill, [class*='slds-chat']")).toBeNull();
  });

  it("with `change`: ONE purple label in the lead line, op word, Trước/Sau", () => {
    setup({
      ...FIELD,
      change: { docType: "field", op: "update", label: "x", changedPaths: ["label"], before: { label: "A" }, after: { label: "B" } },
    });
    expect(screen.getByText("Đề xuất · Sửa")).toBeInTheDocument();
    expect(screen.getByText("Trước")).toBeInTheDocument();
    expect(screen.getByText("Sau")).toBeInTheDocument();
    expect(document.querySelectorAll(".nx-status_proposed")).toHaveLength(1);
  });

  it("without `change`: no op word, no Trước/Sau", () => {
    setup(FIELD);
    expect(screen.queryByText("Trước")).toBeNull();
    expect(screen.getByText("Đề xuất", { selector: ".nx-status_proposed" })).toBeInTheDocument();
  });

  it("records: 'Là dữ liệu mẫu' is on, toggling reports the new value", () => {
    const { onAsSampleChange } = setup(RECORDS);
    const cb = screen.getByLabelText("Là dữ liệu mẫu");
    expect(cb).toBeChecked();
    fireEvent.click(cb);
    expect(onAsSampleChange).toHaveBeenCalledWith(false);
  });
});

describe("ProposalCard — states", () => {
  it("applying: Spinner in a disabled button", () => {
    setup(FIELD, { status: "applying" });
    const btn = screen.getByRole("button", { name: /Đang áp dụng/ });
    expect(btn).toBeDisabled();
    expect(btn.querySelector(".slds-spinner")).not.toBeNull();
  });

  it("done: green result row (role=status) with link, no Áp dụng button, ChangeList kept", () => {
    setup(FIELD, { status: "done" });
    const row = screen.getByRole("status");
    expect(row).toHaveTextContent("Đã tạo thành công.");
    expect(within(row).getByRole("link", { name: /Mở Dữ liệu/ })).toHaveAttribute("href", "/studio/dung/du-lieu");
    expect(screen.queryByRole("button", { name: "Áp dụng" })).toBeNull();
    expect(screen.getByText("Ngày tái khám (Ngày)")).toBeInTheDocument();
    expect(screen.getByText("Đã áp dụng", { selector: ".nx-status" })).toBeInTheDocument();
  });

  it("warn: brown text with the message and no re-apply", () => {
    setup(RECORDS, { status: "warn", message: "Dòng 2 lỗi." });
    expect(screen.getByRole("status")).toHaveTextContent("Dòng 2 lỗi.");
    expect(screen.getByText("Áp dụng một phần", { selector: ".nx-status_warn" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Áp dụng" })).toBeNull();
    expect(screen.getByRole("link", { name: /Mở danh sách/ })).toBeInTheDocument();
  });

  it("error: server sentence in role=alert and a 'Thử lại' button", () => {
    const { onApply } = setup(FIELD, { status: "error", message: "Trường đã tồn tại." });
    expect(screen.getByRole("alert")).toHaveTextContent("Trường đã tồn tại.");
    fireEvent.click(screen.getByRole("button", { name: "Thử lại" }));
    expect(onApply).toHaveBeenCalled();
  });

  it("discarded: collapsed — grey lead + title + where, no ChangeList, no buttons", () => {
    setup(FIELD, { status: "discarded" });
    expect(screen.getByText("Đã bỏ")).toBeInTheDocument();
    expect(screen.getByText("Thêm trường “Ngày tái khám”")).toBeInTheDocument();
    expect(screen.getByText("Bảng benh_nhan")).toBeInTheDocument();
    expect(screen.queryByRole("list")).toBeNull();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("the word 'niêm' never appears", () => {
    const { container } = setup(FIELD, { status: "done" });
    expect(container.textContent?.toLowerCase()).not.toContain("niêm");
  });

  it("unknown kind: dashed card, no Áp dụng; a failed apply-all still shows its error", () => {
    setup({ kind: "hologram" }, { status: "error", message: "Từ chối." });
    expect(screen.getByText(/chưa hiển thị được/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Áp dụng" })).toBeNull();
    expect(screen.getByRole("alert")).toHaveTextContent("Từ chối.");
  });
});

describe("ProposalCard — destructive (InlineConfirm)", () => {
  it("delete_field: 'Xoá…' opens the inline confirm, nothing is applied until the second click", () => {
    const { onApply } = setup(DEL_FIELD);
    expect(screen.getByText(/không nằm trong “Áp dụng tất cả”/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Xoá…" }));
    expect(onApply).not.toHaveBeenCalled();
    expect(screen.getByText(/Xoá trường “Số fax” khỏi bảng benh_nhan/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Xoá trường" }));
    expect(onApply).toHaveBeenCalledTimes(1);
  });

  it("cancel returns focus to the root button and leaves the card idle", () => {
    const { onApply } = setup(DEL_FIELD);
    fireEvent.click(screen.getByRole("button", { name: "Xoá…" }));
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(onApply).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Xoá…" })).toHaveFocus();
  });

  it("delete_entity: confirm stays off until the exact table name is retyped (labelled input)", () => {
    const { onApply } = setup(DEL_ENTITY);
    fireEvent.click(screen.getByRole("button", { name: "Xoá…" }));
    expect(screen.getByText(/cùng 7 trường và toàn bộ bản ghi/)).toBeInTheDocument();
    const confirm = screen.getByRole("button", { name: "Xoá bảng" });
    expect(confirm).toBeDisabled();
    const input = screen.getByLabelText("Gõ khuyen_mai để xác nhận");
    fireEvent.change(input, { target: { value: "khuyen" } });
    expect(confirm).toBeDisabled();
    fireEvent.change(input, { target: { value: "khuyen_mai" } });
    expect(confirm).toBeEnabled();
    fireEvent.click(confirm);
    expect(onApply).toHaveBeenCalledTimes(1);
  });

  it("delete_entity of a pack table: the question names the pack and the records, and the retype still gates it", () => {
    const { onApply } = setup({ kind: "delete_entity", deleteEntity: { entity: "crm_accounts", label: "Khách hàng", fieldCount: 12, packs: ["crm"], recordCount: 4 } });
    expect(screen.getByText("Bảng Khách hàng · có trong gói crm")).toBeInTheDocument();
    expect(screen.getByText("Toàn bộ bản ghi của bảng (lúc đề xuất: 4 bản ghi)")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Xoá…" }));
    expect(screen.getByText("Xoá bảng “Khách hàng” cùng 12 trường và toàn bộ bản ghi của nó (lúc đề xuất: 4 bản ghi)? Không hoàn tác được qua giao diện. Gói crm có bảng này.")).toBeInTheDocument();
    const confirm = screen.getByRole("button", { name: "Xoá bảng" });
    fireEvent.change(screen.getByLabelText("Gõ crm_accounts để xác nhận"), { target: { value: "Khách hàng" } });
    expect(confirm).toBeDisabled();
    fireEvent.click(confirm);
    expect(onApply).not.toHaveBeenCalled();
    fireEvent.change(screen.getByLabelText("Gõ crm_accounts để xác nhận"), { target: { value: "crm_accounts" } });
    fireEvent.click(confirm);
    expect(onApply).toHaveBeenCalledTimes(1);
  });

  it("Bỏ discards without writing", () => {
    const { onApply, onDiscard } = setup(DEL_FIELD);
    fireEvent.click(screen.getByRole("button", { name: "Bỏ" }));
    expect(onDiscard).toHaveBeenCalled();
    expect(onApply).not.toHaveBeenCalled();
  });
});

describe("ProposalCard — focus and names", () => {
  it("the card is focusable with a state+title name; acting on it moves focus to the card", () => {
    const { setState } = setup(FIELD);
    const card = screen.getByRole("article");
    expect(card).toHaveAttribute("tabindex", "-1");
    expect(card).toHaveAccessibleName("Đề xuất: Thêm trường “Ngày tái khám”");
    fireEvent.click(screen.getByRole("button", { name: "Áp dụng" }));
    setState({ status: "done" });
    expect(screen.getByRole("article")).toHaveFocus();
    expect(screen.getByRole("article")).toHaveAccessibleName("Đã áp dụng: Thêm trường “Ngày tái khám”");
  });

  it("a state change nobody clicked here (apply-all) does not steal focus", () => {
    const { setState } = setup(FIELD);
    setState({ status: "done" });
    expect(screen.getByRole("article")).not.toHaveFocus();
  });
});
