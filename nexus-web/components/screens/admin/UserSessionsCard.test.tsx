// UserSessionsCard — the "Phiên đăng nhập" card shared by /profile (own sessions) and the admin's
// user detail (someone else's; specs/065 lát A, #313). The card is dumb: `load` / `end` /
// optional `revokeAll` come from the caller, so the same list + confirm serves both.
import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { UserSessionsCard } from "@/components/screens/admin/UserSessionsCard";
import type { LoginSession } from "@/lib/api";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";

const S1: LoginSession = { id: "s1", browser: "Chrome", os: "macOS", ip: "113.161.42.x", lastUsedAt: "2026-10-07T01:00:00Z", expiresAt: "2026-10-14T01:00:00Z" };
const S2: LoginSession = { id: "s2", browser: "Safari", os: "iOS", ip: "27.3.1.x", lastUsedAt: "2026-10-06T01:00:00Z", expiresAt: "2026-10-13T01:00:00Z" };

describe("UserSessionsCard", () => {
  it("lists the sessions from load() and ends one behind a confirm", async () => {
    let rows = [S1, S2];
    const load = vi.fn(async () => rows);
    const end = vi.fn(async (id: string) => { rows = rows.filter((r) => r.id !== id); });
    render(<UserSessionsCard load={load} end={end} />);

    const list = await screen.findByRole("list", { name: "Phiên đăng nhập" });
    expect(within(list).getByText("Chrome · macOS")).toBeInTheDocument();
    expect(within(list).getByText("Safari · iOS")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Đăng xuất Safari · iOS" }));
    const dialog = await screen.findByRole("dialog", { name: "Đăng xuất thiết bị này?" });
    expect(end).not.toHaveBeenCalled(); // asks first
    fireEvent.click(within(dialog).getByRole("button", { name: "Đăng xuất thiết bị" }));

    await waitFor(() => expect(end).toHaveBeenCalledWith("s2"));
    await waitFor(() => expect(screen.queryByText("Safari · iOS")).not.toBeInTheDocument());
    expect(screen.getByText("Chrome · macOS")).toBeInTheDocument();
  });

  it("a failed load says so (not an empty card)", async () => {
    const load = vi.fn(async () => { throw new Error("boom"); });
    render(<UserSessionsCard load={load} end={vi.fn()} />);
    expect(await screen.findByText(/Không tải được danh sách thiết bị/)).toBeInTheDocument();
  });

  it("revokeAll button exists only when the caller passes revokeAll", async () => {
    const load = async () => [S1];
    const { unmount } = render(<UserSessionsCard load={load} end={vi.fn()} />);
    await screen.findByText("Chrome · macOS");
    expect(screen.queryByRole("button", { name: "Đăng xuất mọi thiết bị" })).not.toBeInTheDocument();
    unmount();

    render(<UserSessionsCard load={load} end={vi.fn()} revokeAll={vi.fn(async () => {})} />);
    expect(await screen.findByRole("button", { name: "Đăng xuất mọi thiết bị" })).toBeInTheDocument();
  });

  describe("in the Studio work surface (specs/078 T035)", () => {
    const mountEmbedded = (load: () => Promise<LoginSession[]>, end: (id: string) => Promise<void>) =>
      render(<EmbeddedSurface><UserSessionsCard load={load} end={end} /></EmbeddedSurface>);

    it("0 dialogs; Đăng xuất thiết bị asks at the row, then ends that session", async () => {
      let rows = [S1, S2];
      const load = vi.fn(async () => rows);
      const end = vi.fn(async (id: string) => { rows = rows.filter((r) => r.id !== id); });
      mountEmbedded(load, end);
      await screen.findByText("Safari · iOS");
      fireEvent.click(screen.getByRole("button", { name: "Đăng xuất Safari · iOS" }));
      expect(document.querySelectorAll("[role=dialog]")).toHaveLength(0);
      expect(screen.getByText(/Thiết bị này bị từ chối ở lần gọi kế tiếp\./)).toBeInTheDocument();
      expect(end).not.toHaveBeenCalled();
      fireEvent.click(screen.getByRole("button", { name: "Đăng xuất thiết bị" }));
      await waitFor(() => expect(end).toHaveBeenCalledWith("s2"));
      await waitFor(() => expect(screen.queryByText("Safari · iOS")).not.toBeInTheDocument());
    });
    it("empty: says so", async () => {
      mountEmbedded(async () => [], vi.fn());
      expect(await screen.findByText("Chưa có thiết bị nào đăng nhập.")).toBeInTheDocument();
    });
    it("failed load: the sentence and Thử lại", async () => {
      mountEmbedded(async () => { throw new Error("boom"); }, vi.fn());
      expect(await screen.findByText(/Không đọc được danh sách thiết bị/)).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Thử lại" })).toBeInTheDocument();
    });
    it("a failed end keeps the question and says so", async () => {
      mountEmbedded(async () => [S1, S2], vi.fn(async () => { throw new Error("x"); }));
      await screen.findByText("Safari · iOS");
      fireEvent.click(screen.getByRole("button", { name: "Đăng xuất Safari · iOS" }));
      fireEvent.click(screen.getByRole("button", { name: "Đăng xuất thiết bị" }));
      expect(await screen.findByText("Chưa đăng xuất được thiết bị này. Thử lại.")).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Đăng xuất thiết bị" })).toBeInTheDocument();
    });
    it("no 'đăng xuất mọi nơi' for another person", async () => {
      mountEmbedded(async () => [S1], vi.fn());
      await screen.findByText("Chrome · macOS");
      expect(screen.queryByRole("button", { name: /mọi thiết bị/ })).toBeNull();
    });
  });
});
