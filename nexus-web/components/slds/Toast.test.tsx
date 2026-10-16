// Toast + Toaster — blueprint toast/base (SLDS 2.264.1). The Toaster keeps the old
// components/ui/Toaster call shape (toast.success(msg) …) so it can replace it without touching
// call sites; toast({variant,title,message}) is the full form.
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, act, fireEvent } from "@testing-library/react";
import { Toast, Toaster, toast } from "./Toast";

afterEach(() => {
  vi.useRealTimers();
});

describe("Toast (presentational)", () => {
  it("renders the blueprint markup for each theme", () => {
    for (const variant of ["info", "success", "warning", "error"] as const) {
      const { container, unmount } = render(<Toast variant={variant} title="Đã lưu khách hàng" onClose={() => {}} />);
      const el = container.querySelector(".slds-notify")!;
      expect(el).toHaveClass("slds-notify", "slds-notify_toast", `slds-theme_${variant}`);
      expect(el).toHaveAttribute("role", "status");
      expect(el.firstElementChild).toHaveClass("slds-assistive-text");
      expect(el.querySelector(`.slds-icon_container.slds-icon-utility-${variant}`)).toHaveClass("slds-m-right_small", "slds-no-flex", "slds-align-top");
      expect(el.querySelector(".slds-notify__content h2.slds-text-heading_small")).toHaveTextContent("Đã lưu khách hàng");
      expect(el.querySelector(".slds-notify__close button")).toHaveClass("slds-button", "slds-button_icon", "slds-button_icon-inverse");
      unmount();
    }
  });

  it("names the variant for assistive tech in Vietnamese", () => {
    render(<Toast variant="error" title="Không lưu được" onClose={() => {}} />);
    expect(screen.getByText("Lỗi")).toHaveClass("slds-assistive-text");
  });

  it("renders the detail line and an inline action link", () => {
    const onAction = vi.fn();
    const onClose = vi.fn();
    const { container } = render(
      <Toast variant="success" title="Đã xoá 3 bản ghi" message="Có thể khôi phục trong 30 ngày." action={{ label: "Hoàn tác", onClick: onAction }} onClose={onClose} />,
    );
    expect(container.querySelector(".slds-notify__content p")).toHaveTextContent("Có thể khôi phục trong 30 ngày.");
    fireEvent.click(screen.getByText("Hoàn tác"));
    expect(onAction).toHaveBeenCalledTimes(1);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("closes from the close button", () => {
    const onClose = vi.fn();
    render(<Toast title="x" onClose={onClose} />);
    fireEvent.click(screen.getByRole("button", { name: "Đóng" }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});

describe("Toaster", () => {
  it("renders a polite live region at the top centre (slds-notify_container)", () => {
    const { container } = render(<Toaster />);
    const region = container.querySelector(".slds-notify_container")!;
    expect(region).toHaveAttribute("aria-live", "polite");
  });

  it("shows toasts pushed through toast() and the legacy helpers", () => {
    render(<Toaster />);
    act(() => {
      toast({ variant: "warning", title: "Thiếu email", message: "Bản ghi vẫn được lưu." });
      toast.error("Không xoá được bản ghi");
    });
    expect(screen.getByText("Thiếu email").closest(".slds-notify")).toHaveClass("slds-theme_warning");
    expect(screen.getByText("Không xoá được bản ghi").closest(".slds-notify")).toHaveClass("slds-theme_error");
  });

  it("auto-hides after the legacy per-type durations (success 4s, error 6s, warning 5s, info 4s, withAction 8s)", () => {
    vi.useFakeTimers();
    render(<Toaster />);
    act(() => {
      toast.success("Đã lưu");
      toast.info("Đang đồng bộ");
      toast.warning("Sắp hết hạn");
      toast.error("Lỗi mạng");
      toast.withAction("Đã xoá", { label: "Hoàn tác", onClick: () => {} });
    });
    const has = (x: string) => screen.queryByText(x) !== null;
    act(() => void vi.advanceTimersByTime(3999));
    expect(has("Đã lưu") && has("Đang đồng bộ")).toBe(true);
    act(() => void vi.advanceTimersByTime(1));
    expect(has("Đã lưu") || has("Đang đồng bộ")).toBe(false);
    expect(has("Sắp hết hạn")).toBe(true);
    act(() => void vi.advanceTimersByTime(1000));
    expect(has("Sắp hết hạn")).toBe(false);
    expect(has("Lỗi mạng")).toBe(true);
    act(() => void vi.advanceTimersByTime(1000));
    expect(has("Lỗi mạng")).toBe(false);
    expect(has("Đã xoá")).toBe(true);
    act(() => void vi.advanceTimersByTime(2000));
    expect(has("Đã xoá")).toBe(false);
  });

  it("keeps only the 5 newest toasts", () => {
    render(<Toaster />);
    act(() => {
      for (let i = 1; i <= 7; i++) toast.info(`Tin ${i}`);
    });
    expect(document.querySelectorAll(".slds-notify")).toHaveLength(5);
    expect(screen.queryByText("Tin 2")).toBeNull();
    expect(screen.getByText("Tin 7")).toBeInTheDocument();
  });

  it("honours an explicit duration and removes a toast from its close button", () => {
    vi.useFakeTimers();
    render(<Toaster />);
    act(() => {
      toast.error("Hết phiên", 1000);
      toast.warning("Cảnh báo");
    });
    act(() => {
      vi.advanceTimersByTime(1000);
    });
    expect(screen.queryByText("Hết phiên")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Đóng" }));
    expect(screen.queryByText("Cảnh báo")).toBeNull();
  });

  it("pauses auto-hide while the pointer is over the toast or focus is inside it", () => {
    vi.useFakeTimers();
    render(<Toaster />);
    act(() => {
      toast.withAction("Đã xoá 3 bản ghi", { label: "Hoàn tác", onClick: () => {} });
    });
    const link = screen.getByText("Hoàn tác");
    const box = link.closest(".slds-notify")!;
    // Keyboard user tabbed to the undo link: the toast must wait for them.
    act(() => link.focus());
    act(() => {
      vi.advanceTimersByTime(60_000);
    });
    expect(document.querySelector(".slds-notify")).not.toBeNull();
    // Pointer over it while focus leaves: still paused.
    fireEvent.mouseEnter(box);
    act(() => link.blur());
    act(() => {
      vi.advanceTimersByTime(60_000);
    });
    expect(document.querySelector(".slds-notify")).not.toBeNull();
    // Pointer leaves too: the countdown restarts and the toast goes.
    fireEvent.mouseLeave(box);
    act(() => {
      vi.advanceTimersByTime(7_999);
    });
    expect(document.querySelector(".slds-notify")).not.toBeNull();
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(document.querySelector(".slds-notify")).toBeNull();
  });

  it("runs withAction's callback and dismisses the toast", () => {
    const undo = vi.fn();
    render(<Toaster />);
    act(() => {
      toast.withAction("Đã thêm vào nav", { label: "Hoàn tác", onClick: undo });
    });
    expect(document.querySelector(".slds-notify")).not.toBeNull();
    fireEvent.click(screen.getByText("Hoàn tác"));
    expect(undo).toHaveBeenCalledTimes(1);
    expect(document.querySelector(".slds-notify")).toBeNull();
  });
});
