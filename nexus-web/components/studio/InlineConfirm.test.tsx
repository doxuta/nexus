// specs/073 kh1a T144/T152: confirmation in the page (never window.confirm). `children` is declared from the start
// so F4/F5/F6 do not add props of their own (queue #559, #577).
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { InlineConfirm } from "./InlineConfirm";

const base = { tone: "error" as const, message: "Xoá dự án Acme?", confirmLabel: "Xoá dự án" };

describe("InlineConfirm", () => {
  it("names what is affected and offers confirm + cancel", () => {
    render(<InlineConfirm {...base} onConfirm={() => {}} onCancel={() => {}} />);
    expect(screen.getByText("Xoá dự án Acme?")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Xoá dự án" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Huỷ" })).toBeInTheDocument();
  });

  it("puts focus on the safe button (cancel), not on the destructive one", () => {
    render(<InlineConfirm {...base} onConfirm={() => {}} onCancel={() => {}} />);
    expect(screen.getByRole("button", { name: "Huỷ" })).toHaveFocus();
  });

  it("Esc cancels", () => {
    const onCancel = vi.fn();
    render(<InlineConfirm {...base} onConfirm={() => {}} onCancel={onCancel} />);
    fireEvent.keyDown(screen.getByRole("button", { name: "Huỷ" }), { key: "Escape" });
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("both buttons are described by the question, so a screen reader hears it with 'Huỷ'", () => {
    render(<InlineConfirm {...base} onConfirm={() => {}} onCancel={() => {}} />);
    expect(screen.getByRole("button", { name: "Huỷ" })).toHaveAccessibleDescription(/Xoá dự án Acme\?/);
    expect(screen.getByRole("button", { name: "Xoá dự án" })).toHaveAccessibleDescription(/Xoá dự án Acme\?/);
  });

  it("while busy focus stays inside the box and Esc does not cancel", () => {
    const onCancel = vi.fn();
    const { container, rerender } = render(<InlineConfirm {...base} onConfirm={() => {}} onCancel={onCancel} />);
    rerender(<InlineConfirm {...base} busy onConfirm={() => {}} onCancel={onCancel} />);
    expect(container.firstElementChild).toHaveFocus();
    fireEvent.keyDown(container.firstElementChild!, { key: "Escape" });
    expect(onCancel).not.toHaveBeenCalled();
    rerender(<InlineConfirm {...base} onConfirm={() => {}} onCancel={onCancel} />);
    expect(screen.getByRole("button", { name: "Huỷ" })).toHaveFocus();
  });

  it("confirm and cancel call their handlers", () => {
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    render(<InlineConfirm {...base} onConfirm={onConfirm} onCancel={onCancel} />);
    fireEvent.click(screen.getByRole("button", { name: "Xoá dự án" }));
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("busy disables both buttons", () => {
    render(<InlineConfirm {...base} busy onConfirm={() => {}} onCancel={() => {}} />);
    expect(screen.getByRole("button", { name: "Xoá dự án" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Huỷ" })).toBeDisabled();
  });

  it("without children there is no extra region; with children they render between message and buttons", () => {
    const { container, rerender } = render(<InlineConfirm {...base} onConfirm={() => {}} onCancel={() => {}} />);
    expect(container.querySelector("[data-confirm-extra]")).toBeNull();
    rerender(
      <InlineConfirm {...base} onConfirm={() => {}} onCancel={() => {}}>
        <label>
          Gõ lại mã <input aria-label="mã" />
        </label>
      </InlineConfirm>,
    );
    const extra = container.querySelector("[data-confirm-extra]");
    expect(extra).not.toBeNull();
    expect(extra).toContainElement(screen.getByLabelText("mã"));
  });

  it("is built on a scoped notification, not a modal or dialog", () => {
    const { container } = render(<InlineConfirm {...base} onConfirm={() => {}} onCancel={() => {}} />);
    expect(container.querySelector(".slds-scoped-notification")).not.toBeNull();
    expect(container.querySelector("[role=dialog],[role=alertdialog]")).toBeNull();
  });

  it("an error tone uses the destructive button, other tones the brand one", () => {
    const { rerender } = render(<InlineConfirm {...base} onConfirm={() => {}} onCancel={() => {}} />);
    expect(screen.getByRole("button", { name: "Xoá dự án" })).toHaveClass("slds-button_destructive");
    rerender(<InlineConfirm {...base} tone="warning" onConfirm={() => {}} onCancel={() => {}} />);
    expect(screen.getByRole("button", { name: "Xoá dự án" })).toHaveClass("slds-button_brand");
  });
});
