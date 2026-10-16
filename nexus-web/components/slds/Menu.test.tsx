// Menu — blueprint menus/dropdown (SLDS 2.264.1) on @radix-ui/react-dropdown-menu.
import { describe, it, expect, vi } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Pencil, Table } from "lucide-react";
import { useState } from "react";
import { Menu, MenuCheckboxItem, MenuDivider, MenuGroup, MenuItem } from "./Menu";
import { Modal } from "./Modal";
import { Panel } from "./Panel";

const trigger = (
  <button type="button" className="slds-button slds-button_icon slds-button_icon-border-filled" title="Thêm thao tác">
    <span className="slds-assistive-text">Thêm thao tác</span>
  </button>
);

function openWithKeyboard() {
  screen.getByRole("button", { name: "Thêm thao tác" }).focus();
  return userEvent.keyboard("{Enter}");
}

describe("Menu", () => {
  it("wraps the trigger in slds-dropdown-trigger and wires aria-haspopup/expanded", () => {
    const { container } = render(
      <Menu trigger={trigger}>
        <MenuItem label="Sửa" />
      </Menu>,
    );
    expect(container.firstElementChild).toHaveClass("slds-dropdown-trigger", "slds-dropdown-trigger_click");
    const btn = screen.getByRole("button", { name: "Thêm thao tác" });
    expect(btn).toHaveAttribute("aria-haspopup", "menu");
    expect(btn).toHaveAttribute("aria-expanded", "false");
  });

  it("joins `leading` buttons and the trigger in one slds-button-group", () => {
    const { container } = render(
      <Menu trigger={trigger} leading={<button type="button">Ghim</button>}>
        <MenuItem label="Sửa" />
      </Menu>,
    );
    const group = container.firstElementChild!.firstElementChild!;
    expect(group).toHaveClass("slds-button-group");
    expect(Array.from(group.children).map((b) => b.textContent)).toEqual(["Ghim", "Thêm thao tác"]);
    expect(screen.getByRole("button", { name: "Ghim" })).not.toHaveAttribute("aria-haspopup");
  });

  it("opens the blueprint dropdown markup from the keyboard", async () => {
    render(
      <Menu trigger={trigger}>
        <MenuItem label="Sửa" icon={Pencil} />
        <MenuItem label="Sao chép" />
        <MenuDivider />
        <MenuItem label="Xoá" tone="error" />
      </Menu>,
    );
    await openWithKeyboard();
    const menu = screen.getByRole("menu", { name: "Thêm thao tác" });
    expect(menu).toHaveClass("slds-dropdown", "slds-dropdown_left");
    // Rendered inline (not portaled) so it stays in a Modal's stacking context; the wrapper un-hides it.
    expect(menu.closest(".slds-dropdown-trigger")).toHaveClass("slds-is-open");
    const list = menu.firstElementChild!;
    expect(list.tagName).toBe("UL");
    expect(list).toHaveClass("slds-dropdown__list");
    expect(list).toHaveAttribute("role", "presentation");
    const items = screen.getAllByRole("menuitem");
    expect(items).toHaveLength(3);
    for (const item of items) {
      expect(item.tagName).toBe("A");
      expect(item.parentElement).toHaveClass("slds-dropdown__item");
      expect(item.parentElement).toHaveAttribute("role", "presentation");
    }
    expect(items[0].querySelector("span[title='Sửa'] svg")).toHaveClass("slds-icon", "slds-icon_x-small", "slds-icon-text-default", "slds-m-right_x-small");
    expect(screen.getByRole("separator")).toHaveClass("slds-has-divider_top-space");
    expect(items[2]).toHaveClass("slds-has-error");
    expect(items[2].querySelector(".slds-icon_container.slds-icon-utility-error .slds-assistive-text")).toHaveTextContent("Lỗi");
  });

  it("aligns right with slds-dropdown_right", async () => {
    render(
      <Menu trigger={trigger} align="right">
        <MenuItem label="Sửa" />
      </Menu>,
    );
    await openWithKeyboard();
    expect(screen.getByRole("menu")).toHaveClass("slds-dropdown_right");
  });

  it("moves with arrow keys, selects with Enter, closes and returns focus", async () => {
    const onEdit = vi.fn();
    const onCopy = vi.fn();
    render(
      <Menu trigger={trigger}>
        <MenuItem label="Sửa" onSelect={onEdit} />
        <MenuItem label="Sao chép" onSelect={onCopy} />
      </Menu>,
    );
    await openWithKeyboard();
    expect(document.activeElement).toHaveTextContent("Sửa");
    await userEvent.keyboard("{ArrowDown}");
    expect(document.activeElement).toHaveTextContent("Sao chép");
    await userEvent.keyboard("{Enter}");
    expect(onCopy).toHaveBeenCalledTimes(1);
    expect(onEdit).not.toHaveBeenCalled();
    expect(screen.queryByRole("menu")).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Thêm thao tác" }));
  });

  it("closes on Escape and skips disabled items", async () => {
    const onDelete = vi.fn();
    render(
      <Menu trigger={trigger}>
        <MenuItem label="Sửa" />
        <MenuItem label="Xoá" disabled onSelect={onDelete} />
        <MenuItem label="Chia sẻ" />
      </Menu>,
    );
    await openWithKeyboard();
    expect(screen.getByRole("menuitem", { name: "Xoá" })).toHaveAttribute("aria-disabled", "true");
    await userEvent.keyboard("{ArrowDown}");
    expect(document.activeElement).toHaveTextContent("Chia sẻ");
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("menu")).toBeNull();
    expect(onDelete).not.toHaveBeenCalled();
  });

  it("renders checkbox items with the selected check and toggles slds-is-selected", async () => {
    function Views() {
      const [view, setView] = useState("table");
      return (
        <Menu trigger={trigger}>
          <MenuCheckboxItem label="Bảng" iconRight={Table} checked={view === "table"} onCheckedChange={() => setView("table")} />
          <MenuCheckboxItem label="Kanban" checked={view === "kanban"} onCheckedChange={() => setView("kanban")} />
        </Menu>
      );
    }
    render(<Views />);
    await openWithKeyboard();
    const [table, kanban] = screen.getAllByRole("menuitemcheckbox");
    expect(table).toHaveAttribute("aria-checked", "true");
    expect(table.parentElement).toHaveClass("slds-dropdown__item", "slds-is-selected");
    expect(table.querySelector("svg.slds-icon_selected")).not.toBeNull();
    expect(kanban.parentElement).not.toHaveClass("slds-is-selected");
    expect(kanban.querySelector("svg.slds-icon_selected")).not.toBeNull();
    await userEvent.click(kanban);
    await openWithKeyboard();
    expect(screen.getByRole("menuitemcheckbox", { name: "Kanban" })).toHaveAttribute("aria-checked", "true");
    expect(screen.getByRole("menuitemcheckbox", { name: "Kanban" }).parentElement).toHaveClass("slds-is-selected");
  });

  it("groups items under a sub-heading", async () => {
    render(
      <Menu trigger={trigger} size="small">
        <MenuGroup label="Bản ghi">
          <MenuItem label="Sửa" />
        </MenuGroup>
      </Menu>,
    );
    await openWithKeyboard();
    expect(screen.getByRole("menu")).toHaveClass("slds-dropdown_small");
    const group = screen.getByRole("group", { name: "Bản ghi" });
    expect(group.tagName).toBe("UL");
    expect(group.firstElementChild).toHaveClass("slds-dropdown__header");
    expect(group.parentElement).toHaveAttribute("role", "presentation");
  });

  it("typeahead matches the visible label, not a tone item's assistive text", async () => {
    render(
      <Menu trigger={trigger}>
        <MenuItem label="Sửa" />
        <MenuItem label="Xoá" tone="error" />
        <MenuItem label="Lưu" />
      </Menu>,
    );
    await openWithKeyboard();
    await userEvent.keyboard("l");
    expect(document.activeElement).toHaveTextContent(/^Lưu$/);
    // Radix buffers typed characters for ~1s; reopening resets the search.
    await userEvent.keyboard("{Escape}");
    await openWithKeyboard();
    await userEvent.keyboard("x");
    expect(document.activeElement).toBe(screen.getByRole("menuitem", { name: /Xoá/ }));
  });

  it("Tab closes the menu and moves focus to the next focusable element on the page", async () => {
    render(
      <>
        <Menu trigger={trigger}>
          <MenuItem label="Sửa" />
        </Menu>
        <button type="button">Sau</button>
      </>,
    );
    await openWithKeyboard();
    expect(document.activeElement).toHaveTextContent("Sửa");
    await userEvent.tab();
    expect(screen.queryByRole("menu")).toBeNull();
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Sau" })));
  });

  it("Shift+Tab closes the menu and returns focus to the trigger", async () => {
    render(
      <>
        <Menu trigger={trigger}>
          <MenuItem label="Sửa" />
        </Menu>
        <button type="button">Sau</button>
      </>,
    );
    await openWithKeyboard();
    await userEvent.tab({ shift: true });
    expect(screen.queryByRole("menu")).toBeNull();
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Thêm thao tác" })));
  });

  // Row action menus open a docked panel or a confirm modal. Both remember the element focused when
  // they open and hand focus back to it on close (panels/base + modals/base docs) — the menu item
  // itself is gone by then, so the item's action must run with focus already on the trigger.
  describe("an item that opens a Panel or a Modal", () => {
    function Page() {
      const [panel, setPanel] = useState(false);
      const [modal, setModal] = useState(false);
      return (
        <>
          <Menu trigger={trigger}>
            <MenuItem label="Sửa" onSelect={() => setPanel(true)} />
            <MenuItem label="Xoá" onSelect={() => setModal(true)} />
          </Menu>
          <Panel open={panel} title="Sửa" onClose={() => setPanel(false)}>
            <input aria-label="Tên" />
          </Panel>
          <Modal open={modal} onOpenChange={setModal} title="Xoá?" footer={<button type="button" onClick={() => setModal(false)}>Huỷ</button>}>
            <p>Chắc chưa?</p>
          </Modal>
        </>
      );
    }
    // Radix's focus-return runs in a timeout after the menu unmounts; let it run before asserting.
    const settle = () => act(() => new Promise((r) => setTimeout(r, 20)));

    it("focus stays in the panel, and closing the panel returns it to the menu trigger", async () => {
      render(<Page />);
      await openWithKeyboard();
      await userEvent.keyboard("{Enter}");
      await settle();
      expect(document.activeElement).toBe(screen.getByLabelText("Tên"));
      await userEvent.click(screen.getByRole("button", { name: "Thu gọn Sửa" }));
      await settle();
      expect(document.activeElement).toBe(screen.getByRole("button", { name: "Thêm thao tác" }));
    });

    it("closing the modal returns focus to the menu trigger", async () => {
      render(<Page />);
      await openWithKeyboard();
      await userEvent.keyboard("{ArrowDown}{Enter}");
      const dialog = await screen.findByRole("dialog");
      await settle();
      expect(dialog).toContainElement(document.activeElement as HTMLElement);
      await userEvent.click(within(dialog).getByRole("button", { name: "Huỷ" }));
      await settle();
      expect(document.activeElement).toBe(screen.getByRole("button", { name: "Thêm thao tác" }));
    });

    it("the next close by Escape still returns focus to the trigger", async () => {
      render(<Page />);
      await openWithKeyboard();
      await userEvent.keyboard("{Enter}");
      await settle();
      await userEvent.click(screen.getByRole("button", { name: "Thu gọn Sửa" }));
      await settle();
      await openWithKeyboard();
      await userEvent.keyboard("{Escape}");
      await settle();
      expect(document.activeElement).toBe(screen.getByRole("button", { name: "Thêm thao tác" }));
    });

    it("an action that moves focus itself keeps it there", async () => {
      render(
        <>
          <Menu trigger={trigger}>
            <MenuItem label="Tìm" onSelect={() => document.getElementById("find")?.focus()} />
          </Menu>
          <input id="find" aria-label="Tìm kiếm" />
        </>,
      );
      await openWithKeyboard();
      await userEvent.keyboard("{Enter}");
      await settle();
      expect(document.activeElement).toBe(screen.getByLabelText("Tìm kiếm"));
    });
  });

  it("works inside an open Modal: Escape closes the menu, not the modal", async () => {
    const onOpenChange = vi.fn();
    const onSelect = vi.fn();
    render(
      <Modal open onOpenChange={onOpenChange} title="Sửa khách hàng">
        <Menu trigger={trigger}>
          <MenuItem label="Sửa" onSelect={onSelect} />
        </Menu>
      </Modal>,
    );
    await openWithKeyboard();
    expect(screen.getByRole("menu")).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("menu")).toBeNull();
    expect(onOpenChange).not.toHaveBeenCalled();
    await openWithKeyboard();
    await userEvent.keyboard("{Enter}");
    expect(onSelect).toHaveBeenCalledTimes(1);
  });
});
