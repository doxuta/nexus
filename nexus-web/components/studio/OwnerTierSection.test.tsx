// specs/078 T182: project-wide customer owner tier in project settings.
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ listTenants: vi.fn(), setOwnerTier: vi.fn(), revertOwnerTier: vi.fn() }));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));
const toast = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn() }));
vi.mock("@/components/ui/Toaster", () => ({ toast }));

import { ApiError } from "@/lib/api";
import { OwnerTierSection } from "./OwnerTierSection";

const tn = (o = {}) => ({ id: "t-1", slug: "acme", name: "Acme", status: "active", createdAt: "", usableAdminCount: 2, usableOwnerCount: 0, convertibleAdminCount: 2, ...o });
const mount = (tenantId = "t-1") =>
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><OwnerTierSection tenantId={tenantId} name="Acme" /></QueryClientProvider>);

beforeEach(() => { Object.values(api).forEach((f) => f.mockReset()); toast.success.mockReset(); toast.error.mockReset(); });

describe("OwnerTierSection", () => {
  it("shows the two counts from ONE tenants call and only the buttons that apply", async () => {
    api.listTenants.mockResolvedValue([tn({ usableOwnerCount: 1, convertibleAdminCount: 2 })]);
    mount();
    expect(await screen.findByText("Hiện có 1 chủ khách đăng nhập được và 2 quản trị viên có thể chuyển thành chủ khách.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Chuyển sang chủ khách…" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Trả về quản trị viên…" })).toBeInTheDocument();
    expect(api.listTenants).toHaveBeenCalledTimes(1);
  });

  it("hides the revert button with no owners and the convert button with no convertible admins", async () => {
    api.listTenants.mockResolvedValue([tn({ usableOwnerCount: 0, convertibleAdminCount: 0 })]);
    mount();
    await screen.findByText(/Hiện có 0 chủ khách/);
    expect(screen.queryByRole("button", { name: /Chuyển sang chủ khách/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /Trả về quản trị viên/ })).toBeNull();
  });

  it("convert: confirm states the consequence (FR-042), calls setOwnerTier with the prop id, toasts counts", async () => {
    api.listTenants.mockResolvedValue([tn()]);
    api.setOwnerTier.mockResolvedValue({ converted: 2, skippedWithRole: 1, ownerRoleId: "r", revokeFailed: 1 });
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Chuyển sang chủ khách…" }));
    expect(screen.getByText(/mất khu quản trị/)).toBeInTheDocument();
    expect(screen.getByText(/không phải ranh giới bảo mật/)).toBeInTheDocument();
    expect(screen.getByText(/ghi nhật ký với tên chủ khách/)).toBeInTheDocument();
    expect(screen.getByText(/đổi mật khẩu ở lần đăng nhập đầu/)).toBeInTheDocument();
    expect(api.setOwnerTier).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Chuyển sang chủ khách" }));
    await waitFor(() => expect(api.setOwnerTier).toHaveBeenCalledWith("t-1"));
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith("Đã chuyển 2, bỏ qua 1"));
    expect(toast.error).toHaveBeenCalledWith(expect.stringContaining("1 tài khoản"));
  });

  it("revert: confirm repeats the FR-033 sentence and names how many owners become administrators", async () => {
    api.listTenants.mockResolvedValue([tn({ usableOwnerCount: 3, convertibleAdminCount: 0 })]);
    api.revertOwnerTier.mockResolvedValue({ reverted: 3, revokeFailed: 0 });
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Trả về quản trị viên…" }));
    expect(screen.getByText(/3 chủ khách sẽ thành quản trị viên/)).toBeInTheDocument();
    expect(screen.getByText(/khẩn cấp thì Ngưng dự án/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Trả về quản trị viên" }));
    await waitFor(() => expect(api.revertOwnerTier).toHaveBeenCalledWith("t-1"));
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith("Đã trả 3 chủ khách về quản trị viên"));
  });

  it("ignores any other tenant row: the id comes only from the prop", async () => {
    api.listTenants.mockResolvedValue([tn({ id: "t-2", usableOwnerCount: 9 })]);
    const { container } = mount("t-1");
    await waitFor(() => expect(api.listTenants).toHaveBeenCalled());
    expect(screen.queryByText(/Hiện có/)).toBeNull();
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing on a failed call (not a platform operator)", async () => {
    api.listTenants.mockRejectedValue(new ApiError(403, "FORBIDDEN", "no"));
    const { container } = mount();
    await waitFor(() => expect(api.listTenants).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
  });

  it("404 TENANT_NOT_FOUND on the action says the project was retired", async () => {
    api.listTenants.mockResolvedValue([tn()]);
    api.setOwnerTier.mockRejectedValueOnce(new ApiError(404, "TENANT_NOT_FOUND", "x"));
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Chuyển sang chủ khách…" }));
    fireEvent.click(screen.getByRole("button", { name: "Chuyển sang chủ khách" }));
    expect(await screen.findByText("Dự án đã ngưng, không đổi được")).toBeInTheDocument();
  });

  it("403 on the action says it needs a Nexus builder", async () => {
    api.listTenants.mockResolvedValue([tn()]);
    api.setOwnerTier.mockRejectedValueOnce(new ApiError(403, "FORBIDDEN", "x"));
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Chuyển sang chủ khách…" }));
    fireEvent.click(screen.getByRole("button", { name: "Chuyển sang chủ khách" }));
    expect(await screen.findByText("Việc này cần quyền người dựng Nexus")).toBeInTheDocument();
  });

  it("revert: revokeFailed warns and 404 on the action says the project was retired", async () => {
    api.listTenants.mockResolvedValue([tn({ usableOwnerCount: 2, convertibleAdminCount: 0 })]);
    api.revertOwnerTier.mockResolvedValueOnce({ reverted: 2, revokeFailed: 1 });
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Trả về quản trị viên…" }));
    fireEvent.click(screen.getByRole("button", { name: "Trả về quản trị viên" }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith(expect.stringContaining("1 tài khoản")));
    api.revertOwnerTier.mockRejectedValueOnce(new ApiError(404, "TENANT_NOT_FOUND", "x"));
    fireEvent.click(await screen.findByRole("button", { name: "Trả về quản trị viên…" }));
    fireEvent.click(screen.getByRole("button", { name: "Trả về quản trị viên" }));
    expect(await screen.findByText("Dự án đã ngưng, không đổi được")).toBeInTheDocument();
  });

  it("counts line is a polite live region", async () => {
    api.listTenants.mockResolvedValue([tn()]);
    mount();
    const line = await screen.findByText(/Hiện có/);
    expect(line).toHaveAttribute("aria-live", "polite");
  });

  it("Cancel returns focus to the button that opened the confirm, not the first button", async () => {
    api.listTenants.mockResolvedValue([tn({ usableOwnerCount: 1, convertibleAdminCount: 2 })]);
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Trả về quản trị viên…" }));
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Trả về quản trị viên…" })).toHaveFocus());
  });

  it("after a successful action focus lands inside the button row even when no button is left", async () => {
    api.listTenants.mockResolvedValueOnce([tn({ usableOwnerCount: 0, convertibleAdminCount: 2 })]);
    api.setOwnerTier.mockResolvedValue({ converted: 2, skippedWithRole: 0, ownerRoleId: "r", revokeFailed: 0 });
    api.listTenants.mockResolvedValue([tn({ usableOwnerCount: 0, convertibleAdminCount: 0 })]);
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Chuyển sang chủ khách…" }));
    fireEvent.click(screen.getByRole("button", { name: "Chuyển sang chủ khách" }));
    await waitFor(() => expect(api.setOwnerTier).toHaveBeenCalled());
    await waitFor(() => expect(screen.queryByRole("button", { name: "Chuyển sang chủ khách" })).toBeNull());
    await waitFor(() => expect(document.activeElement).not.toBe(document.body));
    expect(document.activeElement?.closest("section")).not.toBeNull();
  });
});
