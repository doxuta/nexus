// Issue #3 — "Kết nối agent (MCP)" on the access keys page. Pinned here:
//   * create happens in the page (0 dialog) and shows the key ONCE, with the exact `claude mcp add` line;
//     after "Xong" the key is in the DOM / storage / query cache / mutation cache 0 times;
//   * the list shows only the start of each key; revoke asks in the row and DELETEs the right id;
//   * "Mở cuộc trò chuyện" opens the key's conversation in the assistant, and is absent where the assistant is.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { logout } from "@/lib/api";
import { claudeMcpAddCommand, type McpToken } from "@/lib/admin";

vi.mock("@/lib/studioContext", () => ({ useStudioIdentity: () => ({ matches: () => true }), activeProjectSlug: () => "northwind", projectSlugAt: () => "northwind", transitionPhase: () => "idle" }));
vi.mock("next/navigation", () => ({ usePathname: () => "/studio/du-an/northwind/van-hanh/tu-dong/khoa-truy-cap" }));
const openThread = vi.fn(async () => {});
let assistantHere = true;
vi.mock("@/components/studio/assistant/AssistantProvider", () => ({ useOpenThread: () => (assistantHere ? openThread : null) }));

import { McpConnect } from "@/components/screens/admin/McpConnect";

const jsonRes = (body: unknown, status: number) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const SECRET = "nxm_AbCdEfGhIjKl0123456789_-abcdefghijklmnopq";
const LIVE: McpToken = { id: "tok-1", sessionId: "sess-1", tokenPrefix: "AbCdEfGhIjKl", expiresAt: "2026-10-17T11:00:00Z", createdAt: "2026-10-17T03:00:00Z", lastUsedAt: "2026-10-17T03:05:00Z" };
const FRESH: McpToken = { ...LIVE, id: "tok-2", sessionId: "sess-2", tokenPrefix: "ZyXwVuTsRqPo", lastUsedAt: undefined };

function routedFetch(tokens: McpToken[], opts?: { onCreate?: (init?: RequestInit) => Response; onDelete?: (url: string) => void }) {
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    if (u.endsWith("/admin/mcp-tokens") && method === "GET") return jsonRes({ data: { tokens } }, 200);
    if (u.endsWith("/admin/mcp-tokens") && method === "POST") return opts?.onCreate ? opts.onCreate(init) : jsonRes({ data: { token: LIVE, secret: SECRET } }, 201);
    if (u.includes("/admin/mcp-tokens/") && method === "DELETE") { opts?.onDelete?.(u); return new Response(null, { status: 204 }); }
    return jsonRes({ error: `unmocked ${method} ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
}

let qc: QueryClient;
const mount = () => {
  qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}><EmbeddedSurface><McpConnect slug="northwind" /></EmbeddedSurface></QueryClientProvider>);
};

describe("McpConnect", () => {
  const realFetch = global.fetch;
  beforeEach(async () => {
    assistantHere = true;
    openThread.mockClear();
    await logout();
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
  });
  afterEach(() => { cleanup(); global.fetch = realFetch; window.localStorage.clear(); window.sessionStorage.clear(); });

  it("the command line is the exact Claude Code call", () => {
    expect(claudeMcpAddCommand("nxm_x", "https://app.example.com/mcp")).toBe(
      'claude mcp add --transport http nexus https://app.example.com/mcp --header "Authorization: Bearer nxm_x"',
    );
  });

  it("lists the live keys by their start only; no secret; no dialog", async () => {
    global.fetch = routedFetch([LIVE, FRESH]) as unknown as typeof fetch;
    mount();
    expect(await screen.findByText("nxm_AbCdEfGhIjKl…")).toBeInTheDocument();
    expect(screen.getByText("nxm_ZyXwVuTsRqPo…")).toBeInTheDocument();
    expect(screen.getByText("Chưa dùng")).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(SECRET);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("empty list says what to do", async () => {
    global.fetch = routedFetch([]) as unknown as typeof fetch;
    mount();
    expect(await screen.findByText(/Chưa có agent nào được kết nối/)).toBeInTheDocument();
  });

  it("create shows the key once with the claude mcp add line; after Xong it is nowhere", async () => {
    const bodies: unknown[] = [];
    global.fetch = routedFetch([], { onCreate: (init) => { bodies.push(JSON.parse(String(init?.body))); return jsonRes({ data: { token: LIVE, secret: SECRET } }, 201); } }) as unknown as typeof fetch;
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Tạo khoá cho agent" }));
    expect(await screen.findByText(SECRET)).toBeInTheDocument();
    expect(bodies).toEqual([{}]);
    const line = screen.getByText((_, el) => el?.tagName === "CODE" && el.textContent!.startsWith("claude mcp add"));
    expect(line.textContent).toMatch(/^claude mcp add --transport http nexus https?:\/\/[^ ]+\/mcp --header "Authorization: Bearer nxm_AbCdEfGhIjKl0123456789_-abcdefghijklmnopq"$/);
    expect(screen.getByRole("button", { name: "Sao chép Lệnh cho Claude Code" })).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
    fireEvent.click(screen.getByLabelText("Tôi đã lưu"));
    fireEvent.click(screen.getByRole("button", { name: "Xong" }));
    await waitFor(() => expect(screen.queryByText(SECRET)).toBeNull());
    expect(document.body.innerHTML).not.toContain(SECRET);
    expect(JSON.stringify({ ...window.localStorage, ...window.sessionStorage })).not.toContain(SECRET);
    expect(JSON.stringify(qc.getQueryCache().getAll().map((q) => q.state.data))).not.toContain(SECRET);
    expect(qc.getMutationCache().getAll()).toHaveLength(0);
  });

  it("inside a customer's project the refusal says why", async () => {
    global.fetch = routedFetch([], { onCreate: () => jsonRes({ error: "x", code: "MCP_TOKEN_WORKSPACE_UNSUPPORTED" }, 422) }) as unknown as typeof fetch;
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Tạo khoá cho agent" }));
    expect(await screen.findByText(/chỉ tạo được trong dự án của chính bạn/)).toBeInTheDocument();
  });

  it("revoke asks in the row and deletes that key", async () => {
    const deleted: string[] = [];
    global.fetch = routedFetch([LIVE], { onDelete: (u) => deleted.push(u) }) as unknown as typeof fetch;
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Thu hồi khoá nxm_AbCdEfGhIjKl…" }));
    expect(screen.getByText(/sẽ bị từ chối ngay/)).toBeInTheDocument();
    expect(deleted).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Thu hồi" }));
    await waitFor(() => expect(deleted).toHaveLength(1));
    expect(deleted[0]).toMatch(/\/admin\/mcp-tokens\/tok-1$/);
  });

  it("Mở cuộc trò chuyện opens the key's conversation in the assistant", async () => {
    global.fetch = routedFetch([LIVE]) as unknown as typeof fetch;
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Mở cuộc trò chuyện của khoá nxm_AbCdEfGhIjKl…" }));
    await waitFor(() => expect(openThread).toHaveBeenCalledWith("sess-1"));
  });

  it("where the assistant cannot open, there is no open button", async () => {
    assistantHere = false;
    global.fetch = routedFetch([LIVE]) as unknown as typeof fetch;
    mount();
    await screen.findByText("nxm_AbCdEfGhIjKl…");
    expect(screen.queryByRole("button", { name: /Mở cuộc trò chuyện/ })).toBeNull();
  });
});
