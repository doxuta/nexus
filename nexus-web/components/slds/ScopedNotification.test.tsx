// ScopedNotification — blueprint scoped-notifications/base (SLDS 2.264.1).
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { ScopedNotification } from "./ScopedNotification";

describe("ScopedNotification", () => {
  it("renders the light (default) blueprint markup", () => {
    const { container } = render(
      <ScopedNotification>
        Có vẻ khách hàng này bị trùng. <a href="#">Xem bản trùng</a>
      </ScopedNotification>,
    );
    const el = container.firstElementChild!;
    expect(el).toHaveClass("slds-scoped-notification", "slds-media", "slds-media_center", "slds-scoped-notification_light");
    expect(el).toHaveAttribute("role", "status");
    const icon = el.querySelector(".slds-media__figure > span.slds-icon_container")!;
    expect(icon).toHaveClass("slds-icon-utility-info");
    expect(icon).toHaveAttribute("title", "Thông tin");
    expect(icon.querySelector("svg")).toHaveClass("slds-icon", "slds-icon_small", "slds-icon-text-default");
    expect(icon.querySelector(".slds-assistive-text")).toHaveTextContent("Thông tin");
    expect(el.querySelector(".slds-media__body > p")).toHaveTextContent("Có vẻ khách hàng này bị trùng.");
    expect(screen.getByRole("link", { name: "Xem bản trùng" })).toBeInTheDocument();
  });

  it("renders the dark theme without a text tone on the glyph", () => {
    const { container } = render(<ScopedNotification theme="dark">x</ScopedNotification>);
    const el = container.firstElementChild!;
    expect(el).toHaveClass("slds-scoped-notification_dark");
    expect(el.querySelector("svg")).toHaveClass("slds-icon", "slds-icon_small");
    expect(el.querySelector("svg")).not.toHaveClass("slds-icon-text-default");
  });

  it.each([
    ["info", "Thông tin"],
    ["success", "Thành công"],
    ["warning", "Cảnh báo"],
    ["error", "Lỗi"],
  ] as const)("renders the %s state with its theme and icon", (theme, label) => {
    const { container } = render(<ScopedNotification theme={theme}>x</ScopedNotification>);
    const el = container.firstElementChild!;
    expect(el).toHaveClass("slds-scoped-notification", "slds-media", "slds-media_center", `slds-theme_${theme}`);
    expect(el.className).not.toMatch(/scoped-notification_(light|dark)/);
    const icon = el.querySelector(".slds-icon_container")!;
    expect(icon).toHaveClass(`slds-icon-utility-${theme}`);
    expect(icon).toHaveAttribute("title", label);
    expect(icon.querySelector("svg")).not.toHaveClass("slds-icon-text-default");
    expect(icon.querySelector(".slds-assistive-text")).toHaveTextContent(label);
  });
});
