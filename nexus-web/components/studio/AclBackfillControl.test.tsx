// AclBackfillControl — hợp đồng 2 bước (đoạn-nối slice, docs/NEXUS-MOT-XUONG-2026-07-29.md §0b):
// bấm → dryRun=true TRƯỚC (nhìn thấy đúng danh sách sẽ cấp quyền, zero ghi) → xác nhận trong
// ConfirmDialog → chạy thật (dryRun=false) → failed[] hiện TỪNG dòng kèm reason (cấm nuốt im
// lặng — grant expansion phải auditable bởi người bấm). lib/api mock ở module seam.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { AclBackfillControl } from "./AclBackfillControl";
import { appAclBackfill, getPublishState } from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, appAclBackfill: vi.fn(), getPublishState: vi.fn() };
});

beforeEach(() => {
  vi.mocked(appAclBackfill).mockReset();
  vi.mocked(getPublishState).mockReset();
  vi.mocked(getPublishState).mockResolvedValue({ delivered: false } as never);
});

describe("AclBackfillControl — luồng 2 bước", () => {
  it("bấm → CHỈ dryRun; dialog liệt kê danh sách; xác nhận → chạy thật; failed hiện từng dòng kèm reason", async () => {
    vi.mocked(appAclBackfill)
      .mockResolvedValueOnce({
        seeded: ["crm_accounts", "crm_orders"], skipped: ["dashboard:d1"], failed: [],
      })
      .mockResolvedValueOnce({
        seeded: ["crm_accounts"], skipped: ["dashboard:d1"],
        failed: [{ entity: "crm_orders", reason: "app không có vai mặc định" }],
      });
    const onApplied = vi.fn();
    render(<AclBackfillControl appId="app-1" onApplied={onApplied} />);

    fireEvent.click(screen.getByText("Cấp quyền các bảng của app này"));

    // BƯỚC 1: chỉ dry-run được gọi — chưa có ghi nào xảy ra.
    await waitFor(() => expect(vi.mocked(appAclBackfill)).toHaveBeenCalledWith("app-1", true));
    expect(vi.mocked(appAclBackfill)).toHaveBeenCalledTimes(1);

    // Dialog hiện ĐÚNG danh sách sẽ cấp quyền + dòng gộp mục bỏ qua.
    expect(await screen.findByText("Cấp quyền xem mặc định: crm_accounts")).toBeInTheDocument();
    expect(screen.getByText("Cấp quyền xem mặc định: crm_orders")).toBeInTheDocument();
    expect(screen.getByText("Bỏ qua (dashboard / mục không còn tồn tại)")).toBeInTheDocument();
    // specs/074 T205 (FR-065): the question is asked in the column, never in a dialog.
    expect(screen.queryByRole("dialog")).toBeNull();

    // BƯỚC 2: xác nhận → chạy thật.
    fireEvent.click(screen.getByText("Cấp quyền"));
    await waitFor(() => expect(vi.mocked(appAclBackfill)).toHaveBeenCalledWith("app-1", false));
    expect(onApplied).toHaveBeenCalled(); // readiness được invalidate → bảng kiểm tự cập nhật

    // failed[] PHẢI hiện ra từng dòng kèm reason — không nuốt im lặng.
    expect(await screen.findByText(/crm_orders: app không có vai mặc định/)).toBeInTheDocument();
    expect(screen.getByText(/Đã cấp quyền 1 bảng/)).toBeInTheDocument();
  });

  it("đóng dialog không xác nhận → KHÔNG có lần gọi thật nào", async () => {
    vi.mocked(appAclBackfill).mockResolvedValueOnce({
      seeded: ["crm_accounts"], skipped: [], failed: [],
    });
    render(<AclBackfillControl appId="app-1" />);

    fireEvent.click(screen.getByText("Cấp quyền các bảng của app này"));
    await screen.findByText("Cấp quyền xem mặc định: crm_accounts");

    fireEvent.click(screen.getByText("Huỷ"));
    await waitFor(() =>
      expect(screen.queryByText("Cấp quyền xem mặc định: crm_accounts")).toBeNull(),
    );
    // Duy nhất 1 call (dryRun) — không bao giờ tự chạy thật khi chưa xác nhận.
    expect(vi.mocked(appAclBackfill)).toHaveBeenCalledTimes(1);
    expect(vi.mocked(appAclBackfill)).toHaveBeenCalledWith("app-1", true);
  });
});

describe("AclBackfillControl — 'Áp ngay' (lát f2-direct-apply-access, T379c)", () => {
  it("delivered: dry-run first, then the label and the question in the page, the real run only after confirm", async () => {
    vi.mocked(getPublishState).mockResolvedValue({ delivered: true } as never);
    vi.mocked(appAclBackfill).mockResolvedValue({ seeded: ["crm_accounts"], skipped: [], failed: [] });
    render(<AclBackfillControl appId="app-1" />);
    fireEvent.click(screen.getByText("Cấp quyền các bảng của app này"));
    expect(await screen.findByText("Áp ngay cho người dùng thật")).toBeInTheDocument();
    expect(vi.mocked(appAclBackfill).mock.calls).toEqual([["app-1", true]]);
    fireEvent.click(screen.getByText("Cấp quyền"));
    await waitFor(() => expect(vi.mocked(appAclBackfill)).toHaveBeenCalledWith("app-1", false));
  });

  it("not delivered: the question is there (it lists what is granted) but not the 'Áp ngay' label", async () => {
    vi.mocked(appAclBackfill).mockResolvedValue({ seeded: ["crm_accounts"], skipped: [], failed: [] });
    render(<AclBackfillControl appId="app-1" />);
    fireEvent.click(screen.getByText("Cấp quyền các bảng của app này"));
    expect(await screen.findByText("Cấp quyền xem mặc định: crm_accounts")).toBeInTheDocument();
    expect(screen.queryByText("Áp ngay cho người dùng thật")).toBeNull();
  });
});
