// specs/078 T006 (u7-nen-nhung-mau-trang): inside the Studio frame nothing opens a dialog (D-UI-28).
import { useState } from "react";
import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { ConfirmPrompt } from "@/components/studio/workspaceShared";

const props = { onOpenChange: () => {}, title: "Xoá khoá?", confirmLabel: "Xoá", onConfirm: () => {} };

describe("ConfirmPrompt — nhúng", () => {
  it("ngoài EmbeddedSurface vẫn là Prompt (có role=dialog)", () => {
    render(<ConfirmPrompt {...props} open />);
    expect(document.querySelector("[role=dialog],[role=alertdialog]")).not.toBeNull();
  });

  it("trong EmbeddedSurface là InlineConfirm tại chỗ: không dialog, cuộn vào tầm nhìn, Esc huỷ, focus nút an toàn", () => {
    const scroll = vi.fn();
    Element.prototype.scrollIntoView = scroll;
    const onOpenChange = vi.fn();
    render(
      <EmbeddedSurface>
        <ConfirmPrompt {...props} onOpenChange={onOpenChange} open description="Không hoàn tác được." />
      </EmbeddedSurface>,
    );
    expect(document.querySelector("[role=dialog],[role=alertdialog]")).toBeNull();
    expect(screen.getByText("Xoá khoá?")).toBeInTheDocument();
    expect(screen.getByText("Không hoàn tác được.")).toBeInTheDocument();
    expect(scroll).toHaveBeenCalledWith({ block: "nearest" });
    expect(screen.getByRole("button", { name: "Huỷ" })).toHaveFocus();
    fireEvent.keyDown(screen.getByRole("button", { name: "Huỷ" }), { key: "Escape" });
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("đóng thì focus về nút đã mở; confirmName khoá nút tới khi gõ đúng", () => {
    Element.prototype.scrollIntoView = vi.fn();
    const onConfirm = vi.fn();
    function Host() {
      const [open, setOpen] = useState(false);
      return (
        <EmbeddedSurface>
          <button onClick={() => setOpen(true)}>Mở</button>
          <ConfirmPrompt {...props} onConfirm={onConfirm} open={open} onOpenChange={setOpen} confirmName="acme" />
        </EmbeddedSurface>
      );
    }
    render(<Host />);
    const opener = screen.getByRole("button", { name: "Mở" });
    opener.focus();
    fireEvent.click(opener);
    expect(screen.getByRole("button", { name: "Xoá" })).toBeDisabled();
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "acme" } });
    expect(screen.getByRole("button", { name: "Xoá" })).toBeEnabled();
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(opener).toHaveFocus();
    expect(onConfirm).not.toHaveBeenCalled();
  });
});
