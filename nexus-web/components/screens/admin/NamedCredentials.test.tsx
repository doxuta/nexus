// Component tests — Kho khoá kết nối (specs/050 S8a).
// Pins the SECURITY-CRITICAL contract:
//   * a Nexus-generated hmac key is shown ONCE with the pinned warning copy, and is gone from
//     the DOM and from every browser store once the dialog is left;
//   * the table never renders a secret, even when the API sends an unexpected field;
//   * the create form sends exactly the fields each auth type needs;
//   * delete 409 lists the rules still using the key; 503 / 422 map to honest UI.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { NamedCredentials } from "@/components/screens/admin/NamedCredentials";
import { t } from "@/lib/i18n";
import { logout } from "@/lib/api";
import type { NamedCredential } from "@/lib/admin";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const GENERATED = "nxw_Zk3Hq9LmN2pR7sTuV0wXyA4bC6dE8fG1hJ5iKo";
const LEAK = "LEAKED-SECRET-VALUE";

const CRED: NamedCredential = {
  id: "c-1", name: "cong_thanh_toan", label: "Cổng thanh toán", baseUrl: "https://pay.example.com",
  authType: "hmac", headerName: "", headerPrefix: "", username: "", status: "ready", secretHint: "f5iK",
  extraHeaderNames: ["X-Tenant"], rotatedAt: null, previousExpiresAt: null, usageCount: 2,
  createdAt: "2026-10-01T09:00:00Z", updatedAt: "2026-10-01T09:00:00Z",
};
const WAITING: NamedCredential = {
  ...CRED, id: "c-2", name: "kho_hang", label: "Kho hàng", authType: "header", status: "needs_secret",
  secretHint: "", extraHeaderNames: [], usageCount: 0,
};

