// VisualPicker — blueprint `visual-picker` coverable-content / vertical (SLDS 2.264.1).
import { describe, it, expect } from "vitest";
import { useState } from "react";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Building2, Users } from "lucide-react";
import { VisualPicker, type VisualPickerOption } from "@/components/slds/VisualPicker";

const APPS: VisualPickerOption[] = [
  { value: "crm", label: "CRM", icon: Building2, iconName: "account" },
  { value: "lms", label: "Đào tạo", icon: Users },
  { value: "desk", label: "Hỗ trợ", icon: Users, disabled: true },
];

function Single() {
  const [v, setV] = useState<string | null>("crm");
  return <VisualPicker label="Chọn ứng dụng" options={APPS} value={v} onChange={setV} />;
}

describe("VisualPicker", () => {
  it("coverable: fieldset[role=radiogroup] > legend + slds-visual-picker_medium > radio + label figure/body", () => {
    render(<Single />);
    const group = screen.getByRole("radiogroup", { name: "Chọn ứng dụng" });
    expect(group.tagName).toBe("FIELDSET");
    expect(group.querySelector("legend")).toHaveClass("slds-form-element__legend", "slds-form-element__label");
    const radio = screen.getByRole("radio", { name: "CRM" });
    const picker = radio.parentElement!;
    expect(picker).toHaveClass("slds-visual-picker", "slds-visual-picker_medium");
    const figure = picker.querySelector("label > .slds-visual-picker__figure")!;
    expect(figure).toHaveClass("slds-visual-picker__icon", "slds-align_absolute-center");
    expect(figure.querySelector(".slds-is-selected .slds-icon-action-check")).not.toBeNull();
    expect(figure.querySelector(".slds-is-not-selected .slds-icon-standard-account")).not.toBeNull();
    expect(picker.querySelector(".slds-visual-picker__body .slds-text-title")).toHaveTextContent("CRM");
  });

  it("choosing another native radio moves the check; a disabled option stays disabled", async () => {
    render(<Single />);
    expect(screen.getByRole("radio", { name: "CRM" })).toBeChecked();
    await userEvent.click(screen.getByRole("radio", { name: "Đào tạo" }));
    expect(screen.getByRole("radio", { name: "Đào tạo" })).toBeChecked();
    expect(screen.getByRole("radio", { name: "CRM" })).not.toBeChecked();
    expect(screen.getByRole("radio", { name: "Hỗ trợ" })).toBeDisabled();
  });

  it("disabled glyphs sit where the grey-out glue reaches them (SLDS fills them neutral-base-80; Lucide needs `color`)", () => {
    // Keep in sync with the cssGlue rule in app/globals.css: SLDS greys `.slds-icon` by `fill`, which
    // Lucide (fill:none) ignores, so the glue repeats SLDS's selector for `color` on .nx-lucide glyphs.
    const GREY = ".slds-visual-picker input[disabled] ~ label .slds-visual-picker__figure .nx-lucide.slds-icon";
    render(<VisualPicker label="Chọn ứng dụng" options={[...APPS, { value: "own", label: "Ứng dụng riêng", icon: Building2, iconName: "custom", disabled: true }]} value="crm" />);
    const glyphs = (name: string) => Array.from(screen.getByRole("radio", { name }).parentElement!.querySelectorAll("svg"));
    for (const name of ["Hỗ trợ", "Ứng dụng riêng"]) {
      const [check, glyph] = glyphs(name);
      expect(check.matches(GREY)).toBe(true);
      expect(glyph.matches(GREY)).toBe(true);
    }
    expect(glyphs("Ứng dụng riêng")[1]).toHaveClass("slds-icon-standard-custom", "nx-lucide_tile");
    for (const svg of glyphs("Đào tạo")) expect(svg.matches(GREY)).toBe(false);
  });

  it("multiple: checkboxes (no radiogroup role), Space toggles membership", async () => {
    function Multi() {
      const [v, setV] = useState<string[]>(["crm"]);
      return (
        <>
          <VisualPicker label="Đối tượng" options={APPS} multiple value={v} onChange={setV} size="small" />
          <output>{v.join(",")}</output>
        </>
      );
    }
    render(<Multi />);
    expect(screen.queryByRole("radiogroup")).toBeNull();
    const lms = screen.getByRole("checkbox", { name: "Đào tạo" });
    expect(lms.parentElement).toHaveClass("slds-visual-picker_small");
    lms.focus();
    await userEvent.keyboard(" ");
    expect(screen.getByRole("status")).toHaveTextContent("crm,lms");
    await userEvent.click(screen.getByRole("checkbox", { name: "CRM" }));
    expect(screen.getByRole("status")).toHaveTextContent("lms");
  });

  it("vertical: text figure with title + description and the text-check corner", () => {
    render(
      <VisualPicker
        label="Kiểu trường"
        variant="vertical"
        options={[{ value: "text", label: "Văn bản", description: "Một dòng chữ tự do" }]}
      />,
    );
    const picker = screen.getByRole("radio").parentElement!;
    expect(picker).toHaveClass("slds-visual-picker", "slds-visual-picker_vertical");
    const figure = picker.querySelector(".slds-visual-picker__figure")!;
    expect(figure).toHaveClass("slds-visual-picker__text");
    expect(figure.querySelector(".slds-text-heading_medium")).toHaveTextContent("Văn bản");
    expect(figure.querySelector(".slds-text-title")).toHaveTextContent("Một dòng chữ tự do");
    const check = picker.querySelector<SVGElement>(".slds-visual-picker__text-check .slds-icon-text-check")!;
    // White glyph like the sprite's default fill — not the dark currentColor the Lucide glue falls back to.
    expect(check.style.getPropertyValue("--slds-c-icon-color-foreground")).toBe("var(--slds-g-color-neutral-base-100)");
  });

  it("error: slds-has-error on the fieldset, help line described by each input", () => {
    render(<VisualPicker label="Chọn ứng dụng" options={APPS} error="Chọn một ứng dụng" required />);
    const group = screen.getByRole("radiogroup");
    expect(group).toHaveClass("slds-has-error");
    const help = group.querySelector(".slds-form-element__help")!;
    expect(help).toHaveTextContent("Chọn một ứng dụng");
    expect(screen.getByRole("radio", { name: "CRM" })).toHaveAttribute("aria-describedby", help.id);
  });
});
