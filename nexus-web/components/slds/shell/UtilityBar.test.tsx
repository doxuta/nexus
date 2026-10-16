// UtilityBar — blueprint docked-utility-bar/base (SLDS 2.264.1).
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { Sparkles } from "lucide-react";
import { UtilityBar } from "./UtilityBar";

describe("UtilityBar", () => {
  it("is a labelled footer with an assistive heading and one item per utility", () => {
    const { container } = render(
      <UtilityBar
        label="Thanh tiện ích"
        items={[{ key: "ai", label: "Hỏi AI về dự án này", icon: Sparkles, href: "/studio/du-an/crm/dung/du-lieu", title: "Mở trợ lý" }]}
      />,
    );
    const footer = container.querySelector("footer")!;
    expect(footer).toHaveClass("slds-utility-bar_container");
    expect(footer).toHaveAttribute("aria-label", "Thanh tiện ích");
    expect(footer.querySelector("h2.slds-assistive-text")).toHaveTextContent("Thanh tiện ích");
    // In flow under the workspace, not fixed to the viewport.
    expect(footer.querySelector("ul")).toHaveClass("slds-utility-bar", "static");
    const item = screen.getByRole("link", { name: "Hỏi AI về dự án này" });
    expect(item).toHaveAttribute("href", "/studio/du-an/crm/dung/du-lieu");
    expect(item).toHaveAttribute("title", "Mở trợ lý");
    expect(item).toHaveClass("slds-button", "slds-utility-bar__action");
    expect(item.closest("li")).toHaveClass("slds-utility-bar__item");
    expect(item.querySelector("svg.slds-button__icon.slds-button__icon_left")).not.toBeNull();
    expect(item.querySelector(".slds-utility-bar__text")).toHaveTextContent("Hỏi AI về dự án này");
  });
});
