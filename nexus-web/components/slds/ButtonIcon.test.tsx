// ButtonIcon — blueprint `button-icons` (base, transparent-container, bordered-*, brand, inverse,
// stateful), SLDS 2.264.1. Class strings below are copied from the rendered blueprints.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import * as DropdownMenu from "@radix-ui/react-dropdown-menu";
import { Settings } from "lucide-react";
import { ButtonIcon } from "@/components/slds/ButtonIcon";

const cls = (el: Element) => el.className.split(/\s+/).filter(Boolean).sort();

describe("ButtonIcon", () => {
  it("bare default: title + assistive text carry the label, glyph hidden from AT", () => {
    const { container } = render(<ButtonIcon icon={Settings} label="Cài đặt" />);
    const b = screen.getByRole("button", { name: "Cài đặt" });
    expect(cls(b)).toEqual(["slds-button", "slds-button_icon"]);
    expect(b).toHaveAttribute("title", "Cài đặt");
    expect(b).toHaveAttribute("type", "button");
    expect(b.querySelector(".slds-assistive-text")).toHaveTextContent("Cài đặt");
    const svg = container.querySelector("svg")!;
    expect(svg).toHaveClass("slds-button__icon", "nx-lucide");
    expect(svg).toHaveAttribute("aria-hidden", "true");
    expect(b).not.toHaveAttribute("aria-pressed");
    expect(b).not.toHaveAttribute("aria-haspopup");
  });

  it.each([
    ["container", "slds-button_icon-container"],
    ["border", "slds-button_icon-border"],
    ["border-filled", "slds-button_icon-border-filled"],
    ["border-inverse", "slds-button_icon-border-inverse"],
    ["brand", "slds-button_icon-brand"],
    ["inverse", "slds-button_icon-inverse"],
  ] as const)("variant %s → %s", (variant, c) => {
    render(<ButtonIcon icon={Settings} label="X" variant={variant} />);
    expect(cls(screen.getByRole("button"))).toEqual(["slds-button", "slds-button_icon", c].sort());
  });

  // "With dropdown" blueprints: each variant has its own container class, plus a second x-small glyph.
  it.each([
    ["container", ["slds-button_icon-container-more"]],
    ["border", ["slds-button_icon-container-more", "slds-button_icon-border"]],
    ["border-filled", ["slds-button_icon-more"]],
    ["border-inverse", ["slds-button_icon-more"]],
    ["inverse", ["slds-button_icon-container-more", "slds-button_icon-inverse"]],
  ] as const)("more + %s → blueprint with-dropdown classes", (variant, extra) => {
    const { container } = render(<ButtonIcon icon={Settings} label="Thêm tuỳ chọn" variant={variant} more />);
    const b = screen.getByRole("button", { name: "Thêm tuỳ chọn" });
    expect(cls(b)).toEqual(["slds-button", "slds-button_icon", ...extra].sort());
    expect(b).toHaveAttribute("aria-haspopup", "true");
    expect(b).toHaveAttribute("aria-expanded", "false");
    const [main, chevron] = Array.from(container.querySelectorAll("svg"));
    expect(main).toHaveClass("slds-button__icon", "nx-lucide");
    expect(main).not.toHaveClass("slds-button__icon_x-small");
    expect(chevron).toHaveClass("slds-button__icon", "slds-button__icon_x-small");
  });

  it("container size and glyph size are separate class families", () => {
    const { container } = render(<ButtonIcon icon={Settings} label="X" variant="border" size="small" iconSize="large" />);
    expect(screen.getByRole("button")).toHaveClass("slds-button_icon-small");
    expect(container.querySelector("svg")).toHaveClass("slds-button__icon_large");
  });

  it("stateful: selected → slds-is-selected + aria-pressed; unselected keeps aria-pressed=false", () => {
    const { rerender } = render(<ButtonIcon icon={Settings} label="Thích" variant="border-filled" selected={false} />);
    const b = screen.getByRole("button");
    expect(b).toHaveAttribute("aria-pressed", "false");
    expect(b).not.toHaveClass("slds-is-selected");
    rerender(<ButtonIcon icon={Settings} label="Thích" variant="border-filled" selected />);
    expect(b).toHaveAttribute("aria-pressed", "true");
    expect(b).toHaveClass("slds-is-selected");
  });

  it("error/warning states and hint glyphs", () => {
    const { container, rerender } = render(<ButtonIcon icon={Settings} label="Lỗi" state="error" />);
    expect(screen.getByRole("button")).toHaveClass("slds-button_icon-error");
    rerender(<ButtonIcon icon={Settings} label="Cảnh báo" state="warning" hint />);
    expect(screen.getByRole("button")).toHaveClass("slds-button_icon-warning");
    expect(container.querySelector("svg")).toHaveClass("slds-button__icon_hint");
    rerender(<ButtonIcon icon={Settings} label="X" variant="inverse" hint />);
    expect(container.querySelector("svg")).toHaveClass("slds-button__icon_inverse-hint");
  });

  it("disabled blocks clicks; Enter/Space activate when enabled", async () => {
    const onClick = vi.fn();
    const { rerender } = render(<ButtonIcon icon={Settings} label="X" onClick={onClick} disabled />);
    await userEvent.click(screen.getByRole("button"));
    expect(onClick).not.toHaveBeenCalled();
    rerender(<ButtonIcon icon={Settings} label="X" onClick={onClick} />);
    screen.getByRole("button").focus();
    await userEvent.keyboard("{Enter}");
    await userEvent.keyboard(" ");
    expect(onClick).toHaveBeenCalledTimes(2);
  });

  // Stability contract for the menu/popover groups: Radix asChild must be able to drive it.
  it("works as a Radix DropdownMenu trigger (asChild): aria-expanded follows the menu, Esc closes", async () => {
    render(
      <DropdownMenu.Root>
        <DropdownMenu.Trigger asChild>
          <ButtonIcon icon={Settings} label="Thêm thao tác" variant="border-filled" more />
        </DropdownMenu.Trigger>
        <DropdownMenu.Portal>
          <DropdownMenu.Content className="slds-dropdown">
            <DropdownMenu.Item>Mục một</DropdownMenu.Item>
          </DropdownMenu.Content>
        </DropdownMenu.Portal>
      </DropdownMenu.Root>,
    );
    const trigger = screen.getByRole("button", { name: "Thêm thao tác" });
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(trigger).toHaveAttribute("aria-haspopup", "menu");
    trigger.focus();
    await userEvent.keyboard("{Enter}");
    expect(trigger).toHaveAttribute("aria-expanded", "true");
    expect(await screen.findByRole("menuitem", { name: "Mục một" })).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");
    expect(trigger).toHaveAttribute("aria-expanded", "false");
  });
});
