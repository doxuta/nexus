// specs/078 lát u7-khoa-ket-noi (T153): the key vault inside the Studio frame. No dialog for create / edit / rotate / enter /
// delete / "in use"; the secret shows once in SecretOnceBlock (tick gates "Xong"); every write re-checks the customer scope.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { NamedCredentials } from "@/components/screens/admin/NamedCredentials";
import type { NamedCredential } from "@/lib/admin";
import { logout } from "@/lib/api";

let scopeOk = true;
const guard = { assertScope: () => scopeOk };

const GENERATED = "nxw_Zk3Hq9LmN2pR7sTuV0wXyA4bC6dE8fG1hJ5iKo";
const CRED: NamedCredential = {
  id: "c-1", name: "cong_thanh_toan", label: "Cổng thanh toán", baseUrl: "https://pay.example.com",
  authType: "hmac", headerName: "", headerPrefix: "", username: "", status: "ready", secretHint: "f5iK",
  extraHeaderNames: [], rotatedAt: null, previousExpiresAt: null, usageCount: 2,
  createdAt: "2026-10-01T09:00:00Z", updatedAt: "2026-10-01T09:00:00Z",
};
const WAITING: NamedCredential = { ...CRED, id: "c-2", name: "kho_hang", label: "Kho hàng", authType: "header", status: "needs_secret", secretHint: "", usageCount: 0 };

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
let calls: { method: string; url: string; body?: Record<string, unknown> }[] = [];
let onDelete: () => Response = () => new Response(null, { status: 204 });

function installFetch() {
  calls = [];
  global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    calls.push({ method, url: u, body: init?.body ? JSON.parse(String(init.body)) : undefined });
    if (u.endsWith("/rotate")) return json({ data: { credential: CRED, secret: GENERATED } });
    if (u.endsWith("/admin/credentials") && method === "POST") return json({ data: { credential: CRED, secret: GENERATED } }, 201);
    if (u.endsWith("/admin/credentials")) return json({ data: { credentials: [CRED, WAITING] } });
    if (method === "PUT") return json({ data: { credential: CRED } });
    if (method === "DELETE") return onDelete();
    if (method === "GET") return json({ data: { credential: CRED, usages: [] } });
    return json({ error: "unmocked", code: "X" }, 500);
  }) as unknown as typeof fetch;
}

const dialogs = () => document.body.querySelectorAll("[role=dialog],[role=alertdialog]").length;
async function mount() {
  render(<EmbeddedSurface><NamedCredentials guard={guard} /></EmbeddedSurface>);
  await screen.findByText("Kho hàng");
}
async function rowAction(row: string, item: string) {
  screen.getByRole("button", { name: `Thao tác cho khoá ${row}` }).focus();
  await userEvent.keyboard("{Enter}");
  fireEvent.click(await screen.findByRole("menuitem", { name: item }));
}
const writes = () => calls.filter((c) => c.method !== "GET");

