// Button — blueprint `buttons` (base, with-icon), SLDS 2.264.1.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Download } from "lucide-react";
import { Button, buttonClassName } from "@/components/slds/Button";

describe("Button", () => {
  it("defaults to a neutral, non-submitting button (lightning-button default)", () => {
    render(<Button>Lưu</Button>);
    const b = screen.getByRole("button", { name: "Lưu" });
    expect(b).toHaveClass("slds-button", "slds-button_neutral");
    expect(b).toHaveAttribute("type", "button");
  });

  it.each([
    ["brand", "slds-button_brand"],
    ["outline-brand", "slds-button_outline-brand"],
    ["destructive", "slds-button_destructive"],
    ["text-destructive", "slds-button_text-destructive"],
    ["success", "slds-button_success"],
    ["inverse", "slds-button_inverse"],
  ] as const)("variant %s → %s", (variant, cls) => {
    render(<Button variant={variant}>X</Button>);
    expect(screen.getByRole("button")).toHaveClass("slds-button", cls);
    expect(screen.getByRole("button")).not.toHaveClass("slds-button_neutral");
  });

  it("variant base is the bare slds-button (looks like a link)", () => {
    render(<Button variant="base">X</Button>);
    expect(screen.getByRole("button").className).toBe("slds-button");
  });

  it("renders left/right Lucide glyphs with the blueprint icon classes, hidden from AT", () => {
    const { container } = render(
      <Button iconLeft={Download} iconRight={Download} size="small">
        Tải về
      </Button>,
    );
    const [left, right] = Array.from(container.querySelectorAll("svg"));
    expect(left).toHaveClass("slds-button__icon", "slds-button__icon_left", "slds-button__icon_small", "nx-lucide");
    expect(right).toHaveClass("slds-button__icon", "slds-button__icon_right", "slds-button__icon_small", "nx-lucide");
    expect(left).toHaveAttribute("aria-hidden", "true");
    expect(screen.getByRole("button", { name: "Tải về" })).toBeInTheDocument();
  });

  it("icon-only neutral button: the glyph has no left/right spacing class", () => {
    const { container } = render(<Button iconLeft={Download} aria-label="Tải về" />);
    const svg = container.querySelector("svg")!;
    expect(svg).toHaveClass("slds-button__icon");
    expect(svg).not.toHaveClass("slds-button__icon_left");
    expect(screen.getByRole("button", { name: "Tải về" })).toHaveClass("slds-button_neutral");
  });

  it("stretch adds slds-button_stretch", () => {
    render(<Button stretch>X</Button>);
    expect(screen.getByRole("button")).toHaveClass("slds-button_stretch");
  });

  it("disabled blocks clicks", async () => {
    const onClick = vi.fn();
    render(
      <Button disabled onClick={onClick}>
        X
      </Button>,
    );
    await userEvent.click(screen.getByRole("button"));
    expect(onClick).not.toHaveBeenCalled();
    expect(screen.getByRole("button")).toBeDisabled();
  });

  it("Enter and Space activate it (native button)", async () => {
    const onClick = vi.fn();
    render(<Button onClick={onClick}>X</Button>);
    screen.getByRole("button").focus();
    await userEvent.keyboard("{Enter}");
    await userEvent.keyboard(" ");
    expect(onClick).toHaveBeenCalledTimes(2);
  });

  it("type=submit can be opted into; extra className is kept", () => {
    render(
      <Button type="submit" className="slds-m-left_small">
        X
      </Button>,
    );
    expect(screen.getByRole("button")).toHaveAttribute("type", "submit");
    expect(screen.getByRole("button")).toHaveClass("slds-m-left_small");
  });

  it("buttonClassName gives the same classes for the 'as a link' blueprint variants", () => {
    expect(buttonClassName("brand")).toBe("slds-button slds-button_brand");
    expect(buttonClassName("neutral", { stretch: true })).toBe("slds-button slds-button_neutral slds-button_stretch");
  });

  // specs/073 T052: blueprint buttons/stateful — pressed state is exposed to AT, other variants untouched.
  it("stateful ⇒ slds-button_stateful + aria-pressed, flipping selected/not-selected", () => {
    const { rerender } = render(<Button stateful selected={false}>Trợ lý</Button>);
    const b = screen.getByRole("button", { name: "Trợ lý" });
    expect(b).toHaveClass("slds-button", "slds-button_neutral", "slds-button_stateful", "slds-not-selected");
    expect(b).toHaveAttribute("aria-pressed", "false");
    rerender(<Button stateful selected>Trợ lý</Button>);
    expect(b).toHaveClass("slds-is-selected");
    expect(b).not.toHaveClass("slds-not-selected");
    expect(b).toHaveAttribute("aria-pressed", "true");
  });

  it("a plain button has no stateful class and no aria-pressed", () => {
    render(<Button>Lưu</Button>);
    const b = screen.getByRole("button");
    expect(b).not.toHaveClass("slds-button_stateful");
    expect(b).not.toHaveAttribute("aria-pressed");
  });
});
