import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { ChangeList } from "./ChangeList";

const items = [
  { op: "add", text: "Luật gửi thư" },
  { op: "change", text: "Báo cáo doanh thu" },
  { op: "remove", text: "Bảng cũ" },
  { op: "same", text: "Thương hiệu" },
] as const;

describe("ChangeList", () => {
  it("shows the four marks + ~ − = and hides them from screen readers", () => {
    const { container } = render(<ChangeList items={[...items]} />);
    const marks = [...container.querySelectorAll("[data-mark]")];
    expect(marks.map((m) => m.textContent)).toEqual(["+", "~", "−", "="]);
    for (const m of marks) expect(m.getAttribute("aria-hidden")).toBe("true");
  });

  it("gives each row a word for screen readers", () => {
    render(<ChangeList items={[...items]} />);
    const rows = screen.getAllByRole("listitem");
    expect(within(rows[0]).getByText("Thêm").className).toContain("slds-assistive-text");
    expect(within(rows[1]).getByText("Sửa")).toBeTruthy();
    expect(within(rows[2]).getByText("Xoá")).toBeTruthy();
    expect(within(rows[3]).getByText("Giữ nguyên")).toBeTruthy();
    expect(rows[0].textContent).toContain("Luật gửi thư");
  });

  // issue #23: a long line (a rule's condition, a grant, a path) wraps instead of being cut to one line.
  it("a long line wraps, the mark stays on its first line", () => {
    render(<ChangeList items={[{ op: "add", text: "Nếu Trạng thái bằng Đã đóng và Mức độ ưu tiên bằng Cao thì gửi email cho người phụ trách" }]} />);
    const row = screen.getByRole("listitem");
    const text = within(row).getByText(/người phụ trách/);
    expect(text.className).not.toContain("slds-truncate");
    expect(text.className).toContain("slds-hyphenate");
    expect(row.className).toContain("slds-grid_vertical-align-start");
  });

  it("tone=proposed marks the list as an assistant proposal", () => {
    const { container } = render(<ChangeList items={[items[0]]} tone="proposed" />);
    expect(container.querySelector("ul")?.className).toContain("nx-proposed");
  });
});
