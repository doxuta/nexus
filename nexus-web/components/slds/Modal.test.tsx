// Modal + Prompt — blueprints modals/base and prompt/base (SLDS 2.264.1) on @radix-ui/react-dialog.
import { describe, it, expect, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useRef, useState } from "react";
import { Modal, Prompt } from "./Modal";
import { Toaster, toast } from "./Toast";

// Radix attaches its outside-pointerdown listener on setTimeout(0) after opening.
const nextTask = () => act(() => new Promise((r) => setTimeout(r, 0)));

/** Closes the error toast (a separate React tree, stacked above the modal) the way a user does. */
async function closeToast() {
  const close = screen.getByText("Không lưu được").closest(".slds-notify")!.querySelector("button")!;
  // fireEvent, not userEvent: Radix sets body{pointer-events:none} and the toast's
  // pointer-events-auto class has no CSS in tests.
  fireEvent.pointerDown(close);
  fireEvent.mouseDown(close);
  fireEvent.pointerUp(close);
  fireEvent.mouseUp(close);
  fireEvent.click(close);
}

function footer() {
  return (
    <>
      <button type="button" className="slds-button slds-button_neutral">
        Huỷ
      </button>
      <button type="button" className="slds-button slds-button_brand">
        Lưu
      </button>
    </>
  );
}

