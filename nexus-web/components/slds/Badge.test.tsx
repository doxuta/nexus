// Badge — blueprint `badges/base`, SLDS 2.264.1.
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { Coins } from "lucide-react";
import { Badge } from "@/components/slds/Badge";

describe("Badge", () => {
  it("default: a bare span.slds-badge", () => {
    render(<Badge>Nhãn</Badge>);
    const b = screen.getByText("Nhãn");
    expect(b.tagName).toBe("SPAN");
    expect(b.className).toBe("slds-badge");
  });

  it.each([
    [{ variant: "inverse" as const }, "slds-badge_inverse"],
    [{ variant: "lightest" as const }, "slds-badge_lightest"],
    [{ theme: "success" as const }, "slds-theme_success"],
    [{ theme: "warning" as const }, "slds-theme_warning"],
    [{ theme: "error" as const }, "slds-theme_error"],
  ])("%o → %s", (props, cls) => {
    render(<Badge {...props}>X</Badge>);
    expect(screen.getByText("X").className).toBe(`slds-badge ${cls}`);
  });

  it("left icon: badge__icon_left > icon_container.slds-current-color > xx-small Lucide glyph", () => {
    const { container } = render(
      <Badge iconLeft={Coins} iconTitle="Tín dụng">
        423 tín dụng
      </Badge>,
    );
    const badge = container.firstElementChild!;
    const slot = badge.firstElementChild!;
    expect(slot.className).toBe("slds-badge__icon slds-badge__icon_left");
    const box = slot.firstElementChild!;
    expect(box.className).toBe("slds-icon_container slds-current-color");
    expect(box).toHaveAttribute("title", "Tín dụng");
    const svg = box.querySelector("svg")!;
    expect(svg).toHaveClass("slds-icon", "slds-icon_xx-small", "nx-lucide");
    expect(svg).toHaveAttribute("aria-hidden", "true");
    expect(badge).toHaveTextContent("423 tín dụng");
  });

  it("right icon on an inverse badge adds slds-badge__icon_inverse, after the text", () => {
    const { container } = render(
      <Badge variant="inverse" iconRight={Coins}>
        423 tín dụng
      </Badge>,
    );
    const slot = container.firstElementChild!.lastElementChild!;
    expect(slot.className).toBe("slds-badge__icon slds-badge__icon_right slds-badge__icon_inverse");
  });

  it("keeps child elements (blueprint 'With Child Element')", () => {
    render(
      <Badge>
        Thành phần: <em>Lightning</em>
      </Badge>,
    );
    expect(screen.getByText("Lightning").tagName).toBe("EM");
  });
});
