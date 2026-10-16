// WorkspaceBanner — specs/062-customer-workspace L2 (T033): "Đang dựng cho: <khách>" + "Về Nexus" + the
// proactive renewal that keeps a long build from being thrown out when the 15-minute token lapses.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen, fireEvent } from "@testing-library/react";
import { WorkspaceBanner } from "./WorkspaceBanner";
import {
  WORKSPACE_ENDED_EVENT, clearWorkspace, setWorkspace, takeWorkspaceEndedNotice, setWorkspaceEndedNotice, type WorkspaceState,
} from "@/lib/workspace";
import { toast } from "@/components/ui/Toaster";
import { exitWorkspace, resumeWorkspace } from "@/lib/api";

vi.mock("@/components/ui/Toaster", () => ({ toast: { info: vi.fn(), success: vi.fn(), error: vi.fn(), warning: vi.fn() } }));
vi.mock("@/lib/api", () => ({
  resumeWorkspace: vi.fn(async () => {}),
  exitWorkspace: vi.fn(async () => {}),
}));

const state = (expiresInMs: number): WorkspaceState => ({
  accessToken: "WS-TOK",
  expiresAt: new Date(Date.now() + expiresInMs).toISOString(),
  homeUserId: "u-admin",
  session: {
    id: "ws-1", tenantId: "t-x", tenantSlug: "x", tenantName: "Khách X",
    openedAt: new Date().toISOString(), expiresAt: new Date(Date.now() + 8 * 3600_000).toISOString(),
  },
});

describe("WorkspaceBanner", () => {
  const originalLocation = window.location;
  let assign: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    window.localStorage.clear();
    window.sessionStorage.clear();
    clearWorkspace();
    vi.mocked(resumeWorkspace).mockClear();
    vi.mocked(exitWorkspace).mockClear();
    vi.mocked(toast.warning).mockClear();
    assign = vi.fn();
    Object.defineProperty(window, "location", { configurable: true, value: { ...originalLocation, assign } });
  });
  afterEach(() => {
    vi.useRealTimers();
    Object.defineProperty(window, "location", { configurable: true, value: originalLocation });
  });

  it("renders nothing when the tab is not in a workspace", () => {
    const { container } = render(<WorkspaceBanner />);
    expect(container).toBeEmptyDOMElement();
  });

  it("names the customer and its code in an alert, with a 'Về Nexus' button that leaves", () => {
    setWorkspace(state(10 * 60_000));
    render(<WorkspaceBanner />);
    expect(screen.getByRole("alert")).toHaveTextContent("Đang dựng cho: Khách X (x)");
    fireEvent.click(screen.getByRole("button", { name: "Về Nexus" }));
    expect(exitWorkspace).toHaveBeenCalledTimes(1);
  });

  it("renews 60 s before the token expires — not earlier — and re-arms after the state changes", () => {
    vi.useFakeTimers();
    setWorkspace(state(10 * 60_000));
    render(<WorkspaceBanner />);
    act(() => { vi.advanceTimersByTime(9 * 60_000 - 1_000); });
    expect(resumeWorkspace).not.toHaveBeenCalled();
    act(() => { vi.advanceTimersByTime(2_000); });
    expect(resumeWorkspace).toHaveBeenCalledTimes(1);
    // The renewal landed a fresh token: the next timer counts from the NEW expiry (no tight loop).
    act(() => { setWorkspace(state(14 * 60_000)); });
    act(() => { vi.advanceTimersByTime(60_000); });
    expect(resumeWorkspace).toHaveBeenCalledTimes(1);
    act(() => { vi.advanceTimersByTime(13 * 60_000); });
    expect(resumeWorkspace).toHaveBeenCalledTimes(2);
  });

  it("a tab that wakes up after the token lapsed renews immediately on visibilitychange (a sleeping machine skips timers)", () => {
    vi.useFakeTimers();
    setWorkspace(state(-5 * 60_000)); // already expired when the tab comes back
    render(<WorkspaceBanner />);
    vi.mocked(resumeWorkspace).mockClear();
    Object.defineProperty(document, "visibilityState", { configurable: true, value: "visible" });
    act(() => { document.dispatchEvent(new Event("visibilitychange")); });
    expect(resumeWorkspace).toHaveBeenCalledTimes(1);
  });

  it("visibilitychange with plenty of time left does not renew", () => {
    setWorkspace(state(10 * 60_000));
    render(<WorkspaceBanner />);
    Object.defineProperty(document, "visibilityState", { configurable: true, value: "visible" });
    act(() => { document.dispatchEvent(new Event("visibilitychange")); });
    expect(resumeWorkspace).not.toHaveBeenCalled();
  });

  it("the session ending under the tab → back to the library, and the next page says why in a Toast", () => {
    setWorkspace(state(10 * 60_000));
    const { unmount } = render(<WorkspaceBanner />);
    // lib/api clears the state BEFORE it fires the event: the banner must still know whose session ended.
    act(() => {
      clearWorkspace();
      window.dispatchEvent(new CustomEvent(WORKSPACE_ENDED_EVENT));
    });
    expect(assign).toHaveBeenCalledWith("/studio"); // the front page of the Studio (specs/073), not the old library route
    unmount();
    render(<WorkspaceBanner />); // the page the tab lands on
    expect(toast.warning).toHaveBeenCalledWith("Phiên dựng cho Khách X đã kết thúc — thao tác vừa rồi chưa được lưu");
    expect(takeWorkspaceEndedNotice()).toBeNull(); // one-shot
  });

  it("a caller can replace the label, the leave action and the ended action (the Studio does), the notice is still left", () => {
    setWorkspace(state(10 * 60_000));
    const onExit = vi.fn(async () => {});
    const onEnded = vi.fn();
    render(<WorkspaceBanner exitLabel="Về danh sách dự án" onExit={onExit} onEnded={onEnded} />);
    fireEvent.click(screen.getByRole("button", { name: "Về danh sách dự án" }));
    expect(onExit).toHaveBeenCalledTimes(1);
    expect(exitWorkspace).not.toHaveBeenCalled();
    act(() => {
      clearWorkspace();
      window.dispatchEvent(new CustomEvent(WORKSPACE_ENDED_EVENT));
    });
    expect(onEnded).toHaveBeenCalledTimes(1);
    expect(assign).not.toHaveBeenCalled();
    expect(takeWorkspaceEndedNotice()).toBe("Khách X");
  });

  it("a notice left for the landing page is consumed once", () => {
    setWorkspaceEndedNotice("Khách Y");
    render(<WorkspaceBanner />);
    expect(toast.warning).toHaveBeenCalledTimes(1);
  });
});
