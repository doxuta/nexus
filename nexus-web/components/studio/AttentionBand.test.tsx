// specs/073 so1a T452 (FR-091, architecture 3.2): the navy "Cần anh xem" band — the one bold thing on the page.
import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { AttentionItem } from "@/lib/projectSignals";
import { AttentionBand } from "./AttentionBand";

const item = (slug: string, level: AttentionItem["level"], text: string, route: AttentionItem["action"]["route"] = "bao-tri/kham", label = "Mở khám sức khoẻ"): AttentionItem => ({
  level,
  projectSlug: slug,
  projectName: `Dự án ${slug.toUpperCase()}`,
  text,
  action: { label, route },
});

describe("AttentionBand", () => {
  it("draws nothing when there is nothing to look at", () => {
    const { container } = render(<AttentionBand items={[]} onOpenProject={() => {}} onAction={() => {}} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("is a labelled region on the inverse theme, counting projects not lines", () => {
    const { container } = render(
      <AttentionBand items={[item("a", "error", "2 lỗi"), item("a", "warning", "Chưa có chủ khách đăng nhập được"), item("b", "info", "Sẵn sàng giao.")]} onOpenProject={() => {}} onAction={() => {}} />,
    );
    const region = screen.getByRole("region", { name: "Cần anh xem" });
    expect(region).toHaveClass("slds-theme_alt-inverse");
    expect(within(region).getByText("2 dự án")).toBeInTheDocument();
    expect(container.querySelector("h2")).toHaveTextContent("Cần anh xem");
  });

  it("one column per project, its lines in the order given, each with a severity dot and an action", () => {
    render(<AttentionBand items={[item("a", "error", "2 lỗi"), item("b", "warning", "Cảnh báo B"), item("a", "info", "Thông tin A")]} onOpenProject={() => {}} onAction={() => {}} />);
    const cols = screen.getAllByRole("listitem").filter((li) => li.hasAttribute("data-project"));
    expect(cols.map((c) => c.getAttribute("data-project"))).toEqual(["a", "b"]);
    const colA = within(cols[0]);
    expect(colA.getAllByRole("button", { name: "Mở khám sức khoẻ" })).toHaveLength(2);
    expect(cols[0].querySelectorAll(".nx-dot_error")).toHaveLength(1);
    expect(cols[0].querySelectorAll(".nx-dot_info")).toHaveLength(1);
    expect(cols[1].querySelectorAll(".nx-dot_warning")).toHaveLength(1);
    // the dot is decoration: the level is also in the text for a screen reader
    expect(cols[0].querySelector(".nx-dot")).toHaveAttribute("aria-hidden", "true");
    expect(colA.getByText("Lỗi")).toHaveClass("slds-assistive-text");
  });

  it("the project name opens the project; an action hands its item to the caller", () => {
    const onOpenProject = vi.fn();
    const onAction = vi.fn();
    const it0 = item("a", "error", "2 lỗi", "bao-tri/sao-luu", "Mở sao lưu");
    render(<AttentionBand items={[it0]} onOpenProject={onOpenProject} onAction={onAction} />);
    fireEvent.click(screen.getByRole("button", { name: "Dự án A" }));
    expect(onOpenProject).toHaveBeenCalledWith("a");
    fireEvent.click(screen.getByRole("button", { name: "Mở sao lưu" }));
    expect(onAction).toHaveBeenCalledWith(it0);
  });

  it("under 768px only two projects show; 'Xem thêm' reveals the rest and 'Thu gọn' folds again", () => {
    const items = ["a", "b", "c", "d"].map((s) => item(s, "warning", `Việc ${s}`));
    render(<AttentionBand items={items} onOpenProject={() => {}} onAction={() => {}} />);
    const cols = () => screen.getAllByRole("listitem").filter((li) => li.hasAttribute("data-project"));
    expect(cols().map((c) => c.classList.contains("max-md:hidden"))).toEqual([false, false, true, true]);
    const more = screen.getByRole("button", { name: "Xem thêm 2 dự án" });
    expect(more).toHaveAttribute("aria-expanded", "false");
    expect(more.className).toContain("md:hidden");
    fireEvent.click(more);
    expect(cols().some((c) => c.classList.contains("max-md:hidden"))).toBe(false);
    const less = screen.getByRole("button", { name: "Thu gọn" });
    expect(less).toHaveAttribute("aria-expanded", "true");
    fireEvent.click(less);
    expect(cols().filter((c) => c.classList.contains("max-md:hidden"))).toHaveLength(2);
  });

  it("two projects or fewer: no 'Xem thêm'", () => {
    render(<AttentionBand items={[item("a", "error", "x"), item("b", "error", "y")]} onOpenProject={() => {}} onAction={() => {}} />);
    expect(screen.queryByRole("button", { name: /Xem thêm/ })).toBeNull();
  });
});