describe("NamedCredentials inside the Studio frame", () => {
  const realFetch = global.fetch;
  beforeEach(async () => {
    scopeOk = true;
    onDelete = () => new Response(null, { status: 204 });
    await logout();
    window.localStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
    installFetch();
  });
  afterEach(() => { cleanup(); global.fetch = realFetch; window.localStorage.clear(); window.sessionStorage.clear(); });

  it("create opens a form in the page, not a dialog; the generated key shows once in SecretOnceBlock", async () => {
    await mount();
    fireEvent.click(screen.getByRole("button", { name: /Tạo khoá/ }));
    await screen.findByLabelText(/Tên máy/);
    expect(dialogs()).toBe(0);
    fireEvent.change(screen.getByLabelText(/Tên máy/), { target: { value: "cong_thanh_toan" } });
    fireEvent.change(screen.getByLabelText(/Địa chỉ gốc/), { target: { value: "https://pay.example.com" } });
    fireEvent.click(screen.getByLabelText("HMAC"));
    fireEvent.click(screen.getByRole("switch", { name: /Nexus tự sinh khoá/ }));
    fireEvent.click(within(screen.getByRole("region", { name: "Tạo khoá kết nối" })).getByRole("button", { name: /^Tạo khoá$/ }));
    await waitFor(() => expect(document.querySelector("[data-secret-once] code")?.textContent).toBe(GENERATED));
    expect(dialogs()).toBe(0);
    expect(document.querySelectorAll("input[readonly]")).toHaveLength(0);
    const done = screen.getByRole("button", { name: "Xong" });
    expect(done).toBeDisabled();
    fireEvent.click(screen.getByRole("checkbox"));
    fireEvent.click(done);
    await waitFor(() => expect(document.body.textContent).not.toContain(GENERATED));
    expect(JSON.stringify({ ...window.localStorage, ...window.sessionStorage })).not.toContain(GENERATED);
  });

  it("scope changed before the write: 0 POST", async () => {
    await mount();
    fireEvent.click(screen.getByRole("button", { name: /Tạo khoá/ }));
    await screen.findByLabelText(/Tên máy/);
    fireEvent.change(screen.getByLabelText(/Tên máy/), { target: { value: "abc" } });
    fireEvent.change(screen.getByLabelText(/Địa chỉ gốc/), { target: { value: "https://a.example.com" } });
    scopeOk = false;
    fireEvent.click(within(screen.getByRole("region", { name: "Tạo khoá kết nối" })).getByRole("button", { name: /^Tạo khoá$/ }));
    await new Promise((r) => setTimeout(r, 50));
    expect(writes()).toEqual([]);
  });

  it("edit is a form in the page and PUTs", async () => {
    await mount();
    await rowAction("Cổng thanh toán", "Sửa thông tin");
    await screen.findByLabelText(/Tên hiển thị/);
    expect(dialogs()).toBe(0);
    fireEvent.change(screen.getByLabelText(/Tên hiển thị/), { target: { value: "Cổng mới" } });
    fireEvent.click(screen.getByRole("button", { name: /Lưu thay đổi|Lưu/ }));
    await waitFor(() => expect(writes().map((c) => c.method)).toEqual(["PUT"]));
  });

  it("rotate and enter-key are forms in the page; the new key shows in SecretOnceBlock", async () => {
    await mount();
    await rowAction("Cổng thanh toán", "Xoay khoá");
    await screen.findByRole("heading", { name: /Xoay khoá/ });
    expect(dialogs()).toBe(0);
    fireEvent.click(screen.getByRole("switch", { name: /Nexus tự sinh khoá/ }));
    fireEvent.click(screen.getByRole("button", { name: /^Xoay khoá$/ }));
    await waitFor(() => expect(document.querySelector("[data-secret-once] code")?.textContent).toBe(GENERATED));
    expect(dialogs()).toBe(0);
    cleanup();
    installFetch();
    await mount();
    await rowAction("Kho hàng", "Nhập khoá");
    await screen.findByRole("heading", { name: /Nhập khoá/ });
    expect(dialogs()).toBe(0);
  });

  it("delete confirms in the page and DELETEs; 409 lists the rules in the page", async () => {
    await mount();
    await rowAction("Kho hàng", "Xoá khoá");
    const confirm = await screen.findByRole("button", { name: "Xoá khoá" });
    expect(dialogs()).toBe(0);
    onDelete = () => json({ error: "in use", code: "CREDENTIAL_IN_USE", details: { usages: [{ workflowId: "w1", workflowName: "Báo đơn cho đối tác", actionIndex: 1 }] } }, 409);
    fireEvent.click(confirm);
    await screen.findByText(/Báo đơn cho đối tác/);
    expect(dialogs()).toBe(0);
    expect(writes().map((c) => c.method)).toEqual(["DELETE"]);
  });

  it("scope changed before the usage popover opens: 0 GET /:id", async () => {
    await mount();
    scopeOk = false;
    const before = calls.length;
    fireEvent.click(screen.getByRole("button", { name: /2/ }));
    await waitFor(() => expect(screen.getByText(/không|lỗi/i)).toBeInTheDocument());
    expect(calls.length).toBe(before);
  });

  it("scope changed before delete: 0 DELETE", async () => {
    await mount();
    await rowAction("Kho hàng", "Xoá khoá");
    const confirm = await screen.findByRole("button", { name: "Xoá khoá" });
    scopeOk = false;
    fireEvent.click(confirm);
    await new Promise((r) => setTimeout(r, 50));
    expect(writes()).toEqual([]);
  });

  it("scope changed before edit: 0 PUT", async () => {
    await mount();
    await rowAction("Cổng thanh toán", "Sửa thông tin");
    await screen.findByLabelText(/Tên hiển thị/);
    fireEvent.change(screen.getByLabelText(/Tên hiển thị/), { target: { value: "Cổng mới" } });
    scopeOk = false;
    fireEvent.click(screen.getByRole("button", { name: /Lưu thay đổi|Lưu/ }));
    await new Promise((r) => setTimeout(r, 50));
    expect(writes()).toEqual([]);
  });

  it("scope changed before rotate / enter-key: 0 POST /rotate", async () => {
    await mount();
    await rowAction("Cổng thanh toán", "Xoay khoá");
    await screen.findByRole("heading", { name: /Xoay khoá/ });
    fireEvent.click(screen.getByRole("switch", { name: /Nexus tự sinh khoá/ }));
    scopeOk = false;
    fireEvent.click(screen.getByRole("button", { name: /^Xoay khoá$/ }));
    await new Promise((r) => setTimeout(r, 50));
    expect(writes()).toEqual([]);
    cleanup();
    installFetch();
    scopeOk = true;
    await mount();
    await rowAction("Kho hàng", "Nhập khoá");
    await screen.findByRole("heading", { name: /Nhập khoá/ });
    fireEvent.change(screen.getByLabelText(/^Khoá mới/), { target: { value: "s3cret-value-123" } });
    scopeOk = false;
    fireEvent.click(screen.getByRole("button", { name: /^Lưu khoá$/ }));
    await new Promise((r) => setTimeout(r, 50));
    expect(writes()).toEqual([]);
  });

  it("while a new key is on screen no other row or create can open (the key would be lost unseen)", async () => {
    await mount();
    await rowAction("Cổng thanh toán", "Xoay khoá");
    await screen.findByRole("heading", { name: /Xoay khoá/ });
    fireEvent.click(screen.getByRole("switch", { name: /Nexus tự sinh khoá/ }));
    fireEvent.click(screen.getByRole("button", { name: /^Xoay khoá$/ }));
    await waitFor(() => expect(document.querySelector("[data-secret-once] code")?.textContent).toBe(GENERATED));
    expect(writes().map((c) => `${c.method} ${c.url.split("/").slice(-1)[0]}`)).toEqual(["POST rotate"]);
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Thao tác cho khoá Kho hàng" })).toBeDisabled();
      expect(screen.getByRole("button", { name: "Thao tác cho khoá Cổng thanh toán" })).toBeDisabled();
      expect(screen.getByRole("button", { name: /Tạo khoá/ })).toBeDisabled();
    }, { timeout: 3000 });
    expect(document.body.textContent).toContain(GENERATED);
    fireEvent.click(screen.getByRole("checkbox"));
    fireEvent.click(screen.getByRole("button", { name: "Xong" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Thao tác cho khoá Kho hàng" })).not.toBeDisabled(), { timeout: 3000 });
  });
});
