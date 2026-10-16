// specs/079 f7-lich-su-hoan-tac-thu-lai T075: "Hoàn tác" confirmed in the page, reads the entry first, sends only after confirm.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@/lib/api";

vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), getVersion: vi.fn(), revertDirectEntry: vi.fn() }));
vi.mock("@/lib/studioContext", async (orig) => ({ ...(await orig<typeof import("@/lib/studioContext")>()), useStudioIdentity: () => ({ matches: () => true }) }));
const toastSuccess = vi.fn();
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: (m: string) => toastSuccess(m), error: vi.fn() } }));

import { getVersion, revertDirectEntry } from "@/lib/api";
import { RevertConfirm } from "./RevertConfirm";

const roleDelete = () => ({
  id: "e1", seq: 5, kind: "direct" as const, versionNo: null, note: "", docCount: 1, createdAt: "2026-10-14T08:00:00Z", createdBy: "u", createdByName: "Khách A", createdByLabel: "Khách A", rollbackOf: null, schemaStatus: "",
  docs: [{
    docType: "role", docKey: "r1", label: "Nhân viên", op: "delete" as const, changedPaths: [], moreChanged: 0, before: null, after: null, applyStatus: "applied" as const,
    reassign: [
      { userId: "u1", label: "Lan", status: "active", currentRoleId: "" },
      { userId: "u2", label: "Minh", status: "inactive", currentRoleId: "r9" },
    ],
  }],
});
function open(onCancel = vi.fn(), onDone = vi.fn()) {
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><RevertConfirm project="acme" versionId="e1" onCancel={onCancel} onDone={onDone} /></QueryClientProvider>);
  return { onCancel, onDone };
}

beforeEach(() => {
  vi.mocked(getVersion).mockReset().mockResolvedValue(roleDelete());
  vi.mocked(revertDirectEntry).mockReset();
  toastSuccess.mockReset();
});

describe("RevertConfirm", () => {
  it("names the original author and lists everyone who gets the role back, before anything is sent", async () => {
    open();
    expect(await screen.findByText(/Sẽ trả vai Nhân viên cho/)).toBeTruthy();
    expect(screen.getByText(/Lan/)).toBeTruthy();
    expect(screen.getByText(/Minh/).textContent).toMatch(/đang giữ vai khác/);
    expect(screen.getByText(/Thay đổi gốc: Khách A lúc/)).toBeTruthy();
    expect(revertDirectEntry).not.toHaveBeenCalled();
  });

  it("sends POST revert only after the confirm button, then reports and refreshes", async () => {
    vi.mocked(revertDirectEntry).mockResolvedValue({ revertedId: "e1", entry: null, reassigned: 1 });
    const { onDone } = open();
    await screen.findByText(/Lan/);
    fireEvent.click(screen.getByRole("button", { name: "Hoàn tác" }));
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(revertDirectEntry).toHaveBeenCalledWith("e1");
    expect(toastSuccess).toHaveBeenCalledWith("Đã hoàn tác Nhân viên; trả vai cho 1 người");
  });

  it("Huỷ closes without sending", async () => {
    const { onCancel } = open();
    await screen.findByText(/Lan/);
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(onCancel).toHaveBeenCalled();
    expect(revertDirectEntry).not.toHaveBeenCalled();
  });

  it.each([
    [409, "REVERT_STALE", "Bản đang chạy đã đổi sau thay đổi này; hoàn tác không còn đúng. Tải lại."],
    [422, "REVERT_USE_ROLLBACK", /không hoàn tác riêng được/],
    [422, "REVERT_UNSUPPORTED", "Loại thay đổi này chưa hoàn tác được từ lịch sử."],
  ])("answers %s %s in place and stays open", async (status, code, msg) => {
    vi.mocked(revertDirectEntry).mockRejectedValue(new ApiError(status as number, code as string, "x"));
    const { onDone } = open();
    await screen.findByText(/Lan/);
    fireEvent.click(screen.getByRole("button", { name: "Hoàn tác" }));
    expect(await screen.findByText(msg)).toBeTruthy();
    expect(onDone).not.toHaveBeenCalled();
  });

  it("ROLLBACK_NAME_TAKEN shows the server's own sentence (it names the app), not the rollback wording", async () => {
    vi.mocked(revertDirectEntry).mockRejectedValue(new ApiError(409, "ROLLBACK_NAME_TAKEN", "Tên app Kho đã bị dùng"));
    open();
    await screen.findByText(/Lan/);
    fireEvent.click(screen.getByRole("button", { name: "Hoàn tác" }));
    expect(await screen.findByText("Tên app Kho đã bị dùng")).toBeTruthy();
  });

  it("REVERT_STALE reloads the history behind the message", async () => {
    vi.mocked(revertDirectEntry).mockRejectedValue(new ApiError(409, "REVERT_STALE", "x"));
    open();
    await screen.findByText(/Lan/);
    vi.mocked(getVersion).mockClear();
    fireEvent.click(screen.getByRole("button", { name: "Hoàn tác" }));
    await screen.findByText(/hoàn tác không còn đúng/);
    await waitFor(() => expect(getVersion).toHaveBeenCalled());
  });

  it("a failed read says so once, in the confirm message, and offers no send", async () => {
    vi.mocked(getVersion).mockRejectedValue(new ApiError(500, "X", "x"));
    open();
    expect(await screen.findByText(/Chưa đọc được thay đổi này/)).toBeTruthy();
    expect(screen.queryByText("Đang đọc thay đổi")).toBeNull();
    expect((screen.getByRole("button", { name: "Hoàn tác" }) as HTMLButtonElement).disabled).toBe(true);
  });
});
