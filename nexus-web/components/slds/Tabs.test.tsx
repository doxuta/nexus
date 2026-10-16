// Tabs — blueprints tabs/base (default, overflow, error, sizes) and scoped-tabs/base, SLDS 2.264.1,
// behaviour from @radix-ui/react-tabs (+ react-dropdown-menu for the overflow tab).
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Ticket } from "lucide-react";
import { useState } from "react";
import { fakeRowLayout } from "@/test/fakeRowLayout";
import { Tabs, type TabItem, type TabLink } from "./Tabs";

const items: TabItem[] = [
  { value: "details", label: "Chi tiết", content: "Nội dung chi tiết" },
  { value: "related", label: "Liên quan", content: "Nội dung liên quan" },
  { value: "files", label: "Tệp", content: "Nội dung tệp" },
  { value: "history", label: "Lịch sử", content: "Nội dung lịch sử" },
  { value: "approvals", label: "Duyệt", content: "Nội dung duyệt" },
];

describe("Tabs", () => {
  it("renders the default blueprint: tablist ul, presentation li, anchor tabs, show/hide panels", async () => {
    const { container } = render(<Tabs items={items.slice(0, 3)} label="Hồ sơ" />);
    const root = container.firstElementChild!;
    expect(root.className).toBe("slds-tabs_default");

    const list = screen.getByRole("tablist", { name: "Hồ sơ" });
    expect(list.tagName).toBe("UL");
    expect(list).toHaveClass("slds-tabs_default__nav");

    const tabs = screen.getAllByRole("tab");
    expect(tabs).toHaveLength(3);
    tabs.forEach((tab, i) => {
      expect(tab.tagName).toBe("A");
      expect(tab).toHaveClass("slds-tabs_default__link");
      expect(tab).not.toHaveAttribute("href");
      const li = tab.parentElement!;
      expect(li).toHaveClass("slds-tabs_default__item");
      expect(li).toHaveAttribute("role", "presentation");
      expect(li).toHaveAttribute("title", items[i].label);
    });
    expect(tabs[0]).toHaveAttribute("aria-selected", "true");
    expect(tabs[1]).toHaveAttribute("aria-selected", "false");
    // Tab from before the list lands on the selected tab, which becomes the only tab stop.
    await userEvent.tab();
    expect(tabs[0]).toHaveFocus();
    expect(tabs[0]).toHaveAttribute("tabindex", "0");
    expect(tabs[1]).toHaveAttribute("tabindex", "-1");
    expect(tabs[0].parentElement).toHaveClass("slds-is-active");
    expect(tabs[1].parentElement).not.toHaveClass("slds-is-active");

    // Every panel is in the DOM like the blueprint; only the active one is shown.
    const panels = container.querySelectorAll("[role=tabpanel]");
    expect(panels).toHaveLength(3);
    expect(panels[0]).toHaveClass("slds-tabs_default__content", "slds-show");
    expect(panels[1]).toHaveClass("slds-tabs_default__content", "slds-hide");
    expect(panels[0]).toHaveAttribute("aria-labelledby", tabs[0].id);
    expect(tabs[0]).toHaveAttribute("aria-controls", panels[0].id);
  });

  it("scoped variant swaps the class prefix; size adds the modifier", () => {
    const { container } = render(<Tabs variant="scoped" size="medium" items={items.slice(0, 2)} />);
    expect(container.firstElementChild).toHaveClass("slds-tabs_scoped", "slds-tabs_medium");
    expect(screen.getByRole("tablist")).toHaveClass("slds-tabs_scoped__nav");
    const tab = screen.getAllByRole("tab")[0];
    expect(tab).toHaveClass("slds-tabs_scoped__link");
    expect(tab.parentElement).toHaveClass("slds-tabs_scoped__item", "slds-is-active");
    expect(container.querySelector("[role=tabpanel]")).toHaveClass("slds-tabs_scoped__content", "slds-show");
  });

  it("arrow keys move focus and selection; click selects", async () => {
    const onValueChange = vi.fn();
    render(<Tabs items={items.slice(0, 3)} onValueChange={onValueChange} />);
    const tabs = screen.getAllByRole("tab");
    tabs[0].focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(tabs[1]).toHaveFocus();
    expect(tabs[1]).toHaveAttribute("aria-selected", "true");
    expect(tabs[1].parentElement).toHaveClass("slds-is-active");
    expect(screen.getByText("Nội dung liên quan")).toHaveClass("slds-show");
    expect(screen.getByText("Nội dung chi tiết")).toHaveClass("slds-hide");
    expect(onValueChange).toHaveBeenLastCalledWith("related");

    await userEvent.keyboard("{ArrowLeft}{ArrowLeft}");
    expect(tabs[2]).toHaveFocus(); // wraps around
    expect(tabs[2]).toHaveAttribute("aria-selected", "true");

    await userEvent.click(tabs[0]);
    expect(tabs[0]).toHaveAttribute("aria-selected", "true");
    expect(onValueChange).toHaveBeenLastCalledWith("details");
  });

  it("follows a controlled value", async () => {
    function Controlled() {
      const [value, setValue] = useState("files");
      return <Tabs items={items.slice(0, 3)} value={value} onValueChange={setValue} />;
    }
    render(<Controlled />);
    expect(screen.getByRole("tab", { name: "Tệp" })).toHaveAttribute("aria-selected", "true");
    await userEvent.click(screen.getByRole("tab", { name: "Chi tiết" }));
    expect(screen.getByRole("tab", { name: "Chi tiết" })).toHaveAttribute("aria-selected", "true");
  });

  it("shows the error icon with its assistive text inside the tab", () => {
    render(<Tabs items={[items[0], { ...items[1], error: "Tab này có lỗi" }]} />);
    const tab = screen.getByRole("tab", { name: /Liên quan/ });
    const right = tab.querySelector(".slds-tabs__right-icon .slds-icon_container")!;
    expect(right).toHaveClass("slds-icon-utility-error");
    expect(right).toHaveAttribute("title", "Tab này có lỗi");
    expect(right.querySelector("svg")).toHaveClass("slds-icon", "slds-icon_x-small", "slds-icon-text-error");
    expect(within(tab).getByText("Tab này có lỗi")).toHaveClass("slds-assistive-text");
  });

  it("puts tabs past maxVisible into the 'Thêm' overflow menu and brings the chosen one back", async () => {
    render(<Tabs items={items} maxVisible={3} />);
    const more = screen.getByRole("tab", { name: "Thêm" });
    expect(screen.getAllByRole("tab")).toHaveLength(4);
    expect(more.tagName).toBe("BUTTON");
    expect(more).toHaveClass("slds-button");
    expect(more).toHaveAttribute("aria-haspopup", "menu");
    const li = more.closest("li")!;
    expect(li).toHaveClass("slds-tabs_default__item", "slds-tabs_default__overflow-button");
    expect(li).toHaveAttribute("title", "Thêm tab");

    more.focus();
    await userEvent.keyboard("{Enter}");
    const menu = screen.getByRole("menu");
    expect(menu).toHaveClass("slds-dropdown", "slds-dropdown_right");
    expect(more.closest(".slds-dropdown-trigger")).toHaveClass("slds-dropdown-trigger_click", "slds-is-open");
    expect(menu.firstElementChild).toHaveClass("slds-dropdown__list", "slds-dropdown_length-with-icon-10");
    const entries = within(menu).getAllByRole("menuitem");
    expect(entries.map((e) => e.textContent)).toEqual(["Lịch sử", "Duyệt"]);
    expect(entries[0].parentElement).toHaveClass("slds-dropdown__item");

    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("menu")).toBeNull();
    expect(more).toHaveFocus();

    await userEvent.keyboard("{Enter}");
    await userEvent.keyboard("{ArrowDown}{Enter}");
    // "Duyệt" is now selected and takes the last visible slot; "Tệp" moves into the menu.
    const selected = screen.getByRole("tab", { name: "Duyệt" });
    expect(selected).toHaveAttribute("aria-selected", "true");
    expect(selected).toHaveFocus();
    expect(screen.queryByRole("tab", { name: "Tệp" })).toBeNull();
    expect(screen.getByText("Nội dung duyệt")).toHaveClass("slds-show");
  });

  it("overflow-icons: a menu item with an icon is the blueprint media object with the object tile", async () => {
    const withIcons: TabItem[] = [
      ...items.slice(0, 3),
      { ...items[3], icon: { glyph: Ticket, name: "case" } },
      items[4],
    ];
    render(<Tabs items={withIcons} maxVisible={3} />);
    screen.getByRole("tab", { name: "Thêm" }).focus();
    await userEvent.keyboard("{Enter}");
    const [iconEntry, plainEntry] = within(screen.getByRole("menu")).getAllByRole("menuitem");

    // a[role=menuitem] > span[title] > span.slds-media.slds-media_center > figure + body
    const title = iconEntry.firstElementChild!;
    expect(title.tagName).toBe("SPAN");
    expect(title).toHaveAttribute("title", "Lịch sử");
    const media = title.firstElementChild!;
    expect(media).toHaveClass("slds-media", "slds-media_center");
    const [figure, body] = Array.from(media.children);
    expect(figure).toHaveClass("slds-media__figure");
    const tile = figure.firstElementChild!;
    expect(tile).toHaveClass("slds-icon_container", "slds-icon-standard-case");
    expect(tile.querySelector("svg")).toHaveClass("slds-icon", "slds-icon_small");
    expect(body).toHaveClass("slds-media__body");
    expect(body).toHaveTextContent("Lịch sử");

    // No icon: the plain overflow markup.
    expect(plainEntry.innerHTML).toBe('<span title="Duyệt">Duyệt</span>');
  });
});

