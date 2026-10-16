// SavedViews — the list-view switcher (specs/038 T022 review). The save / rename modal autofocuses its
// name input; React focuses it before Radix's FocusScope mounts, so the kit Modal never saw an opener
// and Esc dropped focus on <body>. SLDS modals: closing returns focus to what opened it — the ▾ switcher.
import { describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

vi.mock("@/lib/auth/AuthProvider", () => ({
  useAuth: () => ({ user: { id: "u1", isAdmin: false } }),
}));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn().mockResolvedValue(undefined),
    listViews: vi.fn().mockResolvedValue([]),
  };
});

import { SavedViews } from "./SavedViews";

const current = { filters: [], sort: null, density: "normal" as const, hiddenColumns: [] };

describe("SavedViews — save dialog focus", () => {
  it("the name input takes focus on open; Esc closes and returns focus to the ▾ switcher", async () => {
    render(<SavedViews entity="crm_deals" current={current} onApply={() => {}} />);
    const trigger = screen.getByRole("button", { name: "Chọn view đã lưu" });
    trigger.focus();
    fireEvent.keyDown(trigger, { key: "Enter" });
    await userEvent.click(await screen.findByRole("menuitem", { name: "Lưu view hiện tại…" }));
    const input = await screen.findByRole("textbox", { name: "Tên view" });
    await waitFor(() => expect(document.activeElement).toBe(input));
    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await act(() => new Promise((r) => setTimeout(r, 0)));
    expect(document.activeElement).toBe(trigger);
  });
});
