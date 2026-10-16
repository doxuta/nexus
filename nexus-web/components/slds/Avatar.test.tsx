// Avatar — blueprint `avatar` (base, initials), SLDS 2.264.1.
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { Building2 } from "lucide-react";
import { Avatar, avatarInitials } from "@/components/slds/Avatar";

describe("avatarInitials (SLDS rule: two words → first+last capital; one word → Xx)", () => {
  it.each([
    ["Đỗ Xuân Tài", "ĐT"],
    ["  trần   thị  bích  ", "TB"],
    ["Acme", "Ac"],
    ["northwind", "No"],
    ["A", "A"],
    ["", ""],
    // Decomposed input (NFD) is normalised first, so the tone mark stays on its letter.
    ["Ống Ba", "ỐB"],
  ])("%j → %j", (name, want) => {
    expect(avatarInitials(name)).toBe(want);
  });
});

describe("Avatar", () => {
  it("image: span.slds-avatar > img with alt + title", () => {
    const { container } = render(<Avatar name="Đỗ Xuân Tài" src="/a.png" shape="circle" size="large" />);
    const span = container.firstElementChild!;
    expect(span.className).toBe("slds-avatar slds-avatar_circle slds-avatar_large");
    const img = screen.getByRole("img", { name: "Đỗ Xuân Tài" });
    expect(img).toHaveAttribute("src", "/a.png");
    expect(img).toHaveAttribute("title", "Đỗ Xuân Tài");
  });

  it("person fallback: circle with abbr initials on the standard-user colour", () => {
    const { container } = render(<Avatar name="Đỗ Xuân Tài" shape="circle" />);
    const abbr = container.querySelector("abbr")!;
    expect(abbr.className).toBe("slds-avatar__initials slds-icon-standard-user");
    expect(abbr).toHaveAttribute("title", "Đỗ Xuân Tài");
    expect(abbr).toHaveTextContent("ĐT");
    expect(container.firstElementChild!.className).toBe("slds-avatar slds-avatar_circle");
  });

  it("entity fallback: square (default) with the entity colour; inverse initials", () => {
    const { container, rerender } = render(<Avatar name="Acme" colorName="account" />);
    expect(container.firstElementChild!.className).toBe("slds-avatar");
    expect(container.querySelector("abbr")!.className).toBe("slds-avatar__initials slds-icon-standard-account");
    rerender(<Avatar name="Acme" inverse />);
    expect(container.querySelector("abbr")!.className).toBe("slds-avatar__initials slds-avatar__initials_inverse");
  });

  it("icon fallback: the entity icon tile with assistive text", () => {
    const { container } = render(<Avatar name="Công ty Acme" icon={Building2} colorName="account" />);
    expect(container.querySelector("abbr")).toBeNull();
    const tile = container.querySelector(".slds-icon_container")!;
    expect(tile).toHaveClass("slds-icon-standard-account");
    expect(tile).toHaveAttribute("title", "Công ty Acme");
    expect(tile.querySelector("svg")).toHaveClass("slds-icon");
    expect(tile.querySelector(".slds-assistive-text")).toHaveTextContent("Công ty Acme");
  });

  it.each(["x-small", "small", "medium", "large"] as const)("size %s", (size) => {
    const { container } = render(<Avatar name="A B" size={size} />);
    expect(container.firstElementChild).toHaveClass(`slds-avatar_${size}`);
  });
});
