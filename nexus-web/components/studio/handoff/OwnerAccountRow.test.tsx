import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { AdminUser } from "@/lib/api";
import { OwnerAccountRow } from "./OwnerAccountRow";

const ROLES = [
  { id: "r-owner", name: "tenant_owner", label: "Chủ khách" },
  { id: "r-sales", name: "sales_staff", label: "Nhân viên kinh doanh" },
];
const OWNER: AdminUser = { id: "u-own", tenantId: "t", email: "chu@khach.vn", fullName: "Chủ", status: "active", isAdmin: false, canManageUsers: true, roleId: "r-owner" };

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
let calls: { method: string; url: string; body: unknown }[];
let respond: () => Response;
const realFetch = global.fetch;

beforeEach(() => {
  calls = [];
  respond = () => json({ data: { user: OWNER } });
  global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ method: init?.method ?? "GET", url: String(url), body: init?.body ? JSON.parse(String(init.body)) : undefined });
    return respond();
  }) as unknown as typeof fetch;
});
afterEach(() => {
  global.fetch = realFetch;
});

describe("OwnerAccountRow", () => {
  it("names the account and the role in one sentence", () => {
    render(<OwnerAccountRow owner={OWNER} roles={ROLES} onChanged={() => {}} />);
    expect(screen.getByText("Chủ khách đăng nhập bằng chu@khach.vn, vai Chủ khách.")).toBeInTheDocument();
    expect(screen.queryByLabelText("Vai")).toBeNull();
  });

  it("Đổi vai opens an in-place form with EVERY role, current one chosen, no dialog, no email field", () => {
    render(<OwnerAccountRow owner={OWNER} roles={ROLES} onChanged={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: "Đổi vai" }));
    const sel = screen.getByLabelText("Vai") as HTMLSelectElement;
    expect(sel.value).toBe("r-owner");
    expect(within(sel).getAllByRole("option").map((o) => o.textContent)).toEqual(["Chủ khách", "Nhân viên kinh doanh"]);
    expect(screen.getByText("Đổi vai chỉ đổi quyền dữ liệu, không thu hồi quyền quản lý người dùng của chủ khách.")).toBeInTheDocument();
    expect(document.querySelector("[role=dialog]")).toBeNull();
    expect(screen.queryByLabelText(/email/i)).toBeNull();
  });

  it("owner without a role: form starts on the placeholder and Lưu sends nothing until a role is chosen", async () => {
    const onChanged = vi.fn();
    render(<OwnerAccountRow owner={{ ...OWNER, roleId: undefined }} roles={ROLES} onChanged={onChanged} />);
    fireEvent.click(screen.getByRole("button", { name: "Đổi vai" }));
    const sel = screen.getByLabelText("Vai") as HTMLSelectElement;
    expect(sel.value).toBe("");
    fireEvent.click(screen.getByRole("button", { name: "Lưu" }));
    expect(calls.filter((c) => c.method === "PUT")).toHaveLength(0);
    fireEvent.change(sel, { target: { value: "r-sales" } });
    fireEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(calls.find((c) => c.method === "PUT")!.body).toEqual({ roleId: "r-sales" });
  });

  it("Lưu sends PUT with a body of exactly {roleId}", async () => {
    const onChanged = vi.fn();
    render(<OwnerAccountRow owner={OWNER} roles={ROLES} onChanged={onChanged} />);
    fireEvent.click(screen.getByRole("button", { name: "Đổi vai" }));
    fireEvent.change(screen.getByLabelText("Vai"), { target: { value: "r-sales" } });
    fireEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    const put = calls.find((b) => b.method === "PUT")!;
    expect(put.url).toMatch(/\/admin\/users\/u-own$/);
    expect(Object.keys(put.body as object)).toEqual(["roleId"]);
    expect(put.body).toEqual({ roleId: "r-sales" });
    expect(screen.queryByLabelText("Vai")).toBeNull();
  });

  it("a server error code prints in place and keeps the form open", async () => {
    respond = () => json({ error: "role_id không hợp lệ hoặc không thuộc tổ chức", code: "ROLE_INVALID" }, 422);
    const onChanged = vi.fn();
    render(<OwnerAccountRow owner={OWNER} roles={ROLES} onChanged={onChanged} />);
    fireEvent.click(screen.getByRole("button", { name: "Đổi vai" }));
    fireEvent.change(screen.getByLabelText("Vai"), { target: { value: "r-sales" } });
    fireEvent.click(screen.getByRole("button", { name: "Lưu" }));
    expect(await screen.findByText("Vai này không còn trong dự án. Tải lại trang rồi chọn lại.")).toBeInTheDocument();
    expect(onChanged).not.toHaveBeenCalled();
    expect(screen.getByLabelText("Vai")).toBeInTheDocument();
  });

  it("Huỷ and Esc close the form without a call", () => {
    render(<OwnerAccountRow owner={OWNER} roles={ROLES} onChanged={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: "Đổi vai" }));
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(screen.queryByLabelText("Vai")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Đổi vai" }));
    fireEvent.keyDown(screen.getByLabelText("Vai"), { key: "Escape" });
    expect(screen.queryByLabelText("Vai")).toBeNull();
    expect(calls).toHaveLength(0);
  });

  it("double click on Lưu sends one call", async () => {
    render(<OwnerAccountRow owner={OWNER} roles={ROLES} onChanged={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: "Đổi vai" }));
    fireEvent.change(screen.getByLabelText("Vai"), { target: { value: "r-sales" } });
    const save = screen.getByRole("button", { name: "Lưu" });
    fireEvent.click(save);
    fireEvent.click(save);
    await waitFor(() => expect(calls.filter((b) => b.method === "PUT")).toHaveLength(1));
  });
});
