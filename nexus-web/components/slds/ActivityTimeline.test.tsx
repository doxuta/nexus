// ActivityTimeline — blueprint activity-timeline/base (SLDS 2.264.1): default, expanded, error.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { CalendarDays, ListTodo, Mail, Paperclip, Phone, Repeat } from "lucide-react";
import { ActivityTimeline, type ActivityTimelineItem } from "./ActivityTimeline";

const ITEMS: ActivityTimelineItem[] = [
  {
    id: "t1",
    type: "task",
    icon: ListTodo,
    title: "Gửi báo giá cho Minh Phát",
    date: "09:00 | 20/09/2026",
    summary: "Bạn đã tạo công việc với Nguyễn Văn An",
    meta: [{ icon: Repeat, label: "Công việc lặp lại" }],
    task: { checked: false, onChange: () => {} },
    fields: [
      { label: "Người liên hệ", value: "Nguyễn Văn An" },
      { label: "Liên quan tới", value: "Hợp đồng 2026" },
    ],
    body: { label: "Mô tả", text: "Chốt đơn giá trước cuộc họp." },
  },
  { id: "c1", type: "call", icon: Phone, title: "Gọi xác nhận lịch họp", date: "10:00 | 23/09/2026", error: "Không tải được chi tiết" },
  { id: "e1", type: "email", icon: Mail, title: "Re: Báo giá", date: "10:30 | 24/09/2026", meta: [{ icon: Paperclip, label: "Có tệp đính kèm" }] },
  { id: "v1", type: "event", icon: CalendarDays, title: "Họp chốt hợp đồng", date: "14:00 | 26/09/2026", body: { label: "Mô tả", text: "Họp tại văn phòng khách." } },
];

const lis = (container: HTMLElement) => Array.from(container.querySelector("ul.slds-timeline")!.children);
const item = (container: HTMLElement, i: number) => lis(container)[i].firstElementChild!;

