// BuilderHeader / BuilderToolbar — blueprint builder-header/base + toolbar (SLDS 2.264.1).
import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { AppWindow, Home, Rocket, Wrench } from "lucide-react";
import { BuilderHeader, BuilderToolbar } from "./BuilderHeader";

function renderHeader(over: Partial<Parameters<typeof BuilderHeader>[0]> = {}) {
  return render(
    <BuilderHeader
      back={{ href: "/studio", label: "Lên cấp trên" }}
      appName="Nexus Studio"
      appIcon={Wrench}
      appHref="/studio"
      navLabel="Điều hướng Studio"
      nav={[
        { label: "Danh sách dự án", href: "/studio", icon: Home, current: "page" },
        { label: "Cài mẫu & Bàn giao", href: "/studio/verticals", icon: Rocket },
      ]}
      title="Tổng quan"
      utilities={[{ label: "App khách", href: "/home", icon: AppWindow }]}
      {...over}
    />,
  );
}

describe("BuilderHeader", () => {
  it("keeps the blueprint container/header/items, in flow instead of fixed", () => {
    const { container } = renderHeader();
    const root = container.firstElementChild!;
    expect(root).toHaveClass("slds-builder-header_container", "static");
    const header = root.querySelector("header.slds-builder-header")!;
    expect([...header.children].filter((c) => c.classList.contains("slds-builder-header__item"))).toHaveLength(5);
  });

  it("renders the back link with assistive text", () => {
    renderHeader();
    const back = screen.getByRole("link", { name: "Lên cấp trên" });
    expect(back).toHaveAttribute("href", "/studio");
    expect(back).toHaveClass("slds-builder-header__item-action");
  });

  it("omits the back item without a target", () => {
    const { container } = renderHeader({ back: undefined });
    expect(screen.queryByRole("link", { name: "Lên cấp trên" })).toBeNull();
    expect(container.querySelector("header")!.children).toHaveLength(4);
  });

  it("links the app name and lists the nav items, marking the current one", () => {
    renderHeader();
    expect(screen.getByRole("link", { name: "Nexus Studio" })).toHaveAttribute("href", "/studio");
    const nav = screen.getByRole("navigation", { name: "Điều hướng Studio" });
    expect(nav).toHaveClass("slds-builder-header__item", "slds-builder-header__nav");
    const items = within(nav).getAllByRole("link");
    expect(items.map((a) => a.getAttribute("href"))).toEqual(["/studio", "/studio/verticals"]);
    expect(items[0]).toHaveAttribute("aria-current", "page");
    expect(items[1]).not.toHaveAttribute("aria-current");
    expect(items[0].closest("li")).toHaveClass("slds-builder-header__nav-item");
  });

  it("says aria-current=true, not page, for the section the open page sits in", () => {
    renderHeader({
      nav: [
        { label: "Danh sách dự án", href: "/studio", icon: Home, current: "true" },
        { label: "Cài mẫu & Bàn giao", href: "/studio/verticals", icon: Rocket },
      ],
    });
    const items = within(screen.getByRole("navigation", { name: "Điều hướng Studio" })).getAllByRole("link");
    expect(items[0]).toHaveAttribute("aria-current", "true");
    expect(items[1]).not.toHaveAttribute("aria-current");
  });

  it("puts the page title in the flexi-truncate h1 and the utilities on the right", () => {
    renderHeader({ titleSuffix: <span>· cho app CRM</span> });
    const h1 = screen.getByRole("heading", { level: 1 });
    expect(h1).toHaveClass("slds-builder-header__item-label");
    expect(h1).toHaveTextContent("Tổng quan· cho app CRM");
    expect(h1.parentElement).toHaveClass("slds-has-flexi-truncate");
    const util = screen.getByRole("link", { name: "App khách" });
    expect(util).toHaveAttribute("href", "/home");
    expect(util.closest(".slds-builder-header__utilities-item")).not.toBeNull();
  });
});

describe("BuilderToolbar", () => {
  it("is a named toolbar with item groups and document actions", () => {
    render(
      <BuilderToolbar label="Thao tác dự án" actions={<button type="button">Bàn giao</button>}>
        <span>CRM</span>
      </BuilderToolbar>,
    );
    const bar = screen.getByRole("toolbar", { name: "Thao tác dự án" });
    expect(bar).toHaveClass("slds-builder-toolbar");
    expect(bar.querySelector(".slds-builder-toolbar__item-group")).toHaveTextContent("CRM");
    expect(bar.querySelector(".slds-builder-toolbar__actions")).toHaveTextContent("Bàn giao");
  });
});
