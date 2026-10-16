// Alert — blueprint alert/base (SLDS 2.264.1): default (info), warning, error, offline.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { UserRound } from "lucide-react";
import { Alert } from "./Alert";

describe("Alert", () => {
  it("renders the default (info) blueprint markup", () => {
    const { container } = render(<Alert onClose={() => {}}>Bạn đang xem dữ liệu của chi nhánh Hà Nội.</Alert>);
    const el = container.firstElementChild!;
    expect(el).toHaveClass("slds-notify", "slds-notify_alert");
    expect(el.className).not.toMatch(/slds-alert_|slds-theme_/);
    expect(el).toHaveAttribute("role", "alert");
    expect(el.children[0]).toHaveClass("slds-assistive-text");
    expect(el.children[0]).toHaveTextContent("Thông tin");
    const icon = el.children[1];
    expect(icon).toHaveClass("slds-icon_container", "slds-icon-utility-info", "slds-m-right_x-small");
    expect(icon.querySelector("svg")).toHaveClass("slds-icon", "slds-icon_x-small");
    expect(icon.querySelector("svg")).toHaveAttribute("aria-hidden", "true");
    expect(el.children[2].tagName).toBe("H2");
    expect(el.children[2]).toHaveTextContent("Bạn đang xem dữ liệu của chi nhánh Hà Nội.");
    const close = el.querySelector(".slds-notify__close button")!;
    expect(close).toHaveClass("slds-button", "slds-button_icon", "slds-button_icon-small", "slds-button_icon-inverse");
    expect(close).toHaveAttribute("title", "Đóng");
  });

  it.each([
    ["warning", "slds-alert_warning", "Cảnh báo"],
    ["error", "slds-alert_error", "Lỗi"],
    ["offline", "slds-alert_offline", "Ngoại tuyến"],
  ] as const)("renders the %s level", (variant, cls, label) => {
    const { container } = render(<Alert variant={variant}>x</Alert>);
    const el = container.firstElementChild!;
    expect(el).toHaveClass("slds-notify", "slds-notify_alert", cls);
    expect(el.querySelector(".slds-assistive-text")).toHaveTextContent(label);
    expect(el.querySelector(".slds-icon_container")).toHaveClass(`slds-icon-utility-${variant}`);
  });

  it("uses a non-inverse close button on the light warning background", () => {
    const { container } = render(
      <Alert variant="warning" onClose={() => {}}>
        x
      </Alert>,
    );
    const close = container.querySelector(".slds-notify__close button")!;
    expect(close).toHaveClass("slds-button_icon-small");
    expect(close).not.toHaveClass("slds-button_icon-inverse");
  });

  it("closes from the close button and has none without onClose", async () => {
    const onClose = vi.fn();
    const { rerender } = render(<Alert onClose={onClose}>x</Alert>);
    await userEvent.click(screen.getByRole("button", { name: "Đóng" }));
    expect(onClose).toHaveBeenCalledTimes(1);
    rerender(<Alert>x</Alert>);
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("takes a custom glyph and keeps links inside the heading", () => {
    const { container } = render(
      <Alert icon={UserRound} iconName="user">
        Đang đăng nhập với tài khoản an.nguyen. <a href="#">Đăng xuất</a>
      </Alert>,
    );
    expect(container.querySelector(".slds-icon_container")).toHaveClass("slds-icon-utility-user");
    expect(screen.getByRole("link", { name: "Đăng xuất" }).closest("h2")).not.toBeNull();
  });
});