// maxVisible="auto": the tabs that do not fit the nav's width go into "Thêm" (blueprint overflow),
// measured — no guessed count. Every cell is 100px wide here (fakeRowLayout).
describe("Tabs — maxVisible='auto'", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it("a narrow nav keeps what fits plus 'Thêm'; the last tab is reachable from the menu", async () => {
    fakeRowLayout(350); // "Thêm" 100 + 2 tabs × 100 fit, a third does not
    render(<Tabs items={items} maxVisible="auto" label="Hồ sơ" />);
    expect(screen.getAllByRole("tab").map((tab) => tab.textContent)).toEqual(["Chi tiết", "Liên quan", "Thêm"]);

    await userEvent.click(screen.getByRole("tab", { name: "Thêm" }));
    const entries = within(screen.getByRole("menu")).getAllByRole("menuitem");
    expect(entries.map((e) => e.textContent)).toEqual(["Tệp", "Lịch sử", "Duyệt"]);
    await userEvent.click(entries[2]);
    expect(screen.getByRole("tab", { name: "Duyệt" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByText("Nội dung duyệt")).toHaveClass("slds-show");
  });

  it("a wide nav shows every tab and no 'Thêm'", () => {
    fakeRowLayout(1000);
    render(<Tabs items={items} maxVisible="auto" />);
    expect(screen.getAllByRole("tab")).toHaveLength(items.length);
    expect(screen.queryByRole("tab", { name: "Thêm" })).toBeNull();
  });

  it("never overflows the selected tab, even when only 'Thêm' fits", () => {
    fakeRowLayout(120);
    render(<Tabs items={items} maxVisible="auto" defaultValue="history" />);
    expect(screen.getAllByRole("tab").map((tab) => tab.textContent)).toEqual(["Lịch sử", "Thêm"]);
  });
});

// specs/073 T051: chế độ liên kết — nav of links, not a tablist.
describe("Tabs mode=links", () => {
  const links: TabLink[] = [
    { value: "tables", label: "Bảng", href: "/studio/p/1/data" },
    { value: "fields", label: "Trường", href: "/studio/p/1/data/fields", dot: true },
    { value: "links", label: "Quan hệ", href: "/studio/p/1/data/links" },
  ];

  it("renders nav > ul > li > a with aria-current=page, no tablist and no panels", () => {
    const { container } = render(<Tabs mode="links" label="Mục trong khu" items={links} current="fields" size="medium" />);
    const nav = screen.getByRole("navigation", { name: "Mục trong khu" });
    expect(nav.tagName).toBe("NAV");
    expect(nav).toHaveClass("slds-tabs_default", "slds-tabs_medium", "nx-tabs-zone");
    expect(nav.querySelector("ul.slds-tabs_default__nav")).not.toBeNull();
    expect(screen.queryByRole("tablist")).toBeNull();
    expect(screen.queryByRole("tab")).toBeNull();
    expect(screen.queryByRole("tabpanel")).toBeNull();
    expect(container.querySelector(".slds-tabs_default__content")).toBeNull();
    const as = screen.getAllByRole("link");
    expect(as.map((a) => a.getAttribute("href"))).toEqual(links.map((l) => l.href));
    expect(as.filter((a) => a.getAttribute("aria-current") === "page")).toEqual([screen.getByRole("link", { name: /Trường/ })]);
    expect(screen.getByRole("link", { name: /Trường/ }).closest("li")).toHaveClass("slds-tabs_default__item", "slds-is-active");
  });

  it("a dot item carries hidden text; others do not", () => {
    render(<Tabs mode="links" label="Mục trong khu" items={links} current="tables" />);
    expect(screen.getByRole("link", { name: /Trường/ })).toHaveTextContent("có việc chờ");
    expect(screen.getByRole("link", { name: /Bảng/ })).not.toHaveTextContent("có việc chờ");
  });

  it("scrolls the current item into view inside its own list, not the window", () => {
    const rect = (left: number, right: number) => ({ left, right, top: 0, bottom: 0, width: right - left, height: 0, x: left, y: 0, toJSON() {} }) as DOMRect;
    const spy = vi.spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (this: Element) {
      return this.tagName === "UL" ? rect(0, 100) : rect(150, 250);
    });
    render(<Tabs mode="links" label="Khu" items={links} current="links" />);
    const ul = screen.getByRole("link", { name: /Bảng/ }).closest("ul")!;
    expect(ul).toHaveClass("overflow-x-auto");
    expect(ul.scrollLeft).toBe(150);
    spy.mockRestore();
  });
});
