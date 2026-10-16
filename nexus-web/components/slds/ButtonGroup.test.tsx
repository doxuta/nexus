// ButtonGroup — blueprint `button-groups` (base, list, row), SLDS 2.264.1.
import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { ChevronDown } from "lucide-react";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { ButtonGroup } from "@/components/slds/ButtonGroup";

describe("ButtonGroup", () => {
  it("base: div.slds-button-group[role=group] around the buttons, no wrappers", () => {
    render(
      <ButtonGroup label="Thao tác">
        <Button>Làm mới</Button>
        <Button>Sửa</Button>
      </ButtonGroup>,
    );
    const g = screen.getByRole("group", { name: "Thao tác" });
    expect(g.tagName).toBe("DIV");
    expect(g.className).toBe("slds-button-group");
    expect(Array.from(g.children).map((c) => c.tagName)).toEqual(["BUTTON", "BUTTON"]);
  });

  it("base + overflow: the trigger sits in the blueprint's slds-button_last dropdown-trigger div", () => {
    render(
      <ButtonGroup overflow={<ButtonIcon icon={ChevronDown} label="Thêm thao tác" variant="border-filled" aria-haspopup="true" />}>
        <Button>Lưu</Button>
      </ButtonGroup>,
    );
    const last = screen.getByRole("button", { name: "Thêm thao tác" }).parentElement!;
    expect(last).toHaveClass("slds-dropdown-trigger", "slds-dropdown-trigger_click", "slds-button_last");
    expect(last.parentElement).toHaveClass("slds-button-group");
  });

  it("list: ul.slds-button-group-list, one li per child, overflow in the last li", () => {
    const { container } = render(
      <ButtonGroup variant="list" overflow={<ButtonIcon icon={ChevronDown} label="Thêm" variant="border-filled" />}>
        <Button>Làm mới</Button>
        {null}
        <Button>Sửa</Button>
      </ButtonGroup>,
    );
    const ul = container.querySelector("ul")!;
    expect(ul.className).toBe("slds-button-group-list");
    expect(ul).not.toHaveAttribute("role");
    const items = Array.from(ul.children);
    expect(items.map((li) => li.tagName)).toEqual(["LI", "LI", "LI"]);
    expect(items[0].className).toBe("");
    expect(within(items[2] as HTMLElement).getByRole("button", { name: "Thêm" }).parentElement).toHaveClass("slds-button_last");
  });

  it("list: a conditional child appearing keeps the other buttons mounted (focus stays put)", () => {
    const Group = ({ extra }: { extra: boolean }): ReactNode => (
      <ButtonGroup variant="list">
        <Button>Làm mới</Button>
        {extra && <Button>Đặt lại</Button>}
        <Button>Sửa bố cục</Button>
      </ButtonGroup>
    );
    const { rerender } = render(<Group extra={false} />);
    const edit = screen.getByRole("button", { name: "Sửa bố cục" });
    edit.focus();
    rerender(<Group extra />);
    expect(screen.getByRole("button", { name: "Sửa bố cục" })).toBe(edit);
    expect(document.activeElement).toBe(edit);
    rerender(<Group extra={false} />);
    expect(document.activeElement).toBe(edit);
  });

  it("row: ul.slds-button-group-row with li.slds-button-group-item", () => {
    const { container } = render(
      <ButtonGroup variant="row">
        <Button>Sửa</Button>
        <Button variant="brand">Lưu</Button>
      </ButtonGroup>,
    );
    const ul = container.querySelector("ul")!;
    expect(ul.className).toBe("slds-button-group-row");
    const items = Array.from(ul.children);
    expect(items.map((li) => li.tagName)).toEqual(["LI", "LI"]);
    items.forEach((li) => expect(li.className).toBe("slds-button-group-item"));
  });
});
