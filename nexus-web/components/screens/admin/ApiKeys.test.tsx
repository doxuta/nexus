// specs/078 lát u7-khoa-truy-cap (FR-078, FR-006): access keys inside the Studio frame.
// Pins the SECURITY-CRITICAL contract:
//   * create happens IN the page (0 dialog); the form turns into SecretOnceBlock, "Xong" off until "Tôi đã lưu";
//   * after "Xong" the secret is in the DOM / URL / local+sessionStorage / query cache / MUTATION cache 0 times
//     (createApiKey is a plain await, never a useMutation);
//   * the list shows only the prefix; revoke asks in the row (InlineConfirm) and DELETEs the right id;
//   * the token of another customer fetches nothing.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { ApiKeys, keyStatus } from "@/components/screens/admin/ApiKeys";
import { logout } from "@/lib/api";
import type { ApiKey } from "@/lib/admin";

let tokenOk = true;
let scopeOk = true;
vi.mock("@/lib/studioContext", () => ({ useStudioIdentity: () => ({ matches: () => tokenOk }), activeProjectSlug: () => (scopeOk ? "northwind" : "khac"), projectSlugAt: () => "northwind", transitionPhase: () => "idle" }));
vi.mock("next/navigation", () => ({ usePathname: () => "/studio/du-an/northwind/van-hanh/tu-dong/khoa-truy-cap" }));

const jsonRes = (body: unknown, status: number) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const SECRET = "nxk_AbCdEfGhIjKl0123456789_-abcdefghijklmnopq";
const LIVE_KEY: ApiKey = { id: "key-1", tenantId: "t-1", roleId: "role-1", name: "CI importer", keyPrefix: "AbCdEfGhIjKl", createdAt: "2026-07-11T09:00:00Z", updatedAt: "2026-07-11T09:00:00Z", lastUsedAt: "2026-07-11T10:00:00Z" };
const REVOKED_KEY: ApiKey = { ...LIVE_KEY, id: "key-2", name: "Old bridge", keyPrefix: "ZyXwVuTsRqPo", revokedAt: "2026-07-10T00:00:00Z" };
const EXPIRED_KEY: ApiKey = { ...LIVE_KEY, id: "key-3", name: "Short lived", keyPrefix: "QqWwEeRrTtYy", expiresAt: "2026-01-01T00:00:00Z" };

