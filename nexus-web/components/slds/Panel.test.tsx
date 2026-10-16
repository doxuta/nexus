// Panel — blueprint panels/base (SLDS 2.264.1): docked left/right, header with collapse button,
// optional drilled-in back button and secondary actions.
import { describe, it, expect, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState, type ReactNode } from "react";
import { Panel } from "./Panel";

/** Stub the 1280px breakpoint that Panel's media query asks about. */
function viewport(width: number) {
  window.matchMedia = ((query: string) => ({
    matches: width >= 1280 && query.includes("1280"),
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    onchange: null,
    dispatchEvent: () => false,
  })) as unknown as typeof window.matchMedia;
}

function Resp({ onClose = () => {} }: { onClose?: () => void }) {
  return (
    <>
      <button type="button" aria-controls="r-panel">
        Mở
      </button>
      <Panel id="r-panel" responsive open position="right" title="Chi tiết" onClose={onClose}>
        <input aria-label="Tìm" />
      </Panel>
    </>
  );
}

function ResponsiveHost() {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button type="button" aria-controls="r2" onClick={() => setOpen(true)}>
        Mở
      </button>
      <Panel id="r2" responsive open={open} title="Chi tiết" onClose={() => setOpen(false)}>
        <input aria-label="Tìm" />
      </Panel>
    </>
  );
}

