// Popover — blueprint popovers/base (SLDS 2.264.1) on @radix-ui/react-popover.
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Star } from "lucide-react";
import { Popover, nubbin } from "./Popover";

const trigger = (
  <button type="button" className="slds-button slds-button_neutral">
    Chi tiết
  </button>
);

const open = () => userEvent.click(screen.getByRole("button", { name: "Chi tiết" }));

// Direct child matching `selector` (happy-dom has no `:scope >`).
const child = (el: Element, selector: string) => Array.from(el.children).find((c) => c.matches(selector))!;

describe("nubbin", () => {
  it("points the nubbin back at the trigger for every placement", () => {
    expect(nubbin("right", "center", "--t").className).toBe("slds-nubbin_left");
    expect(nubbin("left", "center", "--t").className).toBe("slds-nubbin_right");
    expect(nubbin("bottom", "center", "--t").className).toBe("slds-nubbin_top");
    expect(nubbin("top", "center", "--t").className).toBe("slds-nubbin_bottom");
    expect(nubbin("top", "start", "--t").className).toBe("slds-nubbin_bottom-left");
    expect(nubbin("top", "end", "--t").className).toBe("slds-nubbin_bottom-right");
    expect(nubbin("bottom", "start", "--t").className).toBe("slds-nubbin_top-left");
    expect(nubbin("bottom", "end", "--t").className).toBe("slds-nubbin_top-right");
    expect(nubbin("right", "start", "--t").className).toBe("slds-nubbin_left-top");
    expect(nubbin("left", "end", "--t").className).toBe("slds-nubbin_right-bottom");
  });

  it("shifts an edge-aligned popover so the corner nubbin sits on the trigger's centre", () => {
    expect(nubbin("top", "center", "--t").style).toBeUndefined();
    expect(nubbin("top", "start", "--t").style).toEqual({ marginLeft: "calc(var(--t-width) / 2 - 1.5rem)" });
    expect(nubbin("bottom", "end", "--t").style).toEqual({ marginRight: "calc(var(--t-width) / 2 - 1.5rem)" });
    expect(nubbin("right", "start", "--t").style).toEqual({ marginTop: "calc(var(--t-height) / 2 - 1.5rem)" });
    expect(nubbin("left", "end", "--t").style).toEqual({ marginBottom: "calc(var(--t-height) / 2 - 1.5rem)" });
  });
});

