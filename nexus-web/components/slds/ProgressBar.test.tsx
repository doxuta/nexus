// ProgressBar — blueprint `progress-bar` (base, vertical), SLDS 2.264.1.
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { ProgressBar } from "@/components/slds/ProgressBar";

describe("ProgressBar", () => {
  it("base: div.slds-progress-bar[role=progressbar] with the value span and assistive text", () => {
    render(<ProgressBar value={25} label="Tiến độ nhập dữ liệu" />);
    const bar = screen.getByRole("progressbar", { name: "Tiến độ nhập dữ liệu" });
    expect(bar.className).toBe("slds-progress-bar");
    expect(bar).toHaveAttribute("aria-valuemin", "0");
    expect(bar).toHaveAttribute("aria-valuemax", "100");
    expect(bar).toHaveAttribute("aria-valuenow", "25");
    const fill = bar.firstElementChild as HTMLElement;
    expect(fill.className).toBe("slds-progress-bar__value");
    expect(fill.style.width).toBe("25%");
    expect(fill.querySelector(".slds-assistive-text")).toHaveTextContent("Tiến độ: 25%");
  });

  it("clamps out-of-range values to 0–100", () => {
    const { rerender } = render(<ProgressBar value={140} />);
    expect(screen.getByRole("progressbar")).toHaveAttribute("aria-valuenow", "100");
    rerender(<ProgressBar value={-5} />);
    expect(screen.getByRole("progressbar")).toHaveAttribute("aria-valuenow", "0");
    expect((screen.getByRole("progressbar").firstElementChild as HTMLElement).style.width).toBe("0%");
  });

  it("thickness, circular and success colour", () => {
    render(<ProgressBar value={100} size="large" circular success />);
    const bar = screen.getByRole("progressbar");
    expect(bar).toHaveClass("slds-progress-bar", "slds-progress-bar_large", "slds-progress-bar_circular");
    expect(bar.firstElementChild).toHaveClass("slds-progress-bar__value", "slds-progress-bar__value_success");
  });

  it("vertical fills by height", () => {
    render(<ProgressBar value={75} vertical />);
    const bar = screen.getByRole("progressbar");
    expect(bar).toHaveClass("slds-progress-bar_vertical");
    const fill = bar.firstElementChild as HTMLElement;
    expect(fill.style.height).toBe("75%");
    expect(fill.style.width).toBe("");
  });

  it("descriptive: the title row names the bar via aria-labelledby; the % text is hidden from AT", () => {
    render(<ProgressBar value={25} title="Trợ lý cài đặt" />);
    const bar = screen.getByRole("progressbar", { name: "Trợ lý cài đặt" });
    const head = document.getElementById(bar.getAttribute("aria-labelledby")!)!;
    expect(head).toHaveClass("slds-grid", "slds-grid_align-spread", "slds-p-bottom_x-small");
    const pct = head.querySelector("[aria-hidden=true]")!;
    expect(pct.querySelector("strong")).toHaveTextContent("Hoàn thành 25%");
  });
});
