// Breadcrumbs — blueprint breadcrumbs/base (SLDS 2.264.1).
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { Breadcrumbs } from "./Breadcrumbs";

describe("Breadcrumbs", () => {
  it("renders the blueprint nav > ol.slds-breadcrumb > li.slds-breadcrumb__item > a", () => {
    render(
      <Breadcrumbs
        items={[
          { label: "Khách hàng", href: "/crm_accounts" },
          { label: "Công ty Một", href: "/crm_accounts/1" },
        ]}
      />,
    );
    const nav = screen.getByRole("navigation", { name: "Đường dẫn điều hướng" });
    const ol = nav.firstElementChild!;
    expect(ol.tagName).toBe("OL");
    expect(ol).toHaveClass("slds-breadcrumb", "slds-list_horizontal", "slds-wrap");
    const items = Array.from(ol.children);
    expect(items).toHaveLength(2);
    items.forEach((li) => expect(li).toHaveClass("slds-breadcrumb__item"));
    expect(screen.getByRole("link", { name: "Công ty Một" })).toHaveAttribute("href", "/crm_accounts/1");
  });

  it("takes a caller label for the landmark", () => {
    render(<Breadcrumbs label="Vị trí bản ghi" items={[{ label: "Khách hàng", href: "/a" }]} />);
    expect(screen.getByRole("navigation", { name: "Vị trí bản ghi" })).toBeInTheDocument();
  });
});
