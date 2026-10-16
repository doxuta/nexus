// ObjectHomePage — list-view page template (page-headers/object-home inside a card + body + filter panel).
import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Building2 } from "lucide-react";
import { useState } from "react";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { ObjectHomePage, type ListDensity, type ListViewMode } from "./ObjectHomePage";

const icon = { glyph: Building2, name: "account" };

describe("ObjectHomePage", () => {
  it("puts every slot in its object-home region", () => {
    const { container } = render(
      <ObjectHomePage
        icon={icon}
        label="Khách hàng"
        title="Tất cả khách hàng"
        titleMenu={<button type="button">Đổi danh sách</button>}
        meta="12 mục · cập nhật 3 phút trước"
        controls={[<button key="r" type="button">Làm mới</button>]}
        filterPanel={<aside data-testid="filters">Bộ lọc</aside>}
      >
        <table data-testid="body" />
      </ObjectHomePage>,
    );
    // Blueprint page-headers/object-home "Inside a card": the card flattens the header, so header and
    // list meet on one line (the body's), not on the doubled border of two magnet-joined boxes.
    const card = container.firstElementChild!;
    expect(card.tagName).toBe("ARTICLE");
    expect(card).toHaveClass("slds-card");
    const header = card.firstElementChild!;
    expect(header).toHaveClass("slds-page-header");
    expect(container.querySelector(".slds-has-bottom-magnet, .slds-has-top-magnet")).toBeNull();
    expect(within(header as HTMLElement).getByRole("heading", { level: 1 })).toHaveTextContent("Khách hàngTất cả khách hàng");
    expect(screen.getByRole("button", { name: "Đổi danh sách" }).closest(".slds-page-header__name-switcher")).not.toBeNull();
    expect(screen.getByText("12 mục · cập nhật 3 phút trước")).toHaveClass("slds-page-header__meta-text");
    expect(screen.getByRole("button", { name: "Làm mới" }).closest(".slds-page-header__col-controls")).not.toBeNull();

    expect(header.nextElementSibling).toHaveClass("slds-grid");
    const body = screen.getByTestId("body");
    const filters = screen.getByTestId("filters");
    // Body column first, filter panel docked to its right in the same flex row.
    expect(body.closest(".slds-col")!.parentElement).toBe(filters.parentElement);
    expect(filters.parentElement).toHaveClass("slds-grid");
    expect(filters.previousElementSibling).toBe(body.closest(".slds-col"));
  });

  it("draws its own slds-card alone, and none inside an EmbeddedSurface (the surface is the one card)", () => {
    const page = (
      <ObjectHomePage icon={icon} label="Khách hàng" title="Tất cả">
        <table />
      </ObjectHomePage>
    );
    const alone = render(page);
    expect(alone.container.querySelectorAll(".slds-card")).toHaveLength(1);
    alone.unmount();
    const embedded = render(<EmbeddedSurface>{page}</EmbeddedSurface>);
    expect(embedded.container.querySelectorAll(".slds-card")).toHaveLength(0);
  });

  it("renders the empty slot instead of the body", () => {
    render(
      <ObjectHomePage icon={icon} label="Khách hàng" title="Tất cả" empty={<p>Chưa có khách hàng</p>}>
        <table data-testid="body" />
      </ObjectHomePage>,
    );
    expect(screen.getByText("Chưa có khách hàng")).toBeInTheDocument();
    expect(screen.queryByTestId("body")).toBeNull();
    // No table to draw the header/list seam: the column draws the 1px line itself.
    expect(screen.getByText("Chưa có khách hàng").closest(".slds-col")).toHaveClass("slds-border_top");
  });

  it("keeps the body when empty is a falsy `rows.length === 0 && …` expression", () => {
    const rows = [1];
    render(
      <ObjectHomePage icon={icon} label="Khách hàng" title="Tất cả" empty={rows.length === 0 && <p>Chưa có khách hàng</p>}>
        <table data-testid="body" />
      </ObjectHomePage>,
    );
    expect(screen.getByTestId("body")).toBeInTheDocument();
    // The body brings its own top border (slds-table_bordered); a second one would double the seam.
    expect(screen.getByTestId("body").closest(".slds-col")).not.toHaveClass("slds-border_top");
  });

  it("renders typed actions as a button group with the rest in an overflow menu", async () => {
    const onNew = vi.fn();
    const onTrash = vi.fn();
    render(
      <ObjectHomePage
        icon={icon}
        label="Khách hàng"
        title="Tất cả"
        actions={[
          { label: "Mới", onSelect: onNew },
          { label: "Nhập", onSelect: () => {} },
          { label: "Thùng rác", onSelect: onTrash },
        ]}
        maxVisibleActions={2}
      />,
    );
    const newBtn = screen.getByRole("button", { name: "Mới" });
    expect(newBtn).toHaveClass("slds-button", "slds-button_neutral");
    expect(newBtn.closest("ul")).toHaveClass("slds-button-group-list");
    expect(newBtn.closest(".slds-page-header__col-actions")).not.toBeNull();
    await userEvent.click(newBtn);
    expect(onNew).toHaveBeenCalledOnce();

    expect(screen.queryByRole("button", { name: "Thùng rác" })).toBeNull();
    const more = screen.getByRole("button", { name: "Thêm thao tác" });
    expect(more).toHaveClass("slds-button_icon", "slds-button_icon-border-filled");
    more.focus();
    await userEvent.keyboard("{Enter}");
    expect(screen.getByRole("menuitem", { name: "Thùng rác" })).toHaveFocus();
    await userEvent.keyboard("{Enter}");
    expect(onTrash).toHaveBeenCalledOnce();
  });

  it("an action whose label changes (busy text) stays the same button, so keyboard focus stays on it", () => {
    const { rerender } = render(
      <ObjectHomePage icon={icon} label="Khách hàng" title="Tất cả" actions={[{ label: "Xuất CSV", onSelect: () => {} }]} />,
    );
    const btn = screen.getByRole("button", { name: "Xuất CSV" });
    btn.focus();
    rerender(
      <ObjectHomePage icon={icon} label="Khách hàng" title="Tất cả" actions={[{ label: "Đang xuất…", onSelect: () => {} }]} />,
    );
    expect(screen.getByRole("button", { name: "Đang xuất…" })).toBe(btn);
    expect(btn).toHaveFocus();
  });

  it("a busy action the browser blurred when it turned disabled gets focus back when it is enabled again", () => {
    const page = (disabled: boolean) => (
      <ObjectHomePage icon={icon} label="Khách hàng" title="Tất cả" actions={[{ label: "Xuất CSV", onSelect: () => {}, disabled }]} />
    );
    const { rerender } = render(page(false));
    const btn = screen.getByRole("button", { name: "Xuất CSV" });
    btn.focus();
    rerender(page(true));
    // Chrome's focus fixup: a focused control that turns disabled loses focus (jsdom does not do it).
    btn.blur();
    expect(document.activeElement).toBe(document.body);
    rerender(page(false));
    expect(btn).toHaveFocus();
  });

  it("a re-enabled action does not take focus the user moved elsewhere", () => {
    const page = (disabled: boolean) => (
      <>
        <ObjectHomePage icon={icon} label="Khách hàng" title="Tất cả" actions={[{ label: "Xuất CSV", onSelect: () => {}, disabled }]} />
        <input aria-label="Lọc" />
      </>
    );
    const { rerender } = render(page(false));
    const btn = screen.getByRole("button", { name: "Xuất CSV" });
    btn.focus();
    rerender(page(true));
    btn.blur();
    screen.getByRole("textbox", { name: "Lọc" }).focus();
    rerender(page(false));
    expect(screen.getByRole("textbox", { name: "Lọc" })).toHaveFocus();
  });

  it("search: left-icon input, typing reports the value, clear button empties it", async () => {
    function Harness() {
      const [q, setQ] = useState("");
      return <ObjectHomePage icon={icon} label="Khách hàng" title="Tất cả" search={{ value: q, onChange: setQ }} />;
    }
    render(<Harness />);
    const input = screen.getByRole("textbox", { name: "Tìm trong danh sách" });
    expect(input).toHaveClass("slds-input");
    expect(input.parentElement).toHaveClass("slds-form-element__control", "slds-input-has-icon", "slds-input-has-icon_left");
    expect(input.closest(".slds-page-header__control")).not.toBeNull();
    expect(screen.queryByRole("button", { name: "Xoá nội dung tìm" })).toBeNull();

    await userEvent.type(input, "Minh");
    expect(input).toHaveValue("Minh");
    expect(input.parentElement).toHaveClass("slds-input-has-icon_left-right");
    await userEvent.click(screen.getByRole("button", { name: "Xoá nội dung tìm" }));
    expect(input).toHaveValue("");
    expect(input).toHaveFocus();
  });

  it("view switcher: icon-more trigger, checked item is the current view, Enter picks another", async () => {
    const onChange = vi.fn();
    function Harness() {
      const [view, setView] = useState<ListViewMode>("table");
      return (
        <ObjectHomePage
          icon={icon}
          label="Khách hàng"
          title="Tất cả"
          view={{
            value: view,
            onChange: (v) => {
              onChange(v);
              setView(v);
            },
          }}
        />
      );
    }
    render(<Harness />);
    const trigger = screen.getByRole("button", { name: "Hiển thị dạng: Bảng" });
    expect(trigger).toHaveClass("slds-button_icon", "slds-button_icon-more");
    trigger.focus();
    await userEvent.keyboard("{Enter}");
    const items = screen.getAllByRole("menuitemcheckbox");
    expect(items.map((i) => i.textContent)).toEqual(["Bảng", "Kanban", "Lịch"]);
    expect(items[0]).toHaveAttribute("aria-checked", "true");
    await userEvent.keyboard("{ArrowDown}{Enter}");
    expect(onChange).toHaveBeenCalledWith("kanban");
    expect(screen.getByRole("button", { name: "Hiển thị dạng: Kanban" })).toBeInTheDocument();
  });

  it("density menu offers the three densities", async () => {
    const onChange = vi.fn();
    render(
      <ObjectHomePage icon={icon} label="Khách hàng" title="Tất cả" density={{ value: "normal" as ListDensity, onChange }} />,
    );
    screen.getByRole("button", { name: "Mật độ hiển thị" }).focus();
    await userEvent.keyboard("{Enter}");
    const items = screen.getAllByRole("menuitemcheckbox");
    expect(items.map((i) => i.textContent)).toEqual(["Gọn", "Bình thường", "Rộng"]);
    expect(items[1]).toHaveAttribute("aria-checked", "true");
    await userEvent.click(items[0]);
    expect(onChange).toHaveBeenCalledWith("compact");
  });

  it("filter toggle is a stateful button-icon controlling the panel", async () => {
    const onToggle = vi.fn();
    const { rerender } = render(
      <ObjectHomePage icon={icon} label="Khách hàng" title="Tất cả" filter={{ open: false, onToggle, panelId: "fp" }} />,
    );
    const btn = screen.getByRole("button", { name: "Bộ lọc" });
    expect(btn).toHaveClass("slds-button_icon", "slds-button_icon-border-filled");
    expect(btn).toHaveAttribute("aria-pressed", "false");
    expect(btn).toHaveAttribute("aria-controls", "fp");
    expect(btn).not.toHaveClass("slds-is-selected");
    await userEvent.click(btn);
    expect(onToggle).toHaveBeenCalledOnce();

    rerender(<ObjectHomePage icon={icon} label="Khách hàng" title="Tất cả" filter={{ open: true, onToggle, panelId: "fp" }} />);
    expect(screen.getByRole("button", { name: "Bộ lọc" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: "Bộ lọc" })).toHaveClass("slds-is-selected");
  });
});