function routedFetch(opts: {
  list?: () => Response;
  onCreate?: (body: Record<string, unknown>) => Response;
  onDelete?: (url: string) => Response;
  onUpdate?: (url: string, body: Record<string, unknown>) => Response;
  onRotate?: (url: string, body: Record<string, unknown>) => Response;
}) {
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    if (u.endsWith("/rotate") && method === "POST") return opts.onRotate!(u, JSON.parse(String(init?.body)));
    if (u.includes("/admin/credentials/") && method === "PUT") return opts.onUpdate!(u, JSON.parse(String(init?.body)));
    if (u.includes("/admin/credentials/") && method === "GET") {
      return jsonRes({ data: { credential: CRED, usages: [{ workflowId: "w1", workflowName: "Báo đơn cho đối tác", actionIndex: 1 }] } }, 200);
    }
    if (u.includes("/admin/credentials/") && method === "DELETE") return opts.onDelete?.(u) ?? new Response(null, { status: 204 });
    if (u.endsWith("/admin/credentials") && method === "GET") {
      return opts.list?.() ?? jsonRes({ data: { credentials: [{ ...CRED, secret: LEAK }, WAITING] } }, 200);
    }
    if (u.endsWith("/admin/credentials") && method === "POST") {
      return opts.onCreate!(JSON.parse(String(init?.body)));
    }
    return jsonRes({ error: `unmocked ${method} ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
}

async function openCreate() {
  await waitFor(() => expect(screen.getByRole("button", { name: /Tạo khoá/ })).toBeInTheDocument());
  fireEvent.click(screen.getByRole("button", { name: /Tạo khoá/ }));
  await screen.findByLabelText(/Tên máy/);
}

// Row menu (Radix): open with the keyboard, then pick the item by its visible name.
async function pickRowAction(rowLabel: string, item: string) {
  const trigger = screen.getByRole("button", { name: `Thao tác cho khoá ${rowLabel}` });
  trigger.focus();
  await userEvent.keyboard("{Enter}");
  fireEvent.click(await screen.findByRole("menuitem", { name: item }));
}

const submit = () => fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: /^Tạo khoá$/ }));

describe("NamedCredentials", () => {
  let originalFetch: FetchFn;

  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout();
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("lists keys with kind and honest status — never a secret, even from an unexpected field", async () => {
    global.fetch = routedFetch({}) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await screen.findByText("Cổng thanh toán");
    expect(screen.getByText("cong_thanh_toan")).toBeInTheDocument();
    expect(screen.getByText("HMAC")).toBeInTheDocument();
    expect(screen.getByText("Chờ nhập khoá")).toBeInTheDocument();
    expect(screen.getByText("…f5iK")).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(LEAK);
  });

  it("503 shows the not-configured notice instead of a table", async () => {
    global.fetch = routedFetch({
      list: () => jsonRes({ error: "x", code: "CREDENTIALS_NOT_CONFIGURED" }, 503),
    }) as unknown as typeof fetch;
    render(<NamedCredentials />);
    expect(await screen.findByText(t("page.credentials.notConfigured"))).toBeInTheDocument();
  });

  it("hmac auto-generate with two extra headers: sends them, shows the key ONCE, forgets it after Xong", async () => {
    const bodies: Record<string, unknown>[] = [];
    global.fetch = routedFetch({
      onCreate: (b) => { bodies.push(b); return jsonRes({ data: { credential: CRED, secret: GENERATED } }, 201); },
    }) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await openCreate();

    fireEvent.change(screen.getByLabelText(/Tên máy/), { target: { value: "cong_thanh_toan" } });
    fireEvent.change(screen.getByLabelText(/Tên hiển thị/), { target: { value: "Cổng thanh toán" } });
    fireEvent.change(screen.getByLabelText(/Địa chỉ gốc/), { target: { value: "https://pay.example.com" } });
    fireEvent.click(screen.getByLabelText("HMAC"));
    fireEvent.click(screen.getByRole("switch", { name: /Nexus tự sinh khoá/ }));
    fireEvent.click(screen.getByRole("button", { name: "Header bổ sung" }));
    fireEvent.click(screen.getByRole("button", { name: /Thêm header/ }));
    fireEvent.click(screen.getByRole("button", { name: /Thêm header/ }));
    const names = screen.getAllByLabelText(/^Tên header$/);
    const values = screen.getAllByLabelText(/^Giá trị$/);
    fireEvent.change(names[0], { target: { value: "X-Tenant" } });
    fireEvent.change(values[0], { target: { value: "northwind" } });
    fireEvent.change(names[1], { target: { value: "X-Env" } });
    fireEvent.change(values[1], { target: { value: "prod" } });
    submit();

    await waitFor(() => expect(screen.getByTestId("credential-secret")).toHaveValue(GENERATED));
    expect(bodies[0]).toEqual({
      name: "cong_thanh_toan", label: "Cổng thanh toán", baseUrl: "https://pay.example.com",
      authType: "hmac", generateSecret: true, extraHeaders: { "X-Tenant": "northwind", "X-Env": "prod" },
    });
    expect(screen.getByText(t("page.credentials.secret.once"))).toBeInTheDocument();
    // Never in a browser store.
    expect(JSON.stringify({ ...window.localStorage })).not.toContain(GENERATED);
    expect(JSON.stringify({ ...window.sessionStorage })).not.toContain(GENERATED);

    fireEvent.click(screen.getByRole("button", { name: /Xong/ }));
    await waitFor(() => expect(screen.queryByTestId("credential-secret")).not.toBeInTheDocument());
    expect(document.body.textContent).not.toContain(GENERATED);
    // Reopening the dialog starts a blank form.
    fireEvent.click(screen.getByRole("button", { name: /Tạo khoá/ }));
    await screen.findByLabelText(/Tên máy/);
    expect(document.body.textContent).not.toContain(GENERATED);
  });

  it.each([
    ["Header", { authType: "header", headerName: "X-Api-Key", headerPrefix: "", secret: "s3cret-typed" }],
    ["Basic", { authType: "basic", username: "bot", secret: "s3cret-typed" }],
  ])("%s kind sends only its own fields", async (kind, extra) => {
    const bodies: Record<string, unknown>[] = [];
    global.fetch = routedFetch({
      onCreate: (b) => { bodies.push(b); return jsonRes({ data: { credential: WAITING } }, 201); },
    }) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await openCreate();
    fireEvent.change(screen.getByLabelText(/Tên máy/), { target: { value: "kho_hang" } });
    fireEvent.change(screen.getByLabelText(/Địa chỉ gốc/), { target: { value: "https://kho.example.com" } });
    fireEvent.click(screen.getByLabelText(kind));
    if (kind === "Header") {
      fireEvent.change(screen.getByLabelText(/^Tên header$/), { target: { value: "X-Api-Key" } });
    } else {
      fireEvent.change(screen.getByLabelText(/Tên đăng nhập/), { target: { value: "bot" } });
    }
    fireEvent.change(screen.getByLabelText(/Khoá \(bí mật\)/), { target: { value: "s3cret-typed" } });
    submit();
    await waitFor(() => expect(bodies.length).toBe(1));
    expect(bodies[0]).toEqual({ name: "kho_hang", baseUrl: "https://kho.example.com", ...extra });
    // A typed key is the caller's own: no reveal step.
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(screen.queryByTestId("credential-secret")).not.toBeInTheDocument();
  });

  it("url_token kind: offered as 'Khoá trong đường dẫn', explains {secret}, sends base with the placeholder + the token", async () => {
    const bodies: Record<string, unknown>[] = [];
    global.fetch = routedFetch({
      onCreate: (b) => { bodies.push(b); return jsonRes({ data: { credential: WAITING } }, 201); },
    }) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await openCreate();
    fireEvent.change(screen.getByLabelText(/Tên máy/), { target: { value: "telegram" } });
    fireEvent.click(screen.getByLabelText("Khoá trong đường dẫn"));
    expect(screen.getByText(/\{secret\}/)).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText(/Địa chỉ gốc/), { target: { value: "https://api.telegram.org/bot{secret}" } });
    fireEvent.change(screen.getByLabelText(/Khoá \(bí mật\)/), { target: { value: "123456:ABC-def" } });
    submit();
    await waitFor(() => expect(bodies.length).toBe(1));
    expect(bodies[0]).toEqual({ name: "telegram", baseUrl: "https://api.telegram.org/bot{secret}", authType: "url_token", secret: "123456:ABC-def" });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("header kind: Authorization pre-fills 'Bearer ' and sends it", async () => {
    const bodies: Record<string, unknown>[] = [];
    global.fetch = routedFetch({
      onCreate: (b) => { bodies.push(b); return jsonRes({ data: { credential: WAITING } }, 201); },
    }) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await openCreate();
    fireEvent.change(screen.getByLabelText(/Địa chỉ gốc/), { target: { value: "https://a.example.com" } });
    fireEvent.change(screen.getByLabelText(/Tên máy/), { target: { value: "kho" } });
    fireEvent.click(screen.getByLabelText("Header"));
    expect(screen.getByLabelText(/Tiền tố/)).toHaveValue("Bearer ");
    submit();
    await waitFor(() => expect(bodies.length).toBe(1));
    expect(bodies[0].headerPrefix).toBe("Bearer ");
  });

  it("closing the one-time key dialog with Esc asks first; confirming discards the key", async () => {
    global.fetch = routedFetch({
      onCreate: () => jsonRes({ data: { credential: CRED, secret: GENERATED } }, 201),
    }) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await openCreate();
    fireEvent.change(screen.getByLabelText(/Tên máy/), { target: { value: "cong" } });
    fireEvent.change(screen.getByLabelText(/Địa chỉ gốc/), { target: { value: "https://a.example.com" } });
    fireEvent.click(screen.getByLabelText("HMAC"));
    fireEvent.click(screen.getByRole("switch", { name: /Nexus tự sinh khoá/ }));
    submit();
    await screen.findByTestId("credential-secret");
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    fireEvent.click(await screen.findByRole("button", { name: t("page.credentials.leave.confirm") }));
    await waitFor(() => expect(screen.queryByTestId("credential-secret")).not.toBeInTheDocument());
    expect(document.body.textContent).not.toContain(GENERATED);
  });

  it("422 on a field with no visible input surfaces at the top of the form", async () => {
    global.fetch = routedFetch({
      onCreate: () => jsonRes({ error: "Không hỗ trợ tự sinh khoá", code: "CREDENTIAL_GENERATE_UNSUPPORTED", details: { field: "generateSecret" } }, 422),
    }) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await openCreate();
    fireEvent.change(screen.getByLabelText(/Tên máy/), { target: { value: "cong" } });
    fireEvent.change(screen.getByLabelText(/Địa chỉ gốc/), { target: { value: "https://a.example.com" } });
    submit();
    expect(await screen.findByText("Không hỗ trợ tự sinh khoá")).toBeInTheDocument();
  });

  it("'Hiện ký tự đang gõ' only flips the input type", async () => {
    global.fetch = routedFetch({}) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await openCreate();
    fireEvent.click(screen.getByLabelText("Header"));
    const secret = screen.getByLabelText(/Khoá \(bí mật\)/);
    expect(secret).toHaveAttribute("type", "password");
    fireEvent.click(screen.getByLabelText(/Hiện ký tự đang gõ/));
    expect(secret).toHaveAttribute("type", "text");
  });

  it("422 lands on the field named in details.field", async () => {
    global.fetch = routedFetch({
      onCreate: () => jsonRes({ error: "Tên máy phải là chữ thường", code: "CREDENTIAL_NAME_INVALID", details: { field: "name" } }, 422),
    }) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await openCreate();
    fireEvent.change(screen.getByLabelText(/Tên máy/), { target: { value: "Sai Ten" } });
    fireEvent.change(screen.getByLabelText(/Địa chỉ gốc/), { target: { value: "https://a.example.com" } });
    submit();
    expect(await screen.findByText("Tên máy phải là chữ thường")).toBeInTheDocument();
    expect(screen.getByLabelText(/Tên máy/)).toHaveAttribute("aria-invalid", "true");
  });

  it("delete DELETEs the right id after confirmation", async () => {
    const urls: string[] = [];
    global.fetch = routedFetch({ onDelete: (u) => { urls.push(u); return new Response(null, { status: 204 }); } }) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await screen.findByText("Kho hàng");
    await pickRowAction("Kho hàng", "Xoá khoá");
    fireEvent.click(await screen.findByRole("button", { name: "Xoá khoá" }));
    await waitFor(() => expect(urls).toHaveLength(1));
    expect(urls[0]).toMatch(/\/admin\/credentials\/c-2$/);
  });

  it("delete 409 lists the rules that still use the key", async () => {
    global.fetch = routedFetch({
      onDelete: () => jsonRes({
        error: "in use", code: "CREDENTIAL_IN_USE",
        details: { usages: [{ workflowId: "w1", workflowName: "Báo đơn cho đối tác", actionIndex: 1 }] },
      }, 409),
    }) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await screen.findByText("Cổng thanh toán");
    await pickRowAction("Cổng thanh toán", "Xoá khoá");
    fireEvent.click(await screen.findByRole("button", { name: "Xoá khoá" }));
    expect(await screen.findByText("Báo đơn cho đối tác · bước 2")).toBeInTheDocument();
    expect(screen.getByText(t("page.credentials.inUse.title"))).toBeInTheDocument();
  });

  it("'Đang dùng' popover loads the rules from GET /:id", async () => {
    global.fetch = routedFetch({}) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await screen.findByText("Cổng thanh toán");
    fireEvent.click(screen.getByRole("button", { name: /2 quy trình/ }));
    expect(await screen.findByText("Báo đơn cho đối tác · bước 2")).toBeInTheDocument();
  });

  // ── S8b: sửa thông tin · xoay khoá · nhập khoá ──────────────────────────────────────────────
  it("row menu offers Sửa · Xoay khoá · Xoá, and 'Nhập khoá' instead of Xoay for a slot waiting for its key", async () => {
    global.fetch = routedFetch({}) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await screen.findByText("Kho hàng");
    const open = async (label: string) => {
      screen.getByRole("button", { name: `Thao tác cho khoá ${label}` }).focus();
      await userEvent.keyboard("{Enter}");
      return within(await screen.findByRole("menu")).getAllByRole("menuitem").map((e) => e.textContent);
    };
    expect(await open("Cổng thanh toán")).toEqual(["Sửa thông tin", "Xoay khoá", "Xoá khoá"]);
    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("menu")).not.toBeInTheDocument());
    expect(await open("Kho hàng")).toEqual(["Sửa thông tin", "Nhập khoá", "Xoá khoá"]);
  });

  it("edit sends only non-secret fields for the kind and closes on success", async () => {
    const calls: { url: string; body: Record<string, unknown> }[] = [];
    global.fetch = routedFetch({
      onUpdate: (url, body) => { calls.push({ url, body }); return jsonRes({ data: { credential: WAITING } }, 200); },
    }) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await screen.findByText("Kho hàng");
    await pickRowAction("Kho hàng", "Sửa thông tin");
    const dlg = await screen.findByRole("dialog");
    fireEvent.change(within(dlg).getByLabelText(/Tên hiển thị/), { target: { value: "Kho hàng miền Bắc" } });
    fireEvent.change(within(dlg).getByLabelText(/^Tên header$/), { target: { value: "X-Api-Key" } });
    fireEvent.click(within(dlg).getByRole("button", { name: /^Lưu$/ }));
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0].url).toMatch(/\/admin\/credentials\/c-2$/);
    expect(calls[0].body).toEqual({
      label: "Kho hàng miền Bắc", baseUrl: "https://pay.example.com", headerName: "X-Api-Key", headerPrefix: "",
    });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("edit to another host shows the origin refusal and a button that moves to Xoay khoá with the new address", async () => {
    global.fetch = routedFetch({
      onUpdate: () => jsonRes({
        error: "Đổi sang máy chủ khác phải làm bằng Xoay khoá kèm bí mật mới",
        code: "CREDENTIAL_ORIGIN_CHANGE_REQUIRES_ROTATE", details: { field: "baseUrl" },
      }, 422),
    }) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await screen.findByText("Cổng thanh toán");
    await pickRowAction("Cổng thanh toán", "Sửa thông tin");
    let dlg = await screen.findByRole("dialog");
    fireEvent.change(within(dlg).getByLabelText(/Địa chỉ gốc/), { target: { value: "https://other.example.org" } });
    fireEvent.click(within(dlg).getByRole("button", { name: /^Lưu$/ }));
    expect(await within(dlg).findByText(/Đổi sang máy chủ khác phải làm bằng Xoay khoá/)).toBeInTheDocument();
    fireEvent.click(within(dlg).getByRole("button", { name: /Chuyển sang xoay khoá/ }));
    await waitFor(() => expect(screen.getByRole("dialog", { name: /Xoay khoá/ })).toBeInTheDocument());
    dlg = screen.getByRole("dialog");
    expect(within(dlg).getByLabelText(/Địa chỉ gốc/)).toHaveValue("https://other.example.org");
    // New host: a new key is mandatory and the old extra headers do not travel.
    expect(within(dlg).getByText(/header bổ sung hiện có sẽ bị bỏ/i)).toBeInTheDocument();
  });

  it("rotate hmac: sends graceHours and the typed key, extra headers omitted unless replaced", async () => {
    const bodies: Record<string, unknown>[] = [];
    global.fetch = routedFetch({
      onRotate: (_u, b) => { bodies.push(b); return jsonRes({ data: { credential: CRED } }, 200); },
    }) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await screen.findByText("Cổng thanh toán");
    await pickRowAction("Cổng thanh toán", "Xoay khoá");
    const dlg = await screen.findByRole("dialog");
    expect(within(dlg).getByText("X-Tenant")).toBeInTheDocument();
    fireEvent.change(within(dlg).getByLabelText(/Khoá mới/), { target: { value: "new-key-123" } });
    fireEvent.change(within(dlg).getByLabelText(/Ân hạn/), { target: { value: "72" } });
    fireEvent.click(within(dlg).getByRole("button", { name: /^Xoay khoá$/ }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ secret: "new-key-123", graceHours: 72 });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("rotate with replaced extra headers sends them (THAY toàn bộ); a typed key is gone from the DOM after close", async () => {
    const bodies: Record<string, unknown>[] = [];
    global.fetch = routedFetch({
      onRotate: (_u, b) => { bodies.push(b); return jsonRes({ data: { credential: CRED } }, 200); },
    }) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await screen.findByText("Cổng thanh toán");
    await pickRowAction("Cổng thanh toán", "Xoay khoá");
    const dlg = await screen.findByRole("dialog");
    expect(within(dlg).getByText(/sẽ THAY toàn bộ/)).toBeInTheDocument();
    fireEvent.click(within(dlg).getByLabelText(/Thay header bổ sung/));
    fireEvent.click(within(dlg).getByRole("button", { name: /Thêm header/ }));
    fireEvent.change(within(dlg).getByLabelText(/^Tên header$/), { target: { value: "X-Env" } });
    fireEvent.change(within(dlg).getByLabelText(/^Giá trị$/), { target: { value: "prod" } });
    fireEvent.change(within(dlg).getByLabelText(/Khoá mới/), { target: { value: "typed-secret-xyz" } });
    fireEvent.click(within(dlg).getByRole("button", { name: /^Xoay khoá$/ }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ secret: "typed-secret-xyz", graceHours: 24, extraHeaders: { "X-Env": "prod" } });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(document.body.textContent).not.toContain("typed-secret-xyz");
  });

  describe("rotate body shape", () => {
    const bodies: Record<string, unknown>[] = [];
    const setup = async (cred: NamedCredential = CRED) => {
      bodies.length = 0;
      global.fetch = routedFetch({
        list: () => jsonRes({ data: { credentials: [cred, WAITING] } }, 200),
        onRotate: (_u, b) => { bodies.push(b); return jsonRes({ data: { credential: cred } }, 200); },
      }) as unknown as typeof fetch;
      render(<NamedCredentials />);
      await screen.findByText("Cổng thanh toán");
      await pickRowAction("Cổng thanh toán", "Xoay khoá");
      return screen.findByRole("dialog");
    };
    const send = (dlg: HTMLElement) => fireEvent.click(within(dlg).getByRole("button", { name: /^Xoay khoá$/ }));

    it("moved host hides the grace select and never sends graceHours", async () => {
      const dlg = await setup();
      expect(within(dlg).getByLabelText(/Ân hạn/)).toBeInTheDocument();
      fireEvent.change(within(dlg).getByLabelText(/Địa chỉ gốc/), { target: { value: "https://other.example.org" } });
      expect(within(dlg).queryByLabelText(/Ân hạn/)).not.toBeInTheDocument();
      fireEvent.change(within(dlg).getByLabelText(/Khoá mới/), { target: { value: "k-new" } });
      send(dlg);
      await waitFor(() => expect(bodies).toHaveLength(1));
      expect(bodies[0]).toEqual({ baseUrl: "https://other.example.org", secret: "k-new" });
    });

    it("extra headers only (no new key): no secret and no graceHours", async () => {
      const dlg = await setup();
      fireEvent.click(within(dlg).getByLabelText(/Thay header bổ sung/));
      fireEvent.click(within(dlg).getByRole("button", { name: /Thêm header/ }));
      fireEvent.change(within(dlg).getByLabelText(/^Tên header$/), { target: { value: "X-Env" } });
      fireEvent.change(within(dlg).getByLabelText(/^Giá trị$/), { target: { value: "prod" } });
      send(dlg);
      await waitFor(() => expect(bodies).toHaveLength(1));
      expect(bodies[0]).toEqual({ extraHeaders: { "X-Env": "prod" } });
    });

    it("grace 0 is sent as 0 (not dropped)", async () => {
      const dlg = await setup();
      fireEvent.change(within(dlg).getByLabelText(/Khoá mới/), { target: { value: "k-new" } });
      fireEvent.change(within(dlg).getByLabelText(/Ân hạn/), { target: { value: "0" } });
      send(dlg);
      await waitFor(() => expect(bodies).toHaveLength(1));
      expect(bodies[0]).toEqual({ secret: "k-new", graceHours: 0 });
    });

    it("header-type key never sends graceHours and has no grace select", async () => {
      const dlg = await setup({ ...CRED, authType: "header" });
      expect(within(dlg).queryByLabelText(/Ân hạn/)).not.toBeInTheDocument();
      fireEvent.change(within(dlg).getByLabelText(/Khoá mới/), { target: { value: "k-new" } });
      send(dlg);
      await waitFor(() => expect(bodies).toHaveLength(1));
      expect(bodies[0]).toEqual({ secret: "k-new" });
    });

    it("key_unavailable defaults to grace 0 with extra-headers replacement on", async () => {
      const dlg = await setup({ ...CRED, status: "key_unavailable" });
      expect(within(dlg).getByLabelText(/Thay header bổ sung/)).toBeChecked();
      fireEvent.change(within(dlg).getByLabelText(/Khoá mới/), { target: { value: "k-new" } });
      send(dlg);
      await waitFor(() => expect(bodies).toHaveLength(1));
      expect(bodies[0]).toEqual({ secret: "k-new", graceHours: 0, extraHeaders: {} });
    });
  });

  it("rotate hmac with 'Nexus tự sinh': shows the new key ONCE and forgets it after Xong", async () => {
    const bodies: Record<string, unknown>[] = [];
    global.fetch = routedFetch({
      onRotate: (_u, b) => { bodies.push(b); return jsonRes({ data: { credential: CRED, secret: GENERATED } }, 200); },
    }) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await screen.findByText("Cổng thanh toán");
    await pickRowAction("Cổng thanh toán", "Xoay khoá");
    const dlg = await screen.findByRole("dialog");
    fireEvent.click(within(dlg).getByRole("switch", { name: /Nexus tự sinh khoá/ }));
    fireEvent.click(within(dlg).getByRole("button", { name: /^Xoay khoá$/ }));
    await waitFor(() => expect(screen.getByTestId("credential-secret")).toHaveValue(GENERATED));
    expect(bodies[0]).toEqual({ generateSecret: true, graceHours: 24 });
    expect(screen.getByText(t("page.credentials.secret.once"))).toBeInTheDocument();
    expect(JSON.stringify({ ...window.localStorage })).not.toContain(GENERATED);
    fireEvent.click(screen.getByRole("button", { name: /Xong/ }));
    await waitFor(() => expect(screen.queryByTestId("credential-secret")).not.toBeInTheDocument());
    expect(document.body.textContent).not.toContain(GENERATED);
  });

  it("'Nhập khoá' on a waiting slot: titled so, no grace select, sends only the key", async () => {
    const bodies: Record<string, unknown>[] = [];
    global.fetch = routedFetch({
      onRotate: (_u, b) => { bodies.push(b); return jsonRes({ data: { credential: { ...WAITING, status: "ready" } } }, 200); },
    }) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await screen.findByText("Kho hàng");
    await pickRowAction("Kho hàng", "Nhập khoá");
    const dlg = await screen.findByRole("dialog", { name: /Nhập khoá/ });
    expect(within(dlg).queryByLabelText(/Ân hạn/)).not.toBeInTheDocument();
    fireEvent.change(within(dlg).getByLabelText(/Khoá mới/), { target: { value: "partner-token" } });
    fireEvent.click(within(dlg).getByRole("button", { name: /^Lưu khoá$/ }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toEqual({ secret: "partner-token" });
  });

  it("rotate 422 lands on the field named in details.field", async () => {
    global.fetch = routedFetch({
      onRotate: () => jsonRes({ error: "Xoay khoá cần ít nhất một thứ để thay", code: "CREDENTIAL_SECRET_REQUIRED", details: { field: "secret" } }, 422),
    }) as unknown as typeof fetch;
    render(<NamedCredentials />);
    await screen.findByText("Kho hàng");
    await pickRowAction("Kho hàng", "Nhập khoá");
    const dlg = await screen.findByRole("dialog");
    fireEvent.click(within(dlg).getByRole("button", { name: /^Lưu khoá$/ }));
    expect(await within(dlg).findByText("Xoay khoá cần ít nhất một thứ để thay")).toBeInTheDocument();
    expect(within(dlg).getByLabelText(/Khoá mới/)).toHaveAttribute("aria-invalid", "true");
  });
});
