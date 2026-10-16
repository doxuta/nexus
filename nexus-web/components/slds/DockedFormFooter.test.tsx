// DockedFormFooter — blueprint docked-form-footer/base (SLDS 2.264.1): default, with errors, error popover.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { DockedFormFooter } from "./DockedFormFooter";

describe("DockedFormFooter", () => {
  it("renders the blueprint footer with Huỷ then Lưu", async () => {
    const onCancel = vi.fn();
    const onSave = vi.fn();
    const { container } = render(<DockedFormFooter onCancel={onCancel} onSave={onSave} />);
    const el = container.firstElementChild!;
    expect(el).toHaveClass("slds-docked-form-footer");
    const [cancel, save] = Array.from(el.children);
    expect(cancel).toHaveClass("slds-button", "slds-button_neutral");
    expect(cancel).toHaveTextContent("Huỷ");
    expect(cancel).toHaveAttribute("type", "button");
    expect(save).toHaveClass("slds-button", "slds-button_brand");
    expect(save).toHaveTextContent("Lưu");
    await userEvent.click(cancel as HTMLElement);
    await userEvent.click(save as HTMLElement);
    expect(onCancel).toHaveBeenCalledTimes(1);
    expect(onSave).toHaveBeenCalledTimes(1);
  });

  it("shows children as text left of the buttons; unchanged without children", () => {
    const { container, rerender } = render(<DockedFormFooter onCancel={() => {}} onSave={() => {}}>Đang xem trước: Trường mới</DockedFormFooter>);
    const el = container.firstElementChild!;
    const text = screen.getByText("Đang xem trước: Trường mới");
    expect(el.firstElementChild).toBe(text);
    expect(el.children).toHaveLength(3);
    rerender(<DockedFormFooter onCancel={() => {}} onSave={() => {}} />);
    expect(container.firstElementChild!.children).toHaveLength(2);
  });

  it("submits a form by id and disables Lưu while saving", () => {
    render(<DockedFormFooter onCancel={() => {}} form="account-form" saving />);
    const save = screen.getByRole("button", { name: "Lưu" });
    expect(save).toHaveAttribute("type", "submit");
    expect(save).toHaveAttribute("form", "account-form");
    expect(save).toBeDisabled();
  });

  it("opens the error popover by itself when errors appear after Lưu, focus inside", async () => {
    // docs.mdx: "When errors are found within a form, the user will be notified with a popover".
    const { rerender } = render(<DockedFormFooter onCancel={() => {}} onSave={() => {}} />);
    expect(screen.queryByRole("dialog")).toBeNull();
    rerender(<DockedFormFooter onCancel={() => {}} onSave={() => {}} errors={<p>Email không đúng định dạng.</p>} />);
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("Email không đúng định dạng.");
    expect(dialog.querySelector("button.slds-popover__close")).toHaveFocus();
  });

  it("shows the error button first with its popover open; Esc closes, the button reopens it", async () => {
    const { container } = render(
      <DockedFormFooter onCancel={() => {}} onSave={() => {}} errors={<p>Email không đúng định dạng.</p>} />,
    );
    const footer = container.firstElementChild!;
    const errBtn = footer.children[0] as HTMLElement;
    expect(errBtn).toHaveClass("slds-button", "slds-button_icon", "slds-button_icon-error");
    expect(errBtn).toHaveAttribute("title", "Có lỗi cần sửa");
    expect(errBtn.querySelector("svg")).toHaveClass("slds-button__icon", "slds-button__icon_large");
    expect(errBtn).toHaveAttribute("aria-expanded", "true");

    const dialog = await screen.findByRole("dialog");
    expect(dialog.tagName).toBe("SECTION");
    expect(dialog).toHaveClass("slds-popover", "slds-popover_error");
    const heading = dialog.querySelector(".slds-popover__header h2")!;
    expect(heading).toHaveClass("slds-truncate", "slds-text-heading_medium");
    expect(heading).toHaveTextContent("Có lỗi cần sửa");
    expect(dialog).toHaveAttribute("aria-labelledby", heading.id);
    const body = dialog.querySelector(".slds-popover__body")!;
    expect(dialog).toHaveAttribute("aria-describedby", body.id);
    expect(body).toHaveTextContent("Email không đúng định dạng.");
    expect(dialog.querySelector(".slds-popover__header .slds-icon_container.slds-icon-utility-error")).not.toBeNull();
    const close = dialog.querySelector("button.slds-popover__close")!;
    expect(close).toHaveClass("slds-button_icon-small", "slds-float_right", "slds-button_icon-inverse");

    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(errBtn).toHaveFocus();

    await userEvent.click(errBtn);
    expect(await screen.findByRole("dialog")).toHaveClass("slds-popover_error");
  });

  it("closes the error popover from its close button", async () => {
    render(<DockedFormFooter onCancel={() => {}} errors="Thiếu tên khách hàng." />);
    await userEvent.click(await screen.findByRole("button", { name: "Đóng hộp thoại" }));
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
