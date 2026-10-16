// ProgressIndicator — blueprints progress-indicator/base (horizontal) and /vertical (SLDS 2.264.1).
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ProgressIndicator } from "./ProgressIndicator";

const STEPS = ["Thông tin chung", "Liên hệ", "Xác minh email", "Hợp đồng", "Hoàn tất"].map((label, i) => ({ id: `s${i}`, label }));

const items = (container: HTMLElement) => Array.from(container.querySelectorAll("li.slds-progress__item"));

describe("ProgressIndicator (horizontal)", () => {
  it("renders the blueprint markup: completed, active and upcoming markers plus the x-small bar", () => {
    const { container } = render(<ProgressIndicator steps={STEPS} current={2} />);
    const root = container.firstElementChild!;
    expect(root).toHaveClass("slds-progress");
    expect(root).not.toHaveClass("slds-progress_vertical");
    expect(root.querySelector("ol")).toHaveClass("slds-progress__list");
    const li = items(container);
    expect(li).toHaveLength(5);

    const done = li[0].querySelector(".slds-progress__marker")!;
    expect(li[0]).toHaveClass("slds-is-completed");
    expect(done.tagName).toBe("DIV");
    expect(done).toHaveClass("slds-progress__marker_icon");
    expect(done).toHaveAttribute("tabindex", "0");
    expect(done.querySelector("svg")).toHaveClass("slds-icon", "slds-icon_xx-small");
    expect(done.querySelector(".slds-assistive-text")).toHaveTextContent("Thông tin chung - Đã hoàn tất");

    const active = li[2].querySelector(".slds-progress__marker")!;
    expect(li[2]).toHaveClass("slds-is-active");
    expect(active).not.toHaveClass("slds-progress__marker_icon");
    expect(active.querySelector("svg")).toBeNull();
    expect(active.querySelector(".slds-assistive-text")).toHaveTextContent("Xác minh email - Đang thực hiện");

    expect(li[3].className).toBe("slds-progress__item");
    expect(li[3].querySelector(".slds-assistive-text")).toHaveTextContent(/^Hợp đồng$/);

    const bar = root.querySelector('[role="progressbar"]')!;
    expect(bar).toHaveClass("slds-progress-bar", "slds-progress-bar_x-small");
    expect(bar).toHaveAttribute("aria-valuenow", "50");
    expect(bar.querySelector(".slds-progress-bar__value")).toHaveStyle({ width: "50%" });
  });

  it("marks the current step as errored and computes the blueprint's 25%", () => {
    const { container } = render(<ProgressIndicator steps={STEPS} current={1} error />);
    const li = items(container);
    expect(li[1]).toHaveClass("slds-has-error");
    expect(li[1]).not.toHaveClass("slds-is-active");
    const marker = li[1].querySelector(".slds-progress__marker")!;
    expect(marker).toHaveClass("slds-progress__marker_icon");
    expect(marker.querySelector("svg")).not.toBeNull();
    expect(marker.querySelector(".slds-assistive-text")).toHaveTextContent("Liên hệ - Có lỗi");
    expect(container.querySelector('[role="progressbar"]')).toHaveAttribute("aria-valuenow", "25");
  });

  it("shows the step name in a tooltip when a marker gets keyboard focus", async () => {
    render(<ProgressIndicator steps={STEPS} current={2} />);
    await userEvent.tab();
    await userEvent.tab();
    await userEvent.tab();
    const tip = await screen.findByRole("tooltip");
    expect(tip).toHaveTextContent("Xác minh email");
    expect(tip.closest(".slds-popover")).toHaveClass("slds-popover_tooltip");
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("adds the shade modifier", () => {
    const { container } = render(<ProgressIndicator steps={STEPS} current={0} shade />);
    expect(container.firstElementChild).toHaveClass("slds-progress", "slds-progress_shade");
    expect(container.querySelector('[role="progressbar"]')).toHaveAttribute("aria-valuenow", "0");
  });
});

describe("ProgressIndicator (vertical)", () => {
  it("renders non-interactive markers with the step names beside them", () => {
    const { container } = render(<ProgressIndicator steps={STEPS} current={1} vertical />);
    const root = container.firstElementChild!;
    expect(root).toHaveClass("slds-progress", "slds-progress_vertical");
    const li = items(container);

    const done = li[0].children[0];
    expect(done.tagName).toBe("SPAN");
    expect(done).toHaveClass("slds-icon_container", "slds-icon-utility-success", "slds-progress__marker", "slds-progress__marker_icon");
    expect(done).not.toHaveClass("slds-progress__marker_icon-success");
    expect(done).toHaveAttribute("title", "Đã hoàn tất");
    expect(done.querySelector("svg")).toHaveClass("slds-icon", "slds-icon_xx-small");
    expect(li[0].children[1]).toHaveClass("slds-progress__item_content", "slds-grid", "slds-grid_align-spread");
    expect(li[0].children[1]).toHaveTextContent("Thông tin chung");

    const active = li[1].children[0];
    expect(active).toHaveClass("slds-progress__marker");
    expect(active).toHaveTextContent("Đang thực hiện");
    expect(li[2].children[0]).toBeEmptyDOMElement();
    for (const m of container.querySelectorAll(".slds-progress__marker")) expect(m).not.toHaveAttribute("tabindex");

    const bar = root.querySelector('[role="progressbar"]')!;
    expect(bar).not.toHaveClass("slds-progress-bar");
    expect(bar).toHaveAttribute("aria-valuenow", "25");
    expect(bar).toHaveTextContent("Tiến độ: 25%");
  });

  it("renders the green success check and the error marker", () => {
    const { container, rerender } = render(<ProgressIndicator steps={STEPS} current={2} vertical success />);
    expect(items(container)[0].children[0]).toHaveClass("slds-progress__marker_icon-success");
    rerender(<ProgressIndicator steps={STEPS} current={1} vertical error />);
    const err = items(container)[1];
    expect(err).toHaveClass("slds-has-error");
    expect(err.children[0]).toHaveClass("slds-icon_container", "slds-icon-utility-error", "slds-progress__marker", "slds-progress__marker_icon");
    expect(err.children[0]).toHaveAttribute("title", "Có lỗi");
  });

  it("puts a step's description as a weak line under its name", () => {
    const steps = [{ id: "a", label: "Trưởng phòng duyệt", description: "Trần B · Người dùng" }, { id: "b", label: "Giám đốc" }];
    const { container } = render(<ProgressIndicator steps={steps} current={0} vertical />);
    const [first, second] = items(container).map((li) => li.children[1]);
    expect(screen.getByText("Trưởng phòng duyệt")).toBe(first.firstElementChild);
    expect(first.querySelector("p")).toHaveClass("slds-text-body_small", "slds-text-color_weak");
    expect(first.querySelector("p")).toHaveTextContent("Trần B · Người dùng");
    expect(second.children).toHaveLength(0); // no description: the name stays bare text, as in the blueprint
    expect(second).toHaveTextContent("Giám đốc");
  });
});