function routedFetch(keys: ApiKey[], opts?: { onCreate?: (init?: RequestInit) => Response; onDelete?: (url: string) => void }) {
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    if (u.includes("/admin/api-keys") && method === "GET") return jsonRes({ data: { apiKeys: keys } }, 200);
    if (u.includes("/admin/api-keys") && method === "POST") return opts?.onCreate ? opts.onCreate(init) : jsonRes({ data: { apiKey: LIVE_KEY, secret: SECRET } }, 201);
    if (u.includes("/admin/api-keys/") && method === "DELETE") { opts?.onDelete?.(u); return new Response(null, { status: 204 }); }
    if (u.includes("/admin/roles") && method === "GET") return jsonRes({ data: { roles: [{ id: "role-1", name: "machine", label: "Máy tích hợp" }] } }, 200);
    return jsonRes({ error: `unmocked ${method} ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
}

let qc: QueryClient;
const ui = (slug: string) => <QueryClientProvider client={qc}><EmbeddedSurface><ApiKeys slug={slug} /></EmbeddedSurface></QueryClientProvider>;
const mount = () => {
  qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(ui("northwind"));
};
const fillAndCreate = async (name = "CI importer") => {
  fireEvent.click(await screen.findByRole("button", { name: "Tạo khoá truy cập" }));
  fireEvent.change(await screen.findByLabelText(/Tên khoá/), { target: { value: name } });
  fireEvent.change(screen.getByLabelText(/Vai/), { target: { value: "role-1" } });
  fireEvent.click(screen.getByRole("button", { name: "Tạo khoá" }));
};

describe("ApiKeys (Studio, in place)", () => {
  const realFetch = global.fetch;
  beforeEach(async () => {
    tokenOk = true;
    scopeOk = true;
    await logout();
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
  });
  afterEach(() => { cleanup(); global.fetch = realFetch; window.localStorage.clear(); window.sessionStorage.clear(); });

  it("lists name, role, prefix, expiry and a plain-text status; never a secret; no dialog", async () => {
    global.fetch = routedFetch([LIVE_KEY, REVOKED_KEY, EXPIRED_KEY]) as unknown as typeof fetch;
    mount();
    await screen.findByText("CI importer");
    expect(screen.getByText("nxk_AbCdEfGhIjKl…")).toBeInTheDocument();
    expect(screen.getAllByText("Máy tích hợp").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText("Đang dùng").className).toContain("nx-status");
    expect(screen.getByText("Đã thu hồi").className).toContain("nx-status");
    expect(screen.getByText("Hết hạn", { selector: ".nx-status" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Thu hồi khoá Old bridge/ })).toBeNull();
    expect(document.body.textContent).not.toContain(SECRET);
    expect(document.body.textContent).not.toMatch(/Bearer|API/);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("empty list says so in the maker's words", async () => {
    global.fetch = routedFetch([]) as unknown as typeof fetch;
    mount();
    await screen.findByText(/Chưa có khoá truy cập nào/);
  });

  it("create turns the form into the one-time block; after Xong the secret is nowhere", async () => {
    const bodies: unknown[] = [];
    global.fetch = routedFetch([], { onCreate: (init) => { bodies.push(JSON.parse(String(init?.body))); return jsonRes({ data: { apiKey: LIVE_KEY, secret: SECRET } }, 201); } }) as unknown as typeof fetch;
    mount();
    await fillAndCreate();
    // the form is gone, the block took its place — and still no dialog
    expect(await screen.findByText(SECRET)).toBeInTheDocument();
    expect(screen.queryByLabelText(/Tên khoá/)).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByText("Sao chép khoá ngay: sẽ không hiện lại")).toBeInTheDocument();
    expect(bodies[0]).toEqual({ name: "CI importer", roleId: "role-1" });
    // "Xong" stays off until the tick
    const done = screen.getByRole("button", { name: "Xong" });
    expect(done).toBeDisabled();
    fireEvent.click(screen.getByLabelText("Tôi đã lưu"));
    expect(done).toBeEnabled();
    fireEvent.click(done);
    await waitFor(() => expect(screen.queryByText(SECRET)).toBeNull());
    expect(document.body.innerHTML).not.toContain(SECRET);
    expect(window.location.href).not.toContain(SECRET);
    expect(JSON.stringify({ ...window.localStorage })).not.toContain(SECRET);
    expect(JSON.stringify({ ...window.sessionStorage })).not.toContain(SECRET);
    expect(JSON.stringify(qc.getQueryCache().getAll().map((q) => q.state.data))).not.toContain(SECRET);
    // createApiKey is a plain await: nothing for the mutation cache to hold
    expect(qc.getMutationCache().getAll()).toHaveLength(0);
    expect(JSON.stringify(qc.getMutationCache().getAll())).not.toContain(SECRET);
  });

  it("leaving before Xong is blocked (reload / close tab)", async () => {
    global.fetch = routedFetch([]) as unknown as typeof fetch;
    mount();
    await fillAndCreate();
    await screen.findByText(SECRET);
    const ev = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(ev);
    expect(ev.defaultPrevented).toBe(true);
  });

  it("a missing name or role is flagged on the field and nothing is sent", async () => {
    const bodies: unknown[] = [];
    global.fetch = routedFetch([], { onCreate: (init) => { bodies.push(init?.body); return jsonRes({}, 500); } }) as unknown as typeof fetch;
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Tạo khoá truy cập" }));
    fireEvent.click(await screen.findByRole("button", { name: "Tạo khoá" }));
    const name = screen.getByLabelText(/Tên khoá/);
    await waitFor(() => expect(name).toHaveAttribute("aria-invalid", "true"));
    expect(screen.getByLabelText(/Vai/)).toHaveAttribute("aria-invalid", "true");
    expect(name).toHaveFocus();
    expect(bodies).toHaveLength(0);
  });

  it.each([
    ["APIKEY_ROLE_INVALID", /Vai/, "Vai không hợp lệ"],
    ["APIKEY_EXPIRY_INVALID", /Hạn dùng/, "Hạn dùng phải ở tương lai"],
  ])("%s lands on its own field", async (code, label, text) => {
    global.fetch = routedFetch([], { onCreate: () => jsonRes({ error: "x", code }, 422) }) as unknown as typeof fetch;
    mount();
    await fillAndCreate();
    const field = await screen.findByLabelText(label);
    await waitFor(() => expect(field).toHaveAttribute("aria-invalid", "true"));
    expect(field).toHaveAccessibleDescription(expect.stringContaining(text));
    expect(screen.queryByText(SECRET)).toBeNull();
  });

  it("revoke asks in the row, naming the consequence, then DELETEs the right id", async () => {
    const deleted: string[] = [];
    global.fetch = routedFetch([LIVE_KEY], { onDelete: (u) => deleted.push(u) }) as unknown as typeof fetch;
    mount();
    await screen.findByText("CI importer");
    fireEvent.click(screen.getByRole("button", { name: /Thu hồi khoá CI importer/ }));
    expect(deleted).toHaveLength(0);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByRole("alertdialog")).toBeNull();
    const ask = screen.getByText(/sẽ bị từ chối ngay/);
    expect(ask.textContent).toContain("nxk_AbCdEfGhIjKl…");
    const box = ask.closest("[tabindex='-1']") as HTMLElement;
    fireEvent.click(within(box).getByRole("button", { name: "Thu hồi" }));
    await waitFor(() => expect(deleted).toHaveLength(1));
    expect(deleted[0]).toContain("/admin/api-keys/key-1");
  });

  it("the token does not belong to this project: nothing is fetched or drawn", async () => {
    tokenOk = false;
    const f = routedFetch([LIVE_KEY]);
    global.fetch = f as unknown as typeof fetch;
    mount();
    await new Promise((r) => setTimeout(r, 50));
    expect(f).not.toHaveBeenCalled();
    expect(screen.queryByText("CI importer")).toBeNull();
  });

  it("switching customer drops an open form and a revealed secret", async () => {
    global.fetch = routedFetch([LIVE_KEY]) as unknown as typeof fetch;
    const { rerender } = mount();
    fireEvent.click(await screen.findByRole("button", { name: "Tạo khoá truy cập" }));
    fireEvent.change(await screen.findByLabelText(/Tên khoá/), { target: { value: "dang-go" } });
    rerender(ui("khac"));
    await waitFor(() => expect(screen.queryByDisplayValue("dang-go")).toBeNull());
    expect(screen.queryByLabelText(/Tên khoá/)).toBeNull();
    // a revealed secret does not survive the switch either
    await fillAndCreate();
    await screen.findByText(SECRET);
    rerender(ui("khac-nua"));
    await waitFor(() => expect(screen.queryByText(SECRET)).toBeNull());
    expect(document.body.innerHTML).not.toContain(SECRET);
  });

  it("assertScope false on create: 0 POST and the scope message", async () => {
    const posts: unknown[] = [];
    global.fetch = routedFetch([], { onCreate: (init) => { posts.push(init?.body); return jsonRes({}, 500); } }) as unknown as typeof fetch;
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Tạo khoá truy cập" }));
    fireEvent.change(await screen.findByLabelText(/Tên khoá/), { target: { value: "x" } });
    await screen.findByRole("option", { name: "Máy tích hợp" });
    fireEvent.change(screen.getByLabelText(/Vai/), { target: { value: "role-1" } });
    scopeOk = false;
    fireEvent.click(screen.getByRole("button", { name: "Tạo khoá" }));
    await screen.findByText(/chưa tạo khoá nào/);
    expect(posts).toHaveLength(0);
  });

  it("assertScope false on revoke: 0 DELETE and the scope message, not the generic retry", async () => {
    const deleted: string[] = [];
    global.fetch = routedFetch([LIVE_KEY], { onDelete: (u) => deleted.push(u) }) as unknown as typeof fetch;
    mount();
    await screen.findByText("CI importer");
    fireEvent.click(screen.getByRole("button", { name: /Thu hồi khoá CI importer/ }));
    const box = screen.getByText(/sẽ bị từ chối ngay/).closest("[tabindex='-1']") as HTMLElement;
    scopeOk = false;
    fireEvent.click(within(box).getByRole("button", { name: "Thu hồi" }));
    await screen.findByText(/chưa thu hồi khoá nào/);
    expect(deleted).toHaveLength(0);
    expect(screen.queryByText(/Thử lại sau ít phút/)).toBeNull();
  });

  it("roles failing to load says so on the form", async () => {
    const base = routedFetch([]);
    global.fetch = vi.fn(async (u: RequestInfo | URL, i?: RequestInit) => (String(u).includes("/admin/roles") ? jsonRes({ error: "x", code: "X" }, 500) : base(u, i))) as unknown as typeof fetch;
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Tạo khoá truy cập" }));
    await screen.findByText(/Không tải được danh sách vai/);
  });
});

describe("keyStatus", () => {
  it("revoked wins over expiry; future expiry stays active", () => {
    const now = new Date("2026-07-11T12:00:00Z");
    expect(keyStatus({ revokedAt: "2026-07-10T00:00:00Z", expiresAt: "2026-07-01T00:00:00Z" }, now)).toBe("revoked");
    expect(keyStatus({ expiresAt: "2026-07-01T00:00:00Z" }, now)).toBe("expired");
    expect(keyStatus({ expiresAt: "2027-01-01T00:00:00Z" }, now)).toBe("active");
    expect(keyStatus({}, now)).toBe("active");
  });
});
