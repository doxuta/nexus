import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";

import { ConfirmPrompt, InstallResultView, PlanView } from "@/components/studio/workspaceShared";
import type { InstallPlan } from "@/lib/verticalPlan";
// InstallResult không được export từ lib/api — chỉ cần một object tối thiểu để render.
const RESULT = { created: {}, warnings: [] } as never;

// MỘT XƯỞNG RỔ-4 P1 (2026-07-29): the handover wizard shows one-time customer-admin credentials
// that live ONLY in wizard memory — closing destroys them and the new workspace is left with a
// single admin nobody can log in as. "Copy khối bàn giao" was an ordinary optional button and the
// close path was NOT gated, while the workspace-creation reveal in the SAME wizard already was.
// The irreversible action must be the gated one.
describe("InstallResultView — cổng đóng", () => {
  it("mặc định (không truyền closeDisabled) đóng tự do — VerticalLibrary không đổi hành vi", () => {
    const onClose = vi.fn();
    render(<InstallResultView result={RESULT} onClose={onClose} />);
    const btn = screen.getByRole("button", { name: "Đóng" });
    expect(btn).not.toBeDisabled();
    fireEvent.click(btn);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("closeDisabled → nút Đóng bị khoá, có lý do hiện ra, và onClose KHÔNG chạy", () => {
    const onClose = vi.fn();
    render(
      <InstallResultView
        result={RESULT}
        onClose={onClose}
        closeDisabled
        closeDisabledHint="Xác nhận đã lưu khối bàn giao bên dưới trước khi đóng."
      />,
    );
    const btn = screen.getByRole("button", { name: "Đóng" });
    expect(btn).toBeDisabled();
    expect(screen.getByText(/Xác nhận đã lưu khối bàn giao/)).toBeInTheDocument();
    fireEvent.click(btn);
    expect(onClose).not.toHaveBeenCalled();
  });
});

// Queue #106 (from #82): a pack install can succeed and still carry notes the operator must read
// (no default business calendar → SLA rules fall back to wall-clock hours). The install screen
// keeps printing result.warnings verbatim.
describe("InstallResultView — cảnh báo khi cài", () => {
  it("prints every result.warnings line verbatim", () => {
    const warn = "Chưa có lịch làm việc mặc định — 4 luật SLA sẽ tính theo giờ treo tường";
    render(<InstallResultView result={{ warnings: [warn, "Bỏ qua ghi nguồn gốc: pack không có slug"] } as never} onClose={() => {}} />);
    expect(screen.getByText(warn)).toBeInTheDocument();
    expect(screen.getByText("Bỏ qua ghi nguồn gốc: pack không có slug")).toBeInTheDocument();
  });
});

// Queue #104: the dry-run plan now carries the same install notes (plan.warnings) so the operator
// reads "no default working calendar" BEFORE clicking Cài — printed verbatim, and it does NOT turn
// a clean plan into a conflicted one (warnings never gate the confirm).
describe("PlanView — cảnh báo trước khi cài", () => {
  const plan: InstallPlan = {
    entitiesToCreate: ["tk_tickets"], entitiesToSkip: [], fieldsToAdd: [], nnToRegister: [], nnToSkip: [],
    layoutsToInstall: [], rolesToCreate: [], rolesToSkip: [], samplesCount: 0, conflicts: [],
  };
  it("prints every plan.warnings line verbatim and keeps the plan clean", () => {
    const warn = "4 luật của gói tính theo giờ làm việc (Đặt hạn SLA — phiếu Khẩn, …) nhưng tổ chức chưa có lịch làm việc mặc định";
    render(<PlanView plan={{ ...plan, warnings: [warn] }} />);
    expect(screen.getByText(warn)).toBeInTheDocument();
    expect(screen.getByText("Sẵn sàng cài")).toBeInTheDocument();
  });
  it("an older server without the key renders no warnings block", () => {
    render(<PlanView plan={plan} />);
    expect(screen.queryByTestId("plan-warnings")).toBeNull();
  });
});

describe("ConfirmPrompt", () => {
  it("type-name: confirm stays locked until the exact name is typed; the typed text resets on reopen", () => {
    const onConfirm = vi.fn();
    const { rerender } = render(
      <ConfirmPrompt open onOpenChange={() => {}} title="Xoá?" confirmName="acme" confirmLabel="Xoá workspace" danger onConfirm={onConfirm} />,
    );
    const dialog = screen.getByRole("alertdialog");
    const confirm = screen.getByRole("button", { name: "Xoá workspace" });
    expect(dialog).toHaveClass("slds-modal_prompt");
    expect(confirm).toBeDisabled();
    fireEvent.change(screen.getByLabelText("Gõ acme để xác nhận"), { target: { value: "Acme" } });
    expect(confirm).toBeDisabled(); // case-sensitive
    fireEvent.change(screen.getByLabelText("Gõ acme để xác nhận"), { target: { value: "acme" } });
    expect(confirm).toBeEnabled();
    fireEvent.click(confirm);
    expect(onConfirm).toHaveBeenCalledTimes(1);

    rerender(<ConfirmPrompt open={false} onOpenChange={() => {}} title="Xoá?" confirmName="acme" confirmLabel="Xoá workspace" danger onConfirm={onConfirm} />);
    rerender(<ConfirmPrompt open onOpenChange={() => {}} title="Xoá?" confirmName="acme" confirmLabel="Xoá workspace" danger onConfirm={onConfirm} />);
    expect(screen.getByLabelText("Gõ acme để xác nhận")).toHaveValue("");
    expect(screen.getByRole("button", { name: "Xoá workspace" })).toBeDisabled();
  });

  it("simple: no name to type, loading locks both buttons", () => {
    render(<ConfirmPrompt open onOpenChange={() => {}} title="Cài?" confirmLabel="Cài đặt" loading onConfirm={() => {}} />);
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.getByRole("button", { name: "Cài đặt" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Huỷ" })).toBeDisabled();
  });
});
