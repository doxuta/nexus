// specs/074 T461 / T456 (FR-072): the Huỷ / Lưu bar shows only with unsaved changes, says so in a live region,
// shows the real error of a failed save, and arms the unsaved-changes guard for the same span.
import Link from "next/link";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { NavigationBlockerProvider } from "@/lib/navigation-blocker";
import { UnsavedBar } from "./UnsavedBar";

const bar = (p: Partial<React.ComponentProps<typeof UnsavedBar>> = {}) => (
  <NavigationBlockerProvider>
    <Link href="/elsewhere">đi chỗ khác</Link>
    <UnsavedBar dirty={false} onSave={vi.fn()} onCancel={vi.fn()} {...p} />
  </NavigationBlockerProvider>
);

describe("UnsavedBar", () => {
  it("clean page: no buttons, the live region stays mounted and empty", () => {
    render(bar());
    expect(screen.queryByRole("button", { name: "Lưu" })).toBeNull();
    expect(screen.getByRole("status")).toHaveTextContent("");
  });

  it("dirty page: Huỷ + Lưu and the words 'Có thay đổi chưa lưu' in the live region", async () => {
    const onSave = vi.fn();
    const onCancel = vi.fn();
    render(bar({ dirty: true, onSave, onCancel }));
    expect(screen.getByRole("status")).toHaveTextContent("Có thay đổi chưa lưu");
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await userEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(onSave).toHaveBeenCalledTimes(1);
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("a failed save leaves the bar up with the engine's own sentence", () => {
    render(bar({ dirty: true, error: "Tên luật đã có" }));
    expect(screen.getByRole("alert")).toHaveTextContent("Tên luật đã có");
    expect(screen.getByRole("button", { name: "Lưu" })).toBeInTheDocument();
  });

  it("alwaysShow keeps the bar for a page never saved", () => {
    render(bar({ alwaysShow: true }));
    expect(screen.getByRole("button", { name: "Lưu" })).toBeInTheDocument();
  });

  it("the sticky wrapper sits in the flow (never fixed over the assistant pane, never over the last field)", () => {
    render(bar({ dirty: true }));
    const wrap = screen.getByTestId("unsaved-bar");
    expect(wrap.className).toContain("sticky");
    expect(wrap.className).toContain("[&_.slds-docked-form-footer]:static");
  });

  it("dirty arms the leave guard: a link click asks first", async () => {
    const confirm = vi.fn(() => false);
    window.confirm = confirm;
    render(bar({ dirty: true }));
    await userEvent.click(screen.getByRole("link", { name: "đi chỗ khác" }));
    expect(confirm).toHaveBeenCalled();
  });

  it("clean does not ask", async () => {
    const confirm = vi.fn(() => false);
    window.confirm = confirm;
    render(bar());
    await userEvent.click(screen.getByRole("link", { name: "đi chỗ khác" }));
    expect(confirm).not.toHaveBeenCalled();
  });
});
