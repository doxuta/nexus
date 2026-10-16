// EmptyState — blueprint illustration/base (SLDS 2.264.1) frame + text; the picture is a Lucide glyph.
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { SearchX } from "lucide-react";
import { EmptyState } from "./EmptyState";

describe("EmptyState", () => {
  it("renders the illustration frame with heading and body text", () => {
    const { container } = render(<EmptyState title="Chưa có khách hàng nào" description="Tạo khách hàng đầu tiên để bắt đầu." />);
    const el = container.firstElementChild!;
    expect(el).toHaveClass("slds-illustration", "slds-illustration_small");
    const svg = el.children[0];
    expect(svg.tagName.toLowerCase()).toBe("svg");
    expect(svg).toHaveClass("slds-illustration__svg");
    expect(svg).toHaveAttribute("aria-hidden", "true");
    const text = el.children[1];
    expect(text).toHaveClass("slds-text-longform");
    expect(text.querySelector("h3")).toHaveClass("slds-text-heading_medium");
    expect(text.querySelector("h3")).toHaveTextContent("Chưa có khách hàng nào");
    expect(text.querySelector("p")).toHaveClass("slds-text-body_regular");
    expect(text.querySelector("p")).toHaveTextContent("Tạo khách hàng đầu tiên để bắt đầu.");
  });

  it("renders the headline-only variant and the large size", () => {
    const { container } = render(<EmptyState size="large" title="Không có dữ liệu" />);
    const el = container.firstElementChild!;
    expect(el).toHaveClass("slds-illustration", "slds-illustration_large");
    expect(el.querySelector("p")).toBeNull();
  });

  it("renders a custom glyph and the action below the text", () => {
    const { container } = render(
      <EmptyState
        icon={SearchX}
        title="Không tìm thấy kết quả"
        action={
          <button type="button" className="slds-button slds-button_neutral">
            Xoá bộ lọc
          </button>
        }
      />,
    );
    expect(container.querySelector("svg")).toHaveClass("lucide-search-x");
    const btn = screen.getByRole("button", { name: "Xoá bộ lọc" });
    expect(container.querySelector(".slds-text-longform")!.compareDocumentPosition(btn) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("plain: no glyph and no illustration frame, left-aligned, one h3 + one sentence + at most one button", () => {
    const { container } = render(
      <EmptyState plain title="Chưa có bảng nào" description="Bảng là nơi khách lưu thông tin." action={<button type="button">Thêm bảng</button>} />,
    );
    expect(container.querySelector("svg")).toBeNull();
    expect(container.querySelector(".slds-illustration")).toBeNull();
    expect(container.querySelector(".slds-illustration__svg")).toBeNull();
    expect(container.querySelectorAll("h3")).toHaveLength(1);
    expect(container.querySelector("h3")).toHaveClass("slds-text-heading_medium");
    expect(container.querySelectorAll("p")).toHaveLength(1);
    expect(container.querySelectorAll("button")).toHaveLength(1);
    expect(container.querySelector(".slds-text-longform")).not.toBeNull();
    expect(container.firstElementChild).not.toHaveClass("slds-text-align_center");
  });
});
