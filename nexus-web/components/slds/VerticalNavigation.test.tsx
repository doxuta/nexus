// VerticalNavigation — blueprint vertical-navigation/list (SLDS 2.264.1): sections with an h2 title
// the list is described by, the active item marked with slds-is-active + aria-current, items with
// icons, and a link that leaves for another screen in a new tab.
import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { Boxes, Plus, BarChart3 } from "lucide-react";
import { VerticalNavigation, type VerticalNavSection } from "./VerticalNavigation";

const SECTIONS: VerticalNavSection[] = [
  { id: "top", items: [{ key: "home", label: "Tổng quan", href: "/w?panel=tong-quan", active: true }] },
  {
    id: "data",
    title: "Dữ liệu",
    items: [
      { key: "acc", label: "Khách hàng", href: "/w?panel=bang&entity=acc", icon: Boxes },
      { key: "add", label: "Thêm bảng", href: "/w?panel=bang", icon: Plus, actionText: true },
    ],
  },
  { id: "empty", title: "Trống", items: [], note: "Chưa có mục nào." },
  { id: "kpi", title: "Số liệu", items: [{ key: "rep", label: "Báo cáo", href: "/reports", icon: BarChart3, external: true }] },
];

describe("VerticalNavigation", () => {
  it("renders the blueprint nav with one section per group, titled lists described by their h2", () => {
    const { container } = render(<VerticalNavigation label="Công cụ" sections={SECTIONS} compact shade />);
    const nav = screen.getByRole("navigation", { name: "Công cụ" });
    expect(nav).toHaveClass("slds-nav-vertical", "slds-nav-vertical_compact", "slds-nav-vertical_shade");
    expect(container.querySelectorAll(".slds-nav-vertical__section")).toHaveLength(4);
    const title = screen.getByRole("heading", { level: 2, name: "Dữ liệu" });
    expect(title).toHaveClass("slds-nav-vertical__title");
    expect(title.nextElementSibling).toHaveAttribute("aria-describedby", title.id);
  });

  it("marks the active item on the li and the link", () => {
    render(<VerticalNavigation label="Công cụ" sections={SECTIONS} />);
    const active = screen.getByRole("link", { name: "Tổng quan" });
    expect(active).toHaveAttribute("aria-current", "page");
    expect(active).toHaveClass("slds-nav-vertical__action");
    expect(active.closest("li")).toHaveClass("slds-nav-vertical__item", "slds-is-active");
    const other = screen.getByRole("link", { name: "Khách hàng" });
    expect(other).not.toHaveAttribute("aria-current");
    expect(other.closest("li")).not.toHaveClass("slds-is-active");
    expect(other).toHaveAttribute("href", "/w?panel=bang&entity=acc");
  });

  it("draws item icons as decorative Lucide glyphs and action-text items in link colour", () => {
    render(<VerticalNavigation label="Công cụ" sections={SECTIONS} />);
    const withIcon = screen.getByRole("link", { name: "Khách hàng" });
    expect(withIcon.querySelector(".slds-icon_container svg.nx-lucide")).not.toBeNull();
    expect(within(screen.getByRole("link", { name: "Thêm bảng" })).getByText("Thêm bảng")).toHaveClass("slds-nav-vertical__action-text");
  });

  // specs/073 kh6 (FR-023): no Nexus link opens another tab, in or out of a build session.
  it("an external item is a plain same-tab link: no target, no rel, no 'new tab' claim", () => {
    render(<VerticalNavigation label="Công cụ" sections={SECTIONS} />);
    const ext = screen.getByRole("link", { name: /Báo cáo/ });
    expect(ext).toHaveAttribute("href", "/reports");
    expect(ext).not.toHaveAttribute("target");
    expect(ext).not.toHaveAttribute("rel");
    expect(ext.textContent).not.toContain("tab mới");
  });

  it("shows a section note as weak text on a plain list item", () => {
    render(<VerticalNavigation label="Công cụ" sections={SECTIONS} />);
    const note = screen.getByText("Chưa có mục nào.");
    expect(note).toHaveClass("slds-text-color_weak");
    expect(note.closest("li")).not.toHaveClass("slds-nav-vertical__item");
  });
});
