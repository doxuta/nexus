// Accordion — blueprint accordion/base (SLDS 2.264.1); keyboard per WAI-ARIA accordion practices.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Accordion, type AccordionSection } from "./Accordion";

const SECTIONS: AccordionSection[] = [
  { id: "a", summary: "Thông tin chung", content: "Nội dung A" },
  { id: "b", summary: "Liên hệ", content: "Nội dung B" },
  { id: "c", summary: "Hợp đồng", content: "Nội dung C" },
];

const btn = (name: string) => screen.getByRole("button", { name });

describe("Accordion", () => {
  it("renders the blueprint list with the first section open by default", () => {
    const { container } = render(<Accordion sections={SECTIONS} />);
    const ul = container.firstElementChild!;
    expect(ul.tagName).toBe("UL");
    expect(ul).toHaveClass("slds-accordion");
    const items = Array.from(ul.children);
    expect(items).toHaveLength(3);
    for (const li of items) expect(li).toHaveClass("slds-accordion__list-item");
    const first = items[0].querySelector("section.slds-accordion__section")!;
    expect(first).toHaveClass("slds-is-open");
    const heading = first.querySelector(".slds-accordion__summary > h2.slds-accordion__summary-heading")!;
    const b = heading.querySelector("button")!;
    expect(b).toHaveClass("slds-button", "slds-button_reset", "slds-accordion__summary-action");
    expect(b).toHaveAttribute("title", "Thông tin chung");
    expect(b).toHaveAttribute("aria-expanded", "true");
    expect(b.querySelector("svg")).toHaveClass("slds-accordion__summary-action-icon", "slds-button__icon", "slds-button__icon_left");
    expect(b.querySelector("span.slds-accordion__summary-content")).toHaveTextContent("Thông tin chung");
    const content = first.querySelector(".slds-accordion__content")!;
    expect(b).toHaveAttribute("aria-controls", content.id);
    expect(content).not.toHaveAttribute("hidden");
    const second = items[1].querySelector("section")!;
    expect(second).not.toHaveClass("slds-is-open");
    expect(second.querySelector(".slds-accordion__content")).toHaveAttribute("hidden");
    expect(second.querySelector("button")).toHaveAttribute("aria-expanded", "false");
  });

  it("keeps one section open at a time by default, and can close the open one", async () => {
    const onOpenChange = vi.fn();
    render(<Accordion sections={SECTIONS} onOpenChange={onOpenChange} />);
    await userEvent.click(btn("Liên hệ"));
    expect(btn("Liên hệ")).toHaveAttribute("aria-expanded", "true");
    expect(btn("Thông tin chung")).toHaveAttribute("aria-expanded", "false");
    expect(onOpenChange).toHaveBeenLastCalledWith(["b"]);
    await userEvent.click(btn("Liên hệ"));
    expect(btn("Liên hệ")).toHaveAttribute("aria-expanded", "false");
    expect(onOpenChange).toHaveBeenLastCalledWith([]);
  });

  it("opens several sections at once with multiple", async () => {
    render(<Accordion sections={SECTIONS} multiple defaultOpen={[]} />);
    expect(btn("Thông tin chung")).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(btn("Thông tin chung"));
    await userEvent.click(btn("Hợp đồng"));
    expect(btn("Thông tin chung")).toHaveAttribute("aria-expanded", "true");
    expect(btn("Hợp đồng")).toHaveAttribute("aria-expanded", "true");
    expect(btn("Liên hệ")).toHaveAttribute("aria-expanded", "false");
  });

  it("toggles with Enter/Space and moves between headers with arrows, Home and End", async () => {
    render(<Accordion sections={SECTIONS} />);
    await userEvent.tab();
    expect(btn("Thông tin chung")).toHaveFocus();
    await userEvent.keyboard("{ArrowDown}");
    expect(btn("Liên hệ")).toHaveFocus();
    await userEvent.keyboard("{Enter}");
    expect(btn("Liên hệ")).toHaveAttribute("aria-expanded", "true");
    await userEvent.keyboard("{ArrowDown}{ArrowDown}");
    expect(btn("Thông tin chung")).toHaveFocus(); // wraps
    await userEvent.keyboard("{ArrowUp}");
    expect(btn("Hợp đồng")).toHaveFocus(); // wraps back
    await userEvent.keyboard(" ");
    expect(btn("Hợp đồng")).toHaveAttribute("aria-expanded", "true");
    await userEvent.keyboard("{Home}");
    expect(btn("Thông tin chung")).toHaveFocus();
    await userEvent.keyboard("{End}");
    expect(btn("Hợp đồng")).toHaveFocus();
  });

  it("keeps arrow keys inside its own level when accordions are nested", async () => {
    const nested: AccordionSection[] = [
      {
        id: "outer",
        summary: "Ngoài",
        content: (
          <Accordion
            sections={[
              { id: "in1", summary: "Trong 1", content: "x" },
              { id: "in2", summary: "Trong 2", content: "y" },
            ]}
          />
        ),
      },
      { id: "outer2", summary: "Ngoài 2", content: "z" },
    ];
    render(<Accordion sections={nested} />);
    btn("Ngoài").focus();
    await userEvent.keyboard("{ArrowDown}");
    expect(btn("Ngoài 2")).toHaveFocus();
    btn("Trong 1").focus();
    await userEvent.keyboard("{ArrowDown}");
    expect(btn("Trong 2")).toHaveFocus();
    await userEvent.keyboard("{ArrowDown}");
    expect(btn("Trong 1")).toHaveFocus();
  });

  it("renders a section's actions next to its heading", () => {
    const { container } = render(
      <Accordion sections={[{ id: "a", summary: "Có menu", content: "x", actions: <button type="button">Hiện thêm</button> }]} />,
    );
    const summary = container.querySelector(".slds-accordion__summary")!;
    expect(summary.lastElementChild).toHaveTextContent("Hiện thêm");
    expect(summary.firstElementChild).toHaveClass("slds-accordion__summary-heading");
  });
});
