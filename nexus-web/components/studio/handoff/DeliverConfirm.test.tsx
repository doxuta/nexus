import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { readFileSync } from "node:fs";
import { beforeEach, describe, expect, it, vi } from "vitest";

const publishBaseline = vi.fn();
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), publishBaseline: (n?: string) => publishBaseline(n) }));
const toastSuccess = vi.fn();
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: (m: string) => toastSuccess(m), error: vi.fn() } }));

import { ApiError } from "@/lib/api";
import { DeliverConfirm } from "./DeliverConfirm";

const onDelivered = vi.fn();
const ui = (p: Partial<React.ComponentProps<typeof DeliverConfirm>> = {}) =>
  render(<DeliverConfirm blocked={[]} keptSamples={0} ownerEmail="chu@khach.vn" onDelivered={onDelivered} {...p} />);
const deliverBtn = () => screen.getByRole("button", { name: "Giao cho khách: phiên bản 1" });

beforeEach(() => {
  vi.clearAllMocks();
  publishBaseline.mockResolvedValue({ versionNo: 1, kind: "baseline", docCount: 9, publishedAt: "", propagationSeconds: 30 });
});

describe("DeliverConfirm", () => {
  it("blocked: aria-disabled (not disabled), focusable, describes the count, click never publishes", () => {
    const focusRow = vi.fn();
    ui({ blocked: ["steps", "app:a1"], onFocusBlocked: focusRow });
    const b = deliverBtn();
    expect(b.getAttribute("aria-disabled")).toBe("true");
    expect(b).not.toBeDisabled();
    expect(document.getElementById(b.getAttribute("aria-describedby")!)?.textContent).toBe("Còn 2 việc chưa xong ở trên.");
    fireEvent.click(b);
    expect(focusRow).toHaveBeenCalledWith("steps");
    expect(publishBaseline).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("opens inline (no dialog), focuses Chưa giao, Esc returns focus to the deliver button", () => {
    ui();
    fireEvent.click(deliverBtn());
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByText(/không quay lại được/)).toBeInTheDocument();
    expect(screen.getByText("Chủ khách đăng nhập bằng chu@khach.vn.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Chưa giao" })).toHaveFocus();
    fireEvent.keyDown(screen.getByRole("button", { name: "Chưa giao" }), { key: "Escape" });
    expect(deliverBtn()).toHaveFocus();
  });

  it("no owner: the owner sentence is gone", () => {
    ui({ ownerEmail: undefined });
    fireEvent.click(deliverBtn());
    expect(screen.queryByText(/Chủ khách đăng nhập bằng/)).toBeNull();
  });

  it("confirm publishes exactly once with the note, toasts with the wait, calls onDelivered", async () => {
    ui();
    fireEvent.click(deliverBtn());
    fireEvent.change(screen.getByLabelText(/Ghi chú/), { target: { value: "bản đầu" } });
    fireEvent.click(screen.getByRole("button", { name: "Giao phiên bản 1" }));
    fireEvent.click(screen.getByRole("button", { name: "Giao phiên bản 1" }));
    await waitFor(() => expect(onDelivered).toHaveBeenCalled());
    expect(publishBaseline).toHaveBeenCalledTimes(1);
    expect(publishBaseline).toHaveBeenCalledWith("bản đầu");
    expect(toastSuccess).toHaveBeenCalledWith("Đã giao phiên bản 1. Khách thấy bản này đầy đủ trong tối đa 30 giây.");
  });

  it("note over 500 chars: error under the box, confirm off", () => {
    ui();
    fireEvent.click(deliverBtn());
    fireEvent.change(screen.getByLabelText(/Ghi chú/), { target: { value: "x".repeat(501) } });
    expect(screen.getByText("Ghi chú tối đa 500 ký tự.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Giao phiên bản 1" })).toBeDisabled();
  });

  it("kept sample rows: confirm stays off until the checkbox is ticked", () => {
    ui({ keptSamples: 4 });
    fireEvent.click(deliverBtn());
    const yes = screen.getByRole("button", { name: "Giao phiên bản 1" });
    expect(yes).toBeDisabled();
    fireEvent.click(screen.getByLabelText("Tôi biết còn 4 bản ghi mẫu và khách sẽ thấy chúng."));
    expect(yes).not.toBeDisabled();
  });

  it("409 ALREADY_DELIVERED: refresh into the delivered screen, no red error", async () => {
    publishBaseline.mockRejectedValue(new ApiError(409, "ALREADY_DELIVERED", "x"));
    ui();
    fireEvent.click(deliverBtn());
    fireEvent.click(screen.getByRole("button", { name: "Giao phiên bản 1" }));
    await waitFor(() => expect(onDelivered).toHaveBeenCalled());
    expect(toastSuccess).not.toHaveBeenCalled();
    expect(screen.queryByText(/đã giao cho khách rồi/)).toBeNull();
  });

  it("403 and 5xx print inline, stay open and can retry", async () => {
    publishBaseline.mockRejectedValueOnce(new ApiError(403, "FORBIDDEN", "x"));
    ui();
    fireEvent.click(deliverBtn());
    fireEvent.click(screen.getByRole("button", { name: "Giao phiên bản 1" }));
    expect(await screen.findByText("Việc này chỉ người dựng Nexus làm được.")).toBeInTheDocument();
    publishBaseline.mockRejectedValueOnce(new ApiError(500, "INTERNAL", "lỗi hệ thống"));
    fireEvent.click(screen.getByRole("button", { name: "Giao phiên bản 1" }));
    expect(await screen.findByText("lỗi hệ thống")).toBeInTheDocument();
    expect(onDelivered).not.toHaveBeenCalled();
  });

  it("two clicks in one batch (before busy renders) still publish once", async () => {
    publishBaseline.mockReturnValue(new Promise(() => {}));
    ui();
    fireEvent.click(deliverBtn());
    const yes = screen.getByRole("button", { name: "Giao phiên bản 1" });
    act(() => {
      yes.click();
      yes.click();
    });
    expect(publishBaseline).toHaveBeenCalledTimes(1);
  });

  it("after 201 the confirm stays locked (no second publish while the page refetches)", async () => {
    ui();
    fireEvent.click(deliverBtn());
    fireEvent.click(screen.getByRole("button", { name: "Giao phiên bản 1" }));
    await waitFor(() => expect(onDelivered).toHaveBeenCalled());
    expect(screen.getByRole("button", { name: "Giao phiên bản 1" })).toBeDisabled();
  });

  it("422 prints under the note box; 503 names the lock end time; errors are role=alert", async () => {
    publishBaseline.mockRejectedValueOnce(new ApiError(422, "INVALID_BODY", "Ghi chú không hợp lệ."));
    ui();
    fireEvent.click(deliverBtn());
    fireEvent.click(screen.getByRole("button", { name: "Giao phiên bản 1" }));
    expect(await screen.findByText("Nội dung gửi lên không hợp lệ.")).toBeInTheDocument();
    publishBaseline.mockRejectedValueOnce(new ApiError(503, "TENANT_MAINTENANCE", "x", { until: "2026-10-14T03:05:00Z" }));
    fireEvent.click(screen.getByRole("button", { name: "Giao phiên bản 1" }));
    const msg = await screen.findByText(/Khách này đang bảo trì nên tạm khoá ghi, đến \d{2}:\d{2}/);
    expect(msg.closest("[role=alert]")).not.toBeNull();
    expect(onDelivered).not.toHaveBeenCalled();
  });

  it("source never touches the project stage (no PATCH) and never says baseline", () => {
    const src = readFileSync("components/studio/handoff/DeliverConfirm.tsx", "utf8").split("\n").filter((l) => !l.trim().startsWith("//")).join("\n");
    expect(src).not.toMatch(/PATCH|updateProject|setProjectStage/);
  });

  it("never mentions baseline in the UI text", () => {
    const { container } = ui();
    fireEvent.click(deliverBtn());
    expect(container.textContent).not.toMatch(/baseline|tài liệu|máy chủ/i);
  });
});
