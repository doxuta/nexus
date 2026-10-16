import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { DeliveredNotice } from "./DeliveredNotice";

const state = { delivered: true, headVersion: { versionNo: 2, kind: "publish", publishedAt: "2026-10-14T03:00:00Z", publishedBy: "u" }, pendingCount: 0 };
const U = (o = {}) => ({ id: "u1", tenantId: "t", email: "chu@khach.vn", fullName: "", status: "active", isAdmin: false, canManageUsers: true, ...o });

describe("DeliveredNotice", () => {
  it("names the version, offers no re-deliver, and takes focus on mount", () => {
    render(<DeliveredNotice slug="acme" state={state as never} owner={{ users: [U()] } as never} onRetryOwner={vi.fn()} />);
    const block = screen.getByText(/phiên bản 2/).parentElement as HTMLElement;
    expect(block).toHaveFocus();
    expect(block.getAttribute("tabindex")).toBe("-1");
    expect(screen.queryByRole("button", { name: /Giao/ })).toBeNull();
  });
  it("owner read failed: could-not-check + retry, no false warning", () => {
    render(<DeliveredNotice slug="acme" state={state as never} owner="error" onRetryOwner={vi.fn()} />);
    expect(screen.getByText(/Chưa kiểm được chủ khách/)).toBeInTheDocument();
    expect(screen.queryByText(/khách chưa tự thêm nhân viên/)).toBeNull();
  });
  it("no usable owner: warning + link to Người dùng", () => {
    render(<DeliveredNotice slug="acme" state={state as never} owner={{ users: [U({ status: "inactive" })] } as never} onRetryOwner={vi.fn()} />);
    expect(screen.getByRole("link", { name: "Mở Người dùng" })).toBeInTheDocument();
  });
});
