// ColorPicker — blueprint `color-picker` base (SLDS 2.264.1): summary (swatch button + hex input)
// and a selector popover with a "Mặc định" swatch tab and a "Tuỳ chỉnh" range/hue/hex/RGB tab.
import { describe, it, expect, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { ColorPicker } from "@/components/slds/ColorPicker";

function Harness({ initial = "#2e844a", onChange }: { initial?: string; onChange?: (v: string) => void }) {
  const [v, setV] = useState(initial);
  return (
    <ColorPicker
      label="Màu nhấn"
      value={v}
      placeholderColor="#0176d3"
      onChange={(next) => { setV(next); onChange?.(next); }}
    />
  );
}

describe("ColorPicker", () => {
  it("summary: label names the hex input; the swatch button shows and announces the current colour", () => {
    render(<Harness />);
    const input = screen.getByLabelText("Màu nhấn");
    expect(input).toHaveClass("slds-input");
    expect(input).toHaveValue("#2e844a");
    expect(input.closest(".slds-color-picker__summary-input")).not.toBeNull();
    const button = screen.getByRole("button", { name: /Màu hiện tại: #2e844a/ });
    expect(button).toHaveClass("slds-color-picker__summary-button", "slds-button_icon-more");
    expect(button.querySelector(".slds-swatch")).toHaveStyle({ background: "#2e844a" });
  });

  it("no value: swatch falls back to the placeholder colour and the input stays empty", () => {
    render(<ColorPicker label="Màu nhấn" value="" placeholderColor="#0176d3" onChange={() => {}} />);
    expect(screen.getByLabelText("Màu nhấn")).toHaveValue("");
    expect(screen.getByRole("button", { name: /Màu hiện tại: #0176d3/ })).toBeInTheDocument();
  });

  it("typing a valid hex commits it normalised; an invalid one shows the summary error on blur", async () => {
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    const input = screen.getByLabelText("Màu nhấn");
    await userEvent.clear(input);
    await userEvent.type(input, "#C80000");
    expect(onChange).toHaveBeenLastCalledWith("#c80000");

    await userEvent.clear(input);
    await userEvent.type(input, "#12");
    fireEvent.blur(input);
    expect(input).toHaveAttribute("aria-invalid", "true");
    expect(input.closest(".slds-form-element")).toHaveClass("slds-has-error");
    expect(screen.getByText("Mã màu phải có dạng #RRGGBB")).toHaveClass("slds-form-error");
  });

  it("clearing the input commits the empty value (back to the default colour)", async () => {
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    await userEvent.clear(screen.getByLabelText("Màu nhấn"));
    expect(onChange).toHaveBeenLastCalledWith("");
  });

  it("Mặc định tab: picking a swatch then Xong commits it; Huỷ keeps the old colour", async () => {
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    await userEvent.click(screen.getByRole("button", { name: /Màu hiện tại/ }));
    const dialog = screen.getByRole("dialog", { name: "Chọn màu" });
    expect(dialog).toHaveClass("slds-popover", "slds-color-picker__selector");
    expect(within(dialog).getByRole("tab", { name: "Mặc định" })).toHaveAttribute("aria-selected", "true");

    const list = within(dialog).getByRole("listbox", { name: "Màu có sẵn" });
    await userEvent.click(within(list).getByRole("option", { name: "#3be282" }));
    expect(within(list).getByRole("option", { name: "#3be282" })).toHaveAttribute("aria-selected", "true");
    await userEvent.click(within(dialog).getByRole("button", { name: "Xong" }));
    expect(onChange).toHaveBeenLastCalledWith("#3be282");
    expect(screen.queryByRole("dialog")).toBeNull();

    onChange.mockClear();
    await userEvent.click(screen.getByRole("button", { name: /Màu hiện tại/ }));
    await userEvent.click(within(screen.getByRole("listbox")).getByRole("option", { name: "#e3abec" }));
    await userEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(onChange).not.toHaveBeenCalled();
    expect(screen.getByLabelText("Màu nhấn")).toHaveValue("#3be282");
  });

  // Popover docs (SLDS 2.264.1): focus moves into the popover on open, the popover is a keyboard
  // focus trap, and focus returns to the trigger on close. The hex input sits between the trigger
  // and the popover in the DOM, so a trigger-kept focus used to Tab out of (and close) it.
  // happy-dom cannot compute Radix's tab order (tabIndex is -1 on native controls, its TreeWalker
  // does not descend into skipped nodes), so these tests pin the trap and drive the stops
  // explicitly; the real-browser Tab order is checked in the puppeteer acceptance run.
  it("keyboard: Enter opens with focus inside; Tab never leaves; a keyboard-picked swatch is committed by Xong", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    const trigger = screen.getByRole("button", { name: /Màu hiện tại/ });
    trigger.focus();
    await user.keyboard("{Enter}");
    const dialog = screen.getByRole("dialog", { name: "Chọn màu" });
    expect(dialog).toContainElement(document.activeElement as HTMLElement);
    await user.tab();
    expect(screen.getByRole("dialog")).toContainElement(document.activeElement as HTMLElement);

    // #2e844a is not a preset: the tab stop is the first swatch; the arrow moves to and picks the 2nd.
    const list = within(dialog).getByRole("listbox");
    list.querySelector<HTMLElement>("[role=option][tabindex='0']")!.focus();
    await user.keyboard("{ArrowRight}");
    expect(within(list).getByRole("option", { name: "#c2dbf7" })).toHaveAttribute("aria-selected", "true");

    const done = within(dialog).getByRole("button", { name: "Xong" });
    for (let i = 0; i < 20 && document.activeElement !== done; i++) {
      await user.tab();
      expect(screen.getByRole("dialog")).toContainElement(document.activeElement as HTMLElement);
    }
    expect(done).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(onChange).toHaveBeenLastCalledWith("#c2dbf7");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(trigger).toHaveFocus();
  });

  it("keyboard: Tab past Xong and Shift+Tab past the first tab keep the popover open with focus inside", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    screen.getByRole("button", { name: /Màu hiện tại/ }).focus();
    await user.keyboard("{Enter}");
    const dialog = screen.getByRole("dialog", { name: "Chọn màu" });
    within(dialog).getByRole("button", { name: "Xong" }).focus();
    await user.tab();
    expect(screen.getByRole("dialog")).toContainElement(document.activeElement as HTMLElement);

    within(dialog).getByRole("tab", { name: "Mặc định" }).focus();
    await user.tab({ shift: true });
    expect(screen.getByRole("dialog")).toContainElement(document.activeElement as HTMLElement);
  });

  it("swatches: arrow keys move the selection (roving tab stop)", async () => {
    render(<Harness />);
    await userEvent.click(screen.getByRole("button", { name: /Màu hiện tại/ }));
    const options = within(screen.getByRole("listbox")).getAllByRole("option");
    options[0].focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(options[1]).toHaveFocus();
    expect(options[1]).toHaveAttribute("aria-selected", "true");
    expect(options[1]).toHaveAttribute("tabindex", "0");
    expect(options[0]).toHaveAttribute("tabindex", "-1");
  });

  it("Tuỳ chỉnh tab: hex + RGB inputs mirror the draft; typing a hex updates RGB and Xong commits it", async () => {
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    await userEvent.click(screen.getByRole("button", { name: /Màu hiện tại/ }));
    await userEvent.click(screen.getByRole("tab", { name: "Tuỳ chỉnh" }));
    const hex = screen.getByLabelText("Hex");
    expect(hex).toHaveValue("#2e844a");
    expect(screen.getByLabelText("R")).toHaveValue("46");
    expect(screen.getByLabelText("G")).toHaveValue("132");
    expect(screen.getByLabelText("B")).toHaveValue("74");

    await userEvent.clear(hex);
    await userEvent.type(hex, "#c80000");
    expect(screen.getByLabelText("R")).toHaveValue("200");
    expect(screen.getByLabelText("G")).toHaveValue("0");
    await userEvent.click(screen.getByRole("button", { name: "Xong" }));
    expect(onChange).toHaveBeenLastCalledWith("#c80000");
  });

  it("Tuỳ chỉnh tab: an invalid hex marks the field and the custom error line", async () => {
    render(<Harness />);
    await userEvent.click(screen.getByRole("button", { name: /Màu hiện tại/ }));
    await userEvent.click(screen.getByRole("tab", { name: "Tuỳ chỉnh" }));
    const hex = screen.getByLabelText("Hex");
    await userEvent.clear(hex);
    await userEvent.type(hex, "zz");
    expect(hex).toHaveAttribute("aria-invalid", "true");
    expect(hex.closest(".slds-form-element")).toHaveClass("slds-has-error");
    expect(screen.getAllByText("Mã màu phải có dạng #RRGGBB").some((n) => n.classList.contains("slds-form-error"))).toBe(true);
  });

  it("Tuỳ chỉnh tab: the range indicator moves saturation/brightness with the arrow keys; the hue slider recolours the range", async () => {
    render(<Harness initial="#808080" />);
    await userEvent.click(screen.getByRole("button", { name: /Màu hiện tại/ }));
    await userEvent.click(screen.getByRole("tab", { name: "Tuỳ chỉnh" }));
    const indicator = document.querySelector<HTMLButtonElement>(".slds-color-picker__range-indicator")!;
    indicator.focus();
    await userEvent.keyboard("{ArrowUp}");
    // Grey #808080 = s 0 / v 50: up raises brightness only, still grey.
    expect(screen.getByLabelText("Hex")).toHaveValue("#828282");
    await userEvent.keyboard("{Shift>}{ArrowRight}{/Shift}");
    expect(screen.getByLabelText("Hex")).not.toHaveValue("#828282");

    fireEvent.change(screen.getByLabelText("Chọn sắc độ"), { target: { value: "120" } });
    expect(document.querySelector(".slds-color-picker__custom-range")).toHaveStyle({ background: "hsl(120, 100%, 50%)" });
  });
});
