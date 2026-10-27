// Issue #22: on `next dev` the Studio prints `<dev origin>/mcp` for an agent (lib/admin.ts mcpEndpoint), and nothing
// served it. app/mcp proxies it to the API the way app/api/v1/[...path] proxies the API: Origin stripped, the bearer
// and body passed through, the answer mirrored; outside development both answer 404 and send nothing.
import { NextRequest } from "next/server";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { POST as mcpPost } from "../app/mcp/route";
import { GET as apiGet } from "../app/api/v1/[...path]/route";

const upstream = vi.fn<(url: string, init?: RequestInit) => Promise<Response>>(async () =>
  new Response('{"jsonrpc":"2.0","id":1,"result":{}}', { status: 200, headers: { "content-type": "application/json", "x-internal": "1" } }));

beforeEach(() => {
  upstream.mockClear();
  vi.stubGlobal("fetch", upstream);
  vi.stubEnv("DEV_API_PROXY_TARGET", "http://127.0.0.1:18090");
});
afterEach(() => { vi.unstubAllGlobals(); vi.unstubAllEnvs(); });

const mcpCall = () => new NextRequest("http://localhost:3000/mcp", {
  method: "POST",
  body: '{"jsonrpc":"2.0","id":1,"method":"tools/list"}',
  headers: { authorization: "Bearer nxm_x", accept: "application/json, text/event-stream", origin: "http://localhost:3000", "content-type": "application/json" },
});

describe("dev proxy", () => {
  it("on next dev, /mcp reaches the API's /mcp with the bearer and body and without Origin", async () => {
    vi.stubEnv("NODE_ENV", "development");
    const res = await mcpPost(mcpCall());
    expect(upstream).toHaveBeenCalledTimes(1);
    const [url, init] = upstream.mock.calls[0];
    expect(url).toBe("http://127.0.0.1:18090/mcp");
    const sent = new Headers(init?.headers);
    expect(sent.get("authorization")).toBe("Bearer nxm_x");
    expect(sent.get("accept")).toBe("application/json, text/event-stream");
    expect(sent.has("origin")).toBe(false);
    expect(new TextDecoder().decode(init?.body as ArrayBuffer)).toContain("tools/list");
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toBe("application/json");
    expect(res.headers.has("x-internal")).toBe(false);
    expect(await res.text()).toContain('"result"');
  });

  it("the API route keeps its path and query", async () => {
    vi.stubEnv("NODE_ENV", "development");
    await apiGet(new NextRequest("http://localhost:3000/api/v1/admin/mcp-tokens?page=2"), { params: Promise.resolve({ path: ["admin", "mcp-tokens"] }) });
    expect(upstream.mock.calls[0][0]).toBe("http://127.0.0.1:18090/api/v1/admin/mcp-tokens?page=2");
  });

  it("outside development both answer 404 and send nothing", async () => {
    vi.stubEnv("NODE_ENV", "production");
    expect((await mcpPost(mcpCall())).status).toBe(404);
    expect((await apiGet(new NextRequest("http://localhost:3000/api/v1/x"), { params: Promise.resolve({ path: ["x"] }) })).status).toBe(404);
    expect(upstream).not.toHaveBeenCalled();
  });
});
