// Spinner — blueprint `spinners/base`, SLDS 2.264.1.
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { Spinner } from "@/components/slds/Spinner";

describe("Spinner", () => {
  it("default: role=status, medium, Vietnamese assistive text, two dot divs", () => {
    render(<Spinner />);
    const s = screen.getByRole("status");
    expect(s.className).toBe("slds-spinner slds-spinner_medium");
    expect(s.querySelector(".slds-assistive-text")).toHaveTextContent("Đang tải");
    expect(Array.from(s.children).map((c) => c.className)).toEqual([
      "slds-assistive-text",
      "slds-spinner__dot-a",
      "slds-spinner__dot-b",
    ]);
    expect(s.parentElement).not.toHaveClass("slds-spinner_container");
  });

  it.each(["xx-small", "x-small", "small", "medium", "large"] as const)("size %s", (size) => {
    render(<Spinner size={size} />);
    expect(screen.getByRole("status")).toHaveClass(`slds-spinner_${size}`);
  });

  it("brand / inverse / inline / delayed modifiers and a custom label", () => {
    const { rerender } = render(<Spinner variant="brand" inline delayed label="Đang lưu" />);
    const s = screen.getByRole("status");
    expect(s).toHaveClass("slds-spinner_brand", "slds-spinner_inline", "slds-spinner_delayed");
    expect(s).toHaveTextContent("Đang lưu");
    rerender(<Spinner variant="inverse" />);
    expect(screen.getByRole("status")).toHaveClass("slds-spinner_inverse");
  });

  it("container wraps it in slds-spinner_container; fixed adds slds-is-fixed", () => {
    const { rerender } = render(<Spinner container />);
    const wrap = screen.getByRole("status").parentElement!;
    expect(wrap.className).toBe("slds-spinner_container");
    rerender(<Spinner container="fixed" />);
    expect(screen.getByRole("status").parentElement!.className).toBe("slds-spinner_container slds-is-fixed");
  });
});
