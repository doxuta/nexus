// VerticalTabs — blueprint vertical-tabs/base (default, with-icons, with-badges), SLDS 2.264.1,
// behaviour from @radix-ui/react-tabs with orientation="vertical".
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Building2, Contact } from "lucide-react";
import { VerticalTabs } from "./VerticalTabs";

const items = [
  { value: "general", label: "Thông tin chung", content: "Nội dung chung" },
  { value: "contacts", label: "Liên hệ", content: "Nội dung liên hệ" },
  { value: "billing", label: "Thanh toán và hoá đơn điện tử", content: "Nội dung thanh toán" },
];

describe("VerticalTabs", () => {
  it("renders the blueprint: vertical tablist, nav items with left/label/right spans, show/hide panels", async () => {
    const { container } = render(<VerticalTabs items={items} label="Cài đặt" />);
    expect(container.firstElementChild!.className).toBe("slds-vertical-tabs");
    const list = screen.getByRole("tablist", { name: "Cài đặt" });
    expect(list.tagName).toBe("UL");
    expect(list).toHaveClass("slds-vertical-tabs__nav");
    expect(list).toHaveAttribute("aria-orientation", "vertical");

    const tabs = screen.getAllByRole("tab");
    expect(tabs).toHaveLength(3);
    const li = tabs[0].parentElement!;
    expect(li).toHaveClass("slds-vertical-tabs__nav-item", "slds-is-active");
    expect(li).toHaveAttribute("role", "presentation");
    expect(li).toHaveAttribute("title", "Thông tin chung");
    expect(tabs[0]).toHaveClass("slds-vertical-tabs__link");
    expect(tabs[0]).not.toHaveAttribute("href");
    const parts = Array.from(tabs[2].children);
    expect(parts.map((p) => p.className)).toEqual([
      "slds-vertical-tabs__left-icon",
      "slds-truncate",
      "slds-vertical-tabs__right-icon",
    ]);
    expect(parts[1]).toHaveAttribute("title", "Thanh toán và hoá đơn điện tử");

    const panels = container.querySelectorAll("[role=tabpanel]");
    expect(panels[0]).toHaveClass("slds-vertical-tabs__content", "slds-show");
    expect(panels[1]).toHaveClass("slds-vertical-tabs__content", "slds-hide");
    expect(panels[0]).toHaveAttribute("aria-labelledby", tabs[0].id);

    await userEvent.tab();
    expect(tabs[0]).toHaveFocus();
  });

  it("ArrowDown/ArrowUp move focus and selection", async () => {
    const onValueChange = vi.fn();
    render(<VerticalTabs items={items} onValueChange={onValueChange} />);
    const tabs = screen.getAllByRole("tab");
    tabs[0].focus();
    await userEvent.keyboard("{ArrowDown}");
    expect(tabs[1]).toHaveFocus();
    expect(tabs[1].parentElement).toHaveClass("slds-is-active");
    expect(tabs[0].parentElement).not.toHaveClass("slds-is-active");
    expect(screen.getByText("Nội dung liên hệ")).toHaveClass("slds-show");
    expect(onValueChange).toHaveBeenLastCalledWith("contacts");
    await userEvent.keyboard("{ArrowUp}");
    expect(tabs[0]).toHaveFocus();
    expect(tabs[0]).toHaveAttribute("aria-selected", "true");
  });

  it("puts an object icon on the left and a badge on the right", () => {
    render(
      <VerticalTabs
        defaultValue="contacts"
        items={[
          { ...items[0], icon: { glyph: Building2, name: "account" } },
          { ...items[1], icon: { glyph: Contact, name: "contact" }, badge: "Mới" },
        ]}
      />,
    );
    const tab = screen.getByRole("tab", { name: /Liên hệ/ });
    expect(tab).toHaveAttribute("aria-selected", "true");
    expect(tab.querySelector(".slds-vertical-tabs__left-icon .slds-icon_container")).toHaveClass("slds-icon-standard-contact");
    expect(tab.querySelector(".slds-vertical-tabs__left-icon svg")).toHaveClass("slds-icon", "slds-icon_small");
    const badge = tab.querySelector(".slds-vertical-tabs__right-icon > .slds-badge")!;
    expect(badge).toHaveClass("slds-badge_inverse");
    expect(badge).toHaveTextContent("Mới");
  });
});