describe("Modal", () => {
  it("renders the modals/base markup with header, tagline, body, footer and backdrop", () => {
    render(
      <Modal open onOpenChange={() => {}} title="Khách hàng mới" tagline="Điền các trường bắt buộc." size="large" footer={footer()}>
        <p>Nội dung</p>
      </Modal>,
    );
    const dialog = screen.getByRole("dialog", { name: "Khách hàng mới" });
    expect(dialog.tagName).toBe("SECTION");
    expect(dialog).toHaveClass("slds-modal", "slds-fade-in-open", "slds-modal_large");
    expect(dialog).toHaveAttribute("aria-modal", "true");
    const container = dialog.querySelector(".slds-modal__container")!;
    const heading = container.querySelector(".slds-modal__header h1")!;
    expect(heading).toHaveClass("slds-modal__title", "slds-hyphenate");
    expect(dialog.getAttribute("aria-labelledby")).toBe(heading.id);
    expect(container.querySelector(".slds-modal__header p.slds-m-top_x-small")).toHaveTextContent("Điền các trường bắt buộc.");
    expect(container.querySelector(".slds-modal__content")).toHaveClass("slds-p-around_medium");
    expect(container.querySelector(".slds-modal__footer")).not.toHaveClass("slds-modal__footer_directional");
    expect(document.querySelector(".slds-backdrop.slds-backdrop_open")).toHaveAttribute("role", "presentation");
    expect(dialog).not.toHaveAttribute("aria-describedby");
  });

  it("puts the close button first and closes from it", async () => {
    const onOpenChange = vi.fn();
    render(
      <Modal open onOpenChange={onOpenChange} title="Sửa">
        x
      </Modal>,
    );
    const close = screen.getByRole("button", { name: "Huỷ và đóng" });
    expect(close).toHaveClass("slds-button", "slds-button_icon", "slds-modal__close");
    expect(close.parentElement!.firstElementChild).toBe(close);
    await userEvent.click(close);
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("traps focus: Tab, Shift+Tab or a stray focus never leave it, and the page behind is hidden from AT", async () => {
    render(
      <>
        <button type="button">Ngoài</button>
        <Modal open onOpenChange={() => {}} title="Sửa" footer={footer()}>
          <input aria-label="Tên" />
        </Modal>
      </>,
    );
    const dialog = screen.getByRole("dialog");
    const outside = screen.getByText("Ngoài");
    expect(dialog.contains(document.activeElement)).toBe(true);
    expect(outside.closest("[aria-hidden='true']")).not.toBeNull();
    for (let i = 0; i < 6; i++) {
      await userEvent.tab();
      expect(document.activeElement).not.toBe(outside);
      expect(dialog.contains(document.activeElement)).toBe(true);
    }
    for (let i = 0; i < 6; i++) {
      await userEvent.tab({ shift: true });
      expect(document.activeElement).not.toBe(outside);
      expect(dialog.contains(document.activeElement)).toBe(true);
    }
    // Radix loops Tab even without a trap, so the trap itself shows only when focus is pulled out
    // (a click or script on the page behind): the modal must take it back.
    act(() => outside.focus());
    expect(dialog.contains(document.activeElement)).toBe(true);
  });

  it("closes on Escape", async () => {
    const onOpenChange = vi.fn();
    render(
      <Modal open onOpenChange={onOpenChange} title="Sửa" footer={footer()}>
        <input aria-label="Tên" />
      </Modal>,
    );
    await userEvent.keyboard("{Escape}");
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("stays open when the user closes a toast shown over it", async () => {
    const onOpenChange = vi.fn();
    render(
      <>
        <Modal open onOpenChange={onOpenChange} title="Khách hàng mới" footer={footer()}>
          <input aria-label="Tên" />
        </Modal>
        <Toaster />
      </>,
    );
    act(() => {
      toast.error("Không lưu được");
    });
    await nextTask();
    await closeToast();
    expect(screen.queryByText("Không lưu được")).toBeNull();
    expect(onOpenChange).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog", { name: "Khách hàng mới" })).toBeInTheDocument();
  });

  it("opens from a trigger and returns focus to it on close", async () => {
    render(
      <Modal title="Nhập dữ liệu" trigger={<button type="button">Mở</button>}>
        x
      </Modal>,
    );
    const trigger = screen.getByRole("button", { name: "Mở" });
    await userEvent.click(trigger);
    expect(screen.getByRole("dialog", { name: "Nhập dữ liệu" })).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(trigger);
  });

  // SLDS docs (modals, Keyboard Interaction): "Esc key closes the modal and moves focus to
  // whatever triggered the modal to open". Radix only does that for its own Trigger.
  it("controlled without a trigger: Escape returns focus to the element that opened it", async () => {
    function Harness() {
      const [open, setOpen] = useState(false);
      return (
        <>
          <button type="button" onClick={() => setOpen(true)}>
            Mở
          </button>
          <Modal open={open} onOpenChange={setOpen} title="Sửa">
            x
          </Modal>
        </>
      );
    }
    render(<Harness />);
    const opener = screen.getByRole("button", { name: "Mở" });
    await userEvent.click(opener);
    expect(screen.getByRole("dialog", { name: "Sửa" })).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    await waitFor(() => expect(opener).toHaveFocus());
  });

  it("when the flow removed the opener, focus goes to returnFocus()", async () => {
    function Harness() {
      const [open, setOpen] = useState(false);
      const [done, setDone] = useState(false);
      const next = useRef<HTMLButtonElement>(null);
      return (
        <>
          {done ? (
            <button type="button" ref={next}>
              Bước kế
            </button>
          ) : (
            <button type="button" onClick={() => setOpen(true)}>
              Bắt đầu
            </button>
          )}
          <Modal
            open={open}
            onOpenChange={setOpen}
            title="Bước"
            returnFocus={() => next.current}
            footer={
              <button type="button" onClick={() => { setDone(true); setOpen(false); }}>
                Xong
              </button>
            }
          >
            x
          </Modal>
        </>
      );
    }
    render(<Harness />);
    await userEvent.click(screen.getByRole("button", { name: "Bắt đầu" }));
    await userEvent.click(screen.getByRole("button", { name: "Xong" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    await waitFor(() => expect(screen.getByRole("button", { name: "Bước kế" })).toHaveFocus());
  });

  // A menu item that opens a dialog is unmounted before the dialog mounts, so the "opener" Radix
  // sees is <body> — still connected, but no place to return to. returnFocus() must win.
  it("when nothing was focused at open (<body>), focus goes to returnFocus()", async () => {
    function Harness() {
      const [open, setOpen] = useState(false);
      const back = useRef<HTMLButtonElement>(null);
      return (
        <>
          <button type="button" ref={back}>
            Thêm thao tác
          </button>
          <button type="button" onClick={() => setOpen(true)}>
            Mở
          </button>
          <Modal open={open} onOpenChange={setOpen} title="Xoá" returnFocus={() => back.current}>
            x
          </Modal>
        </>
      );
    }
    render(<Harness />);
    const opener = screen.getByRole("button", { name: "Mở" });
    opener.focus();
    opener.blur(); // activeElement = <body> when the dialog opens
    fireEvent.click(opener);
    expect(screen.getByRole("dialog", { name: "Xoá" })).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    await waitFor(() => expect(screen.getByRole("button", { name: "Thêm thao tác" })).toHaveFocus());
  });

  it("renders the headless + footless variant with an accessible name", () => {
    render(
      <Modal open onOpenChange={() => {}} label="Xem trước tệp">
        x
      </Modal>,
    );
    const dialog = screen.getByRole("dialog", { name: "Xem trước tệp" });
    expect(dialog.querySelector(".slds-modal__header")).toBeNull();
    expect(dialog.querySelector(".slds-modal__content")).toHaveClass("slds-modal__content_headless", "slds-modal__content_footless");
    expect(dialog.querySelector(".slds-modal__footer")).toBeNull();
  });

  it("renders the directional footer", () => {
    render(
      <Modal open onOpenChange={() => {}} title="Bước 1" footer={footer()} directionalFooter>
        x
      </Modal>,
    );
    expect(screen.getByRole("dialog").querySelector(".slds-modal__footer")).toHaveClass("slds-modal__footer_directional");
  });
});

describe("Prompt", () => {
  it("renders prompt/base as an alertdialog described by its message", () => {
    render(
      <Prompt open onOpenChange={() => {}} theme="error" title="Dịch vụ không khả dụng">
        Máy chủ đang bảo trì.
      </Prompt>,
    );
    const dialog = screen.getByRole("alertdialog", { name: "Dịch vụ không khả dụng" });
    expect(dialog).toHaveClass("slds-modal", "slds-fade-in-open", "slds-modal_prompt");
    expect(dialog).toHaveAttribute("aria-modal", "true");
    expect(dialog.querySelector(".slds-modal__header")).toHaveClass("slds-theme_error", "slds-theme_alert-texture");
    expect(dialog.querySelector(".slds-modal__header h1")).toHaveClass("slds-text-heading_medium");
    const message = dialog.querySelector(".slds-modal__content")!;
    expect(dialog.getAttribute("aria-describedby")).toBe(message.id);
    expect(message).toHaveAttribute("tabindex", "0");
    expect(dialog.querySelector(".slds-modal__footer")).toHaveClass("slds-theme_default");
    expect(dialog.querySelector(".slds-modal__close")).toBeNull();
  });

  it("gives the message initial focus and dismisses with the default button or Escape", async () => {
    const onOpenChange = vi.fn();
    render(
      <Prompt open onOpenChange={onOpenChange} theme="warning" title="Sắp hết hạn">
        Gói dịch vụ còn 3 ngày.
      </Prompt>,
    );
    const message = screen.getByText("Gói dịch vụ còn 3 ngày.").closest(".slds-modal__content");
    expect(document.activeElement).toBe(message);
    await userEvent.keyboard("{Escape}");
    expect(onOpenChange).toHaveBeenCalledWith(false);
    onOpenChange.mockClear();
    await userEvent.click(screen.getByRole("button", { name: "Đã hiểu" }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("controlled without a trigger: dismissing returns focus to the element that opened it", async () => {
    function Harness() {
      const [open, setOpen] = useState(false);
      return (
        <>
          <button type="button" onClick={() => setOpen(true)}>
            Xem cảnh báo
          </button>
          <Prompt open={open} onOpenChange={setOpen} title="Sắp hết hạn">
            Gói dịch vụ còn 3 ngày.
          </Prompt>
        </>
      );
    }
    render(<Harness />);
    const opener = screen.getByRole("button", { name: "Xem cảnh báo" });
    await userEvent.click(opener);
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("alertdialog")).toBeNull();
    await waitFor(() => expect(opener).toHaveFocus());
  });

  it("stays open when the user closes a toast shown over it", async () => {
    const onOpenChange = vi.fn();
    render(
      <>
        <Prompt open onOpenChange={onOpenChange} title="Sắp hết hạn">
          Gói dịch vụ còn 3 ngày.
        </Prompt>
        <Toaster />
      </>,
    );
    act(() => {
      toast.error("Không lưu được");
    });
    await nextTask();
    await closeToast();
    expect(screen.queryByText("Không lưu được")).toBeNull();
    expect(onOpenChange).not.toHaveBeenCalled();
    expect(screen.getByRole("alertdialog")).toBeInTheDocument();
  });
});