describe("Popover", () => {
  it("opens the blueprint section with header, icon, badge and footer", async () => {
    render(
      <Popover trigger={trigger} title="Tiêu đề" icon={Star} iconLabel="Đánh dấu" badge="Thông báo quản trị" footer={<button type="button" className="slds-button slds-button_neutral">Tìm hiểu thêm</button>}>
        <p>Nội dung</p>
      </Popover>,
    );
    const btn = screen.getByRole("button", { name: "Chi tiết" });
    expect(btn).toHaveAttribute("aria-haspopup", "dialog");
    expect(btn).toHaveAttribute("aria-expanded", "false");
    await open();
    expect(btn).toHaveAttribute("aria-expanded", "true");

    const dialog = screen.getByRole("dialog", { name: "Tiêu đề" });
    expect(dialog.tagName).toBe("SECTION");
    expect(dialog).toHaveClass("slds-popover");
    expect(dialog).not.toHaveClass("slds-popover_dark");
    // Nubbin follows the side/align Radix actually placed it on.
    expect(dialog).toHaveClass(nubbin(dialog.dataset.side as "right", dialog.dataset.align as "center", "").className);

    const body = child(dialog, ".slds-popover__body");
    expect(dialog).toHaveAttribute("aria-describedby", body.id);
    expect(child(child(body, ".slds-popover__meta"), ".slds-badge")).toHaveTextContent("Thông báo quản trị");
    const media = child(body, ".slds-media");
    const figure = child(child(media, ".slds-media__figure"), ".slds-icon_container");
    expect(figure).toHaveAttribute("title", "Đánh dấu");
    expect(figure.querySelector("svg")).toHaveClass("slds-icon", "slds-icon_small", "slds-icon-text-default");
    const heading = child(child(child(media, ".slds-media__body"), "header.slds-popover__header"), "h2");
    expect(heading).toHaveClass("slds-text-heading_small");
    expect(dialog).toHaveAttribute("aria-labelledby", heading.id);
    expect(media.querySelector(".slds-media__body")).toHaveTextContent("Nội dung");

    const close = child(dialog, "button.slds-popover__close");
    expect(close).toHaveClass("slds-button", "slds-button_icon", "slds-button_icon-small", "slds-float_right");
    expect(close).toHaveAttribute("title", "Đóng hộp thoại");
    expect(close.querySelector(".slds-assistive-text")).toHaveTextContent("Đóng hộp thoại");

    const footer = child(dialog, "footer.slds-popover__footer");
    expect(footer.firstElementChild).toHaveTextContent("Tìm hiểu thêm");
  });

  it("names a headless popover with aria-label and applies dark/size modifiers", async () => {
    render(
      <Popover trigger={trigger} label="Gợi ý" dark size="small">
        <p>Nội dung</p>
      </Popover>,
    );
    await open();
    const dialog = screen.getByRole("dialog", { name: "Gợi ý" });
    expect(dialog).toHaveAttribute("aria-label", "Gợi ý");
    expect(dialog).not.toHaveAttribute("aria-labelledby");
    expect(dialog).toHaveClass("slds-popover_dark", "slds-popover_small");
    expect(dialog.querySelector("header")).toBeNull();
  });

  it("focuses the first control that is not the close button, Esc closes and returns focus", async () => {
    render(
      <Popover trigger={trigger} title="Tiêu đề" footer={<button type="button">Tìm hiểu thêm</button>}>
        <p>Nội dung</p>
      </Popover>,
    );
    await open();
    expect(screen.getByRole("button", { name: "Tìm hiểu thêm" })).toHaveFocus();
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByRole("button", { name: "Chi tiết" })).toHaveFocus();
  });

  it("falls back to the close button, which closes the popover", async () => {
    render(
      <Popover trigger={trigger} title="Tiêu đề">
        <p>Nội dung</p>
      </Popover>,
    );
    await open();
    const close = screen.getByRole("button", { name: "Đóng hộp thoại" });
    expect(close).toHaveFocus();
    await userEvent.keyboard("{Enter}");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByRole("button", { name: "Chi tiết" })).toHaveFocus();
  });

  it("drops the close button on request and makes a small scrolling body focusable", async () => {
    render(
      <Popover trigger={trigger} title="Tiêu đề" closeButton={false} scrollable>
        <p>Nội dung</p>
      </Popover>,
    );
    await open();
    const dialog = screen.getByRole("dialog");
    expect(dialog.querySelector(".slds-popover__close")).toBeNull();
    const body = screen.getByRole("region");
    expect(body).toHaveClass("slds-popover__body", "slds-popover__body_small");
    expect(body).toHaveAttribute("tabindex", "0");
    expect(body).toHaveFocus();
  });

  it("puts the header outside the body on request (showcase headerTitle) with body classes", async () => {
    render(
      <Popover trigger={trigger} title="Thông báo" headerOutside bodyClassName="slds-p-around_none" scrollable>
        <ul>
          <li>Dòng</li>
        </ul>
      </Popover>,
    );
    await open();
    const dialog = screen.getByRole("dialog", { name: "Thông báo" });
    expect(Array.from(dialog.children).map((c) => c.tagName)).toEqual(["BUTTON", "HEADER", "DIV"]);
    expect(child(dialog, "header")).toHaveClass("slds-popover__header");
    const body = child(dialog, "div");
    expect(body).toHaveClass("slds-popover__body", "slds-popover__body_small", "slds-p-around_none");
    expect(body.firstElementChild!.tagName).toBe("UL");
    expect(dialog.querySelector(".slds-media")).toBeNull();
  });
});