describe("Panel — responsive (docked from 1280px, overlay below; specs/073 u0-be-mat)", () => {
  it("stays docked in the layout from 1280px: no backdrop, no dialog", () => {
    viewport(1440);
    const { container } = render(<Resp />);
    expect(container.querySelector(".slds-backdrop")).toBeNull();
    expect(container.querySelector(".slds-panel")).not.toHaveAttribute("role", "dialog");
    expect(container.querySelector(".slds-panel")).toHaveClass("slds-panel_docked-right", "slds-is-open");
  });

  it("becomes a modal overlay with a backdrop below 1280px", () => {
    viewport(1024);
    const { container } = render(<Resp />);
    expect(container.querySelector(".slds-backdrop.slds-backdrop_open")).not.toBeNull();
    const dialog = screen.getByRole("dialog", { name: "Chi tiết" });
    expect(dialog).toHaveAttribute("aria-modal", "true");
    expect(dialog).toHaveClass("slds-panel");
  });

  it("keeps the old docked markup without the responsive prop at any width", () => {
    viewport(1024);
    const { container } = render(
      <Panel open title="Bộ lọc" onClose={() => {}}>
        x
      </Panel>,
    );
    expect(container.querySelector(".slds-backdrop")).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("Esc closes the overlay; backdrop click closes it too", async () => {
    viewport(800);
    const onClose = vi.fn();
    const { container } = render(<Resp onClose={onClose} />);
    screen.getByRole("textbox", { name: "Tìm" }).focus();
    await userEvent.keyboard("{Escape}");
    expect(onClose).toHaveBeenCalledTimes(1);
    await userEvent.click(container.querySelector(".slds-backdrop")!);
    expect(onClose).toHaveBeenCalledTimes(2);
  });

  it("mounted open below 1280px: focus enters, Esc closes, Tab stays inside", async () => {
    viewport(800);
    const onClose = vi.fn();
    render(<Resp onClose={onClose} />);
    expect(screen.getByLabelText("Tìm")).toHaveFocus();
    await userEvent.tab();
    expect(screen.getByRole("button", { name: /Chi tiết/ })).toHaveFocus();
    await userEvent.keyboard("{Escape}");
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("an open docked panel crossing below 1280px takes the overlay duties", async () => {
    let wide = true;
    const subs = new Set<() => void>();
    window.matchMedia = ((query: string) => ({
      get matches() {
        return wide && query.includes("1280");
      },
      media: query,
      addEventListener: (_: string, cb: () => void) => subs.add(cb),
      removeEventListener: (_: string, cb: () => void) => subs.delete(cb),
    })) as unknown as typeof window.matchMedia;
    const onClose = vi.fn();
    render(<Resp onClose={onClose} />);
    expect(screen.getByLabelText("Tìm")).not.toHaveFocus();
    wide = false;
    await act(async () => subs.forEach((cb) => cb()));
    expect(screen.getByLabelText("Tìm")).toHaveFocus();
    await userEvent.keyboard("{Escape}");
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("traps Tab inside the overlay", async () => {
    viewport(800);
    render(<Resp />);
    const input = screen.getByRole("textbox", { name: "Tìm" });
    const close = screen.getByRole("button", { name: "Thu gọn Chi tiết" });
    close.focus();
    await userEvent.tab();
    expect(document.activeElement).toBe(input);
    await userEvent.tab();
    expect(document.activeElement).toBe(close);
    await userEvent.tab({ shift: true });
    expect(document.activeElement).toBe(input);
  });

  it("Esc closes and returns focus to the trigger", async () => {
    viewport(800);
    render(<ResponsiveHost />);
    const trigger = screen.getByRole("button", { name: "Mở" });
    await userEvent.click(trigger);
    expect(document.activeElement).toBe(screen.getByRole("textbox", { name: "Tìm" }));
    await userEvent.keyboard("{Escape}");
    expect(document.activeElement).toBe(trigger);
  });
});

/** A page with the panel's toggle button (aria-controls, per the panels docs) before it. */
function Page({ initial = false, children }: { initial?: boolean; children?: ReactNode }) {
  const [open, setOpen] = useState(initial);
  return (
    <>
      <button type="button" aria-expanded={open} aria-controls="kit-filter" onClick={() => setOpen((o) => !o)}>
        Hiện bảng lọc
      </button>
      <Panel id="kit-filter" open={open} title="Bộ lọc" onClose={() => setOpen(false)}>
        {children}
      </Panel>
    </>
  );
}

describe("Panel", () => {
  it("renders the docked-left blueprint markup when open", () => {
    const { container } = render(
      <Panel open title="Bộ lọc" onClose={() => {}}>
        Nội dung
      </Panel>,
    );
    const panel = container.querySelector(".slds-panel")!;
    expect(panel).toHaveClass("slds-panel", "slds-size_medium", "slds-panel_docked", "slds-panel_docked-left", "slds-is-open");
    const title = screen.getByRole("heading", { level: 2, name: "Bộ lọc" });
    expect(title).toHaveClass("slds-panel__header-title", "slds-text-heading_small", "slds-truncate");
    expect(title).toHaveAttribute("title", "Bộ lọc");
    expect(panel.querySelector(".slds-panel__header-actions .slds-panel__close")).not.toBeNull();
    expect(panel.querySelector(".slds-panel__body")).toHaveTextContent("Nội dung");
  });

  it("docks right and drops slds-is-open when closed", () => {
    const { container } = render(
      <Panel open={false} position="right" title="Chi tiết">
        x
      </Panel>,
    );
    const panel = container.querySelector(".slds-panel")!;
    expect(panel).toHaveClass("slds-panel_docked-right");
    expect(panel).not.toHaveClass("slds-is-open");
  });

  it("calls onClose from the collapse button", async () => {
    const onClose = vi.fn();
    render(
      <Panel open title="Bộ lọc" onClose={onClose}>
        x
      </Panel>,
    );
    const btn = screen.getByRole("button", { name: "Thu gọn Bộ lọc" });
    expect(btn).toHaveClass("slds-button", "slds-button_icon", "slds-button_icon-small", "slds-panel__close");
    await userEvent.click(btn);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("renders a back button first when drilled in, and centres the title on request", async () => {
    const onBack = vi.fn();
    const { container } = render(
      <Panel open title="Điều kiện" onBack={onBack} centeredTitle>
        x
      </Panel>,
    );
    const header = container.querySelector(".slds-panel__header")!;
    expect(header).toHaveClass("slds-panel__header_align-center");
    const back = header.firstElementChild!;
    expect(back).toHaveClass("slds-panel__back");
    await userEvent.click(screen.getByRole("button", { name: "Quay lại" }));
    expect(onBack).toHaveBeenCalledTimes(1);
    // No onClose → no collapse button.
    expect(container.querySelector(".slds-panel__close")).toBeNull();
  });

  it("puts secondary actions before the collapse button", () => {
    const { container } = render(
      <Panel open title="Bộ lọc" onClose={() => {}} actions={<button type="button">Thêm</button>}>
        x
      </Panel>,
    );
    const actions = container.querySelector(".slds-panel__header-actions")!;
    expect(actions.firstElementChild).toHaveTextContent("Thêm");
    expect(actions.lastElementChild).toHaveClass("slds-panel__close");
  });

  describe("focus (panels docs, Accessibility)", () => {
    it("does not take focus when the page loads with the panel already open", () => {
      render(
        <Page initial>
          <input aria-label="Tìm" />
        </Page>,
      );
      expect(document.activeElement).toBe(document.body);
    });

    it("moves focus to the first focusable that isn't the close button when opened", async () => {
      render(
        <Page>
          <input aria-label="Tìm" />
        </Page>,
      );
      await userEvent.click(screen.getByRole("button", { name: "Hiện bảng lọc" }));
      expect(document.activeElement).toBe(screen.getByRole("textbox", { name: "Tìm" }));
    });

    it("falls back to the close button when it is the only focusable", async () => {
      render(<Page>Không có điều khiển</Page>);
      await userEvent.click(screen.getByRole("button", { name: "Hiện bảng lọc" }));
      expect(document.activeElement).toBe(screen.getByRole("button", { name: "Thu gọn Bộ lọc" }));
    });

    it("returns focus to the trigger when closed from inside", async () => {
      render(
        <Page>
          <input aria-label="Tìm" />
        </Page>,
      );
      const trigger = screen.getByRole("button", { name: "Hiện bảng lọc" });
      await userEvent.click(trigger);
      await userEvent.click(screen.getByRole("button", { name: "Thu gọn Bộ lọc" }));
      expect(document.activeElement).toBe(trigger);
    });

    it("returns focus to the aria-controls trigger when it mounted open", async () => {
      render(
        <Page initial>
          <input aria-label="Tìm" />
        </Page>,
      );
      await userEvent.click(screen.getByRole("button", { name: "Thu gọn Bộ lọc" }));
      expect(document.activeElement).toBe(screen.getByRole("button", { name: "Hiện bảng lọc" }));
    });

    it("leaves focus alone when closed from the trigger itself", async () => {
      render(
        <Page>
          <input aria-label="Tìm" />
        </Page>,
      );
      const trigger = screen.getByRole("button", { name: "Hiện bảng lọc" });
      await userEvent.click(trigger);
      await userEvent.click(trigger);
      expect(document.activeElement).toBe(trigger);
    });
  });
});
