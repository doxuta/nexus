// ExpandableSection — blueprint expandable-section/base (SLDS 2.264.1).
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ExpandableSection } from "./ExpandableSection";

describe("ExpandableSection", () => {
  it("renders the open blueprint markup with the button wired to its content", () => {
    const { container } = render(<ExpandableSection title="Thông tin liên hệ">Nội dung</ExpandableSection>);
    const section = container.firstElementChild!;
    expect(section).toHaveClass("slds-section", "slds-is-open");
    const h3 = section.querySelector("h3.slds-section__title")!;
    const btn = h3.querySelector("button")!;
    expect(btn).toHaveClass("slds-button", "slds-section__title-action");
    expect(btn).toHaveAttribute("type", "button");
    expect(btn).toHaveAttribute("aria-expanded", "true");
    expect(btn.querySelector("svg")).toHaveClass("slds-section__title-action-icon", "slds-button__icon", "slds-button__icon_left");
    expect(btn.querySelector("svg")).toHaveAttribute("aria-hidden", "true");
    const label = btn.querySelector("span.slds-truncate")!;
    expect(label).toHaveAttribute("title", "Thông tin liên hệ");
    const content = section.querySelector(".slds-section__content")!;
    expect(btn).toHaveAttribute("aria-controls", content.id);
    expect(content).not.toHaveAttribute("hidden");
    expect(content).toHaveTextContent("Nội dung");
  });

  it("closes and reopens from the keyboard (Enter and Space)", async () => {
    const onOpenChange = vi.fn();
    const { container } = render(
      <ExpandableSection title="Địa chỉ" onOpenChange={onOpenChange}>
        Nội dung
      </ExpandableSection>,
    );
    const btn = screen.getByRole("button", { name: "Địa chỉ" });
    const content = container.querySelector(".slds-section__content")!;
    await userEvent.tab();
    expect(btn).toHaveFocus();
    await userEvent.keyboard("{Enter}");
    expect(btn).toHaveAttribute("aria-expanded", "false");
    expect(content).toHaveAttribute("hidden");
    expect(container.firstElementChild).not.toHaveClass("slds-is-open");
    expect(onOpenChange).toHaveBeenLastCalledWith(false);
    await userEvent.keyboard(" ");
    expect(btn).toHaveAttribute("aria-expanded", "true");
    expect(content).not.toHaveAttribute("hidden");
    expect(onOpenChange).toHaveBeenLastCalledWith(true);
  });

  it("starts closed with defaultOpen=false", () => {
    const { container } = render(
      <ExpandableSection title="Ghi chú" defaultOpen={false}>
        x
      </ExpandableSection>,
    );
    expect(screen.getByRole("button", { name: "Ghi chú" })).toHaveAttribute("aria-expanded", "false");
    expect(container.querySelector(".slds-section__content")).toHaveAttribute("hidden");
  });

  it("follows a controlled open prop", async () => {
    const onOpenChange = vi.fn();
    const { rerender } = render(
      <ExpandableSection title="Hệ thống" open={false} onOpenChange={onOpenChange}>
        x
      </ExpandableSection>,
    );
    const btn = screen.getByRole("button", { name: "Hệ thống" });
    await userEvent.click(btn);
    expect(onOpenChange).toHaveBeenCalledWith(true);
    expect(btn).toHaveAttribute("aria-expanded", "false");
    rerender(
      <ExpandableSection title="Hệ thống" open onOpenChange={onOpenChange}>
        x
      </ExpandableSection>,
    );
    expect(btn).toHaveAttribute("aria-expanded", "true");
  });

  it("renders the non-collapsible variant without a button", () => {
    const { container } = render(
      <ExpandableSection title="Tóm tắt" collapsible={false}>
        x
      </ExpandableSection>,
    );
    expect(screen.queryByRole("button")).toBeNull();
    const h3 = container.querySelector("h3.slds-section__title")!;
    expect(h3).toHaveClass("slds-theme_shade");
    expect(h3.querySelector("span")).toHaveClass("slds-truncate", "slds-p-horizontal_small");
    expect(container.firstElementChild).toHaveClass("slds-section", "slds-is-open");
    expect(container.querySelector(".slds-section__content")).not.toHaveAttribute("hidden");
  });
});