describe("ActivityTimeline", () => {
  it("renders the blueprint item markup with the type tile and connector class", () => {
    const { container } = render(<ActivityTimeline items={ITEMS} />);
    expect(container.firstElementChild).toHaveClass("slds-timeline");
    expect(lis(container)).toHaveLength(4);
    const task = item(container, 0);
    expect(task).toHaveClass("slds-timeline__item_expandable", "slds-timeline__item_task");
    expect(task).not.toHaveClass("slds-is-open");
    expect(task.children[0]).toHaveClass("slds-assistive-text");
    expect(task.children[0]).toHaveTextContent("Công việc");

    const figure = task.querySelector(".slds-media > .slds-media__figure")!;
    const toggle = figure.children[0];
    expect(toggle).toHaveClass("slds-button", "slds-button_icon");
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle).toHaveAttribute("title", "Mở/đóng chi tiết: Gửi báo giá cho Minh Phát");
    expect(toggle.querySelector("svg")).toHaveClass("slds-button__icon", "slds-timeline__details-action-icon");
    const tile = figure.children[1];
    expect(tile).toHaveClass("slds-icon_container", "slds-icon-standard-task", "slds-timeline__icon");
    expect(tile).toHaveAttribute("title", "Công việc");
    expect(tile.querySelector("svg")).toHaveClass("slds-icon", "slds-icon_small");

    const trigger = task.querySelector(".slds-media__body > .slds-timeline__trigger")!;
    expect(trigger).toHaveClass("slds-grid", "slds-grid_align-spread");
    const left = trigger.children[0];
    expect(left).toHaveClass("slds-grid", "slds-grid_vertical-align-center", "slds-truncate_container_75", "slds-no-space");
    expect(left.querySelector(".slds-checkbox input[type=checkbox]")).not.toBeNull();
    const h3 = left.querySelector("h3.slds-truncate")!;
    expect(h3).toHaveAttribute("title", "Gửi báo giá cho Minh Phát");
    expect(h3.querySelector("strong")).toHaveTextContent("Gửi báo giá cho Minh Phát");
    const meta = left.querySelector(".slds-no-flex .slds-icon_container")!;
    expect(meta).toHaveAttribute("title", "Công việc lặp lại");
    expect(meta.querySelector("svg")).toHaveClass("slds-icon", "slds-icon_xx-small", "slds-icon-text-default", "slds-m-left_x-small");
    const actions = trigger.children[1];
    expect(actions).toHaveClass("slds-timeline__actions", "slds-timeline__actions_inline");
    expect(actions.querySelector("p.slds-timeline__date")).toHaveTextContent("09:00 | 20/09/2026");
    expect(task.querySelector(".slds-media__body > p.slds-m-horizontal_xx-small")).toHaveTextContent("Bạn đã tạo công việc với Nguyễn Văn An");

    const details = task.querySelector("article.slds-timeline__item_details")!;
    expect(details).toHaveClass("slds-box", "slds-theme_shade", "slds-m-top_x-small", "slds-m-horizontal_xx-small");
    // Hidden by SLDS CSS (visibility/height 0) until the item is .slds-is-open — see the file header.
    expect(details).not.toHaveClass("slds-p-around_medium");
    expect(toggle).toHaveAttribute("aria-controls", details.id);
    const field = details.querySelector("ul.slds-list_horizontal.slds-wrap > li")!;
    expect(field).toHaveClass("slds-grid", "slds-grid_vertical", "slds-size_1-of-2", "slds-p-bottom_small");
    expect(field.children[0]).toHaveClass("slds-text-title", "slds-p-bottom_x-small");
    expect(field.children[1]).toHaveClass("slds-text-body_medium", "slds-truncate");
    expect(details.querySelector("p.slds-p-top_x-small")).toHaveTextContent("Chốt đơn giá trước cuộc họp.");

    expect(item(container, 1).querySelector(".slds-icon-standard-log-a-call")).not.toBeNull();
    expect(item(container, 1)).toHaveClass("slds-timeline__item_call");
  });

  it("expands and collapses an item from its toggle (click, Enter, Space)", async () => {
    const { container } = render(<ActivityTimeline items={ITEMS} />);
    const task = item(container, 0);
    const toggle = screen.getByRole("button", { name: "Mở/đóng chi tiết: Gửi báo giá cho Minh Phát" });
    const details = task.querySelector("article")!;
    await userEvent.click(toggle);
    expect(task).toHaveClass("slds-is-open");
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(details).toHaveClass("slds-p-around_medium");
    await userEvent.keyboard("{Enter}");
    expect(task).not.toHaveClass("slds-is-open");
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(details).not.toHaveClass("slds-p-around_medium");
    await userEvent.keyboard(" ");
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(task).toHaveClass("slds-is-open");
  });

  it("starts with the given items expanded and reports changes", async () => {
    const onExpandedChange = vi.fn();
    const { container } = render(<ActivityTimeline items={ITEMS} defaultExpanded={["v1"]} onExpandedChange={onExpandedChange} />);
    expect(item(container, 3)).toHaveClass("slds-is-open");
    await userEvent.click(screen.getByRole("button", { name: "Mở/đóng chi tiết: Gửi báo giá cho Minh Phát" }));
    expect(onExpandedChange).toHaveBeenLastCalledWith(["v1", "t1"]);
  });

  it("shows the error line instead of details", async () => {
    const { container } = render(<ActivityTimeline items={ITEMS} defaultExpanded={["c1"]} />);
    const call = item(container, 1);
    expect(call.querySelector("article")).toBeNull();
    const err = call.querySelector(".slds-media__body > div.slds-text-color_error")!;
    expect(err).toHaveClass("slds-grid", "slds-m-top_x-small", "slds-m-horizontal_xx-small");
    expect(err).toHaveTextContent("Không tải được chi tiết");
    expect(err.querySelector(".slds-icon_container.slds-icon-utility-error svg")).toHaveClass("slds-icon", "slds-icon-text-error", "slds-icon_x-small");
    expect(call.querySelector(".slds-media__figure button")).toHaveAttribute("aria-controls", err.id);
  });

  it("disables the toggle when an item has nothing to expand", () => {
    render(<ActivityTimeline items={ITEMS} />);
    const toggle = screen.getByRole("button", { name: "Mở/đóng chi tiết: Re: Báo giá" });
    expect(toggle).toBeDisabled();
    expect(toggle).not.toHaveAttribute("aria-expanded");
  });

  it("toggles the task checkbox with Space", async () => {
    const onChange = vi.fn();
    render(<ActivityTimeline items={[{ ...ITEMS[0], task: { checked: false, onChange } }]} />);
    const box = screen.getByRole("checkbox", { name: "Đánh dấu hoàn thành: Gửi báo giá cho Minh Phát" });
    box.focus();
    await userEvent.keyboard(" ");
    expect(onChange).toHaveBeenCalledWith(true);
  });
});
