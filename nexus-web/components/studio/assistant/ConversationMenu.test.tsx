import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { ConversationMenu } from "./ConversationMenu";

const S = [
  { id: "a", title: "Phòng khám", updatedAt: new Date().toISOString() },
  { id: "b", title: "", updatedAt: new Date().toISOString() },
];
const base = { sessions: S, loading: false, activeId: null as string | null, onRetry: vi.fn(), onNew: vi.fn(), onOpen: vi.fn(), onDelete: vi.fn() };

describe("ConversationMenu", () => {
  it("lists sessions, opens one and starts a new one", async () => {
    const onOpen = vi.fn();
    const onNew = vi.fn();
    render(<ConversationMenu {...base} onOpen={onOpen} onNew={onNew} />);
    await userEvent.click(screen.getByRole("button", { name: "Cuộc trò chuyện" }));
    expect(screen.getByText(/Cuộc trò chuyện không tên/)).toBeTruthy();
    await userEvent.click(screen.getByRole("menuitem", { name: /Phòng khám/ }));
    expect(onOpen).toHaveBeenCalledWith("a");
    await userEvent.click(screen.getByRole("button", { name: "Cuộc trò chuyện" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Cuộc trò chuyện mới" }));
    expect(onNew).toHaveBeenCalled();
  });
  it("shows empty and load-error states with retry", async () => {
    const onRetry = vi.fn();
    const { rerender } = render(<ConversationMenu {...base} sessions={[]} />);
    await userEvent.click(screen.getByRole("button", { name: "Cuộc trò chuyện" }));
    expect(screen.getByText("Chưa có cuộc trò chuyện nào.")).toBeTruthy();
    rerender(<ConversationMenu {...base} sessions={[]} error onRetry={onRetry} />);
    await userEvent.click(screen.getByRole("button", { name: "Thử lại" }));
    expect(onRetry).toHaveBeenCalled();
  });
  it("asks in the page before deleting the open conversation, Huỷ keeps it", async () => {
    const onDelete = vi.fn();
    render(<ConversationMenu {...base} activeId="a" onDelete={onDelete} />);
    await userEvent.click(screen.getByRole("button", { name: "Cuộc trò chuyện" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Xoá cuộc trò chuyện đang mở" }));
    expect(onDelete).not.toHaveBeenCalled();
    expect(screen.getByText(/Xoá cuộc trò chuyện "Phòng khám"\?/)).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(onDelete).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Cuộc trò chuyện" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Xoá cuộc trò chuyện đang mở" }));
    await userEvent.click(screen.getByRole("button", { name: "Xoá cuộc trò chuyện đang mở" }));
    expect(onDelete).toHaveBeenCalledWith("a");
  });
});
