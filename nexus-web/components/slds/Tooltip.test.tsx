// Tooltip — blueprint tooltips/base (SLDS 2.264.1) on @radix-ui/react-tooltip.
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { nubbin } from "./Popover";
import { Tooltip } from "./Tooltip";

const trigger = (
  <button type="button" className="slds-button slds-button_icon" title="Trợ giúp">
    <span className="slds-assistive-text">Trợ giúp</span>
  </button>
);

describe("Tooltip", () => {
  it("opens on keyboard focus with the blueprint bubble and describes the trigger", async () => {
    render(<Tooltip trigger={trigger}>Mã số thuế gồm 10 hoặc 13 chữ số.</Tooltip>);
    expect(screen.queryByRole("tooltip")).toBeNull();
    await userEvent.tab();
    const btn = screen.getByRole("button", { name: "Trợ giúp" });
    expect(btn).toHaveFocus();

    const tip = await screen.findByRole("tooltip");
    expect(tip).toHaveTextContent("Mã số thuế gồm 10 hoặc 13 chữ số.");
    expect(btn).toHaveAttribute("aria-describedby", tip.id);

    const bubble = tip.closest(".slds-popover")!;
    expect(bubble).toHaveClass("slds-popover_tooltip");
    // Nubbin follows the placement Radix reports (blueprint default: top/start → nubbin bottom-left).
    const el = bubble as HTMLElement;
    expect(bubble).toHaveClass(nubbin(el.dataset.side as "top", el.dataset.align as "start", "").className);
    const body = bubble.querySelector(".slds-popover__body")!;
    expect(body).toHaveTextContent("Mã số thuế gồm 10 hoặc 13 chữ số.");
    // The visible copy is hidden from AT: Radix's role=tooltip copy is what gets announced, once.
    expect(body).toHaveAttribute("aria-hidden", "true");
  });

  it("closes on Escape", async () => {
    render(<Tooltip trigger={trigger}>Gợi ý</Tooltip>);
    await userEvent.tab();
    await screen.findByRole("tooltip");
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("stays open while controlled open", () => {
    render(
      <Tooltip trigger={trigger} open side="bottom" align="center">
        Gợi ý
      </Tooltip>,
    );
    const bubble = screen.getByRole("tooltip").closest(".slds-popover") as HTMLElement;
    expect(bubble).toHaveClass(nubbin(bubble.dataset.side as "bottom", bubble.dataset.align as "center", "").className);
  });
});
