// specs/065 lát D (FR-035): the orphan-endpoint gate is blind to HTTP method and to path prefix, so the
// two owner-tier client functions are pinned here to the exact method + path they must call.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { logout, revertOwnerTier, setOwnerTier } from "@/lib/api";

function jsonRes(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

describe("owner tier client", () => {
  let originalFetch: typeof fetch;

  beforeEach(async () => {
    originalFetch = global.fetch;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout();
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  afterEach(() => {
    global.fetch = originalFetch;
  });

  it("setOwnerTier('t1') => POST /admin/tenants/t1/owner-tier, returns the counts", async () => {
    const fetchMock = vi.fn(async () => jsonRes({ data: { converted: 2, skippedWithRole: 1, ownerRoleId: "r1", revokeFailed: 0 } }));
    global.fetch = fetchMock as unknown as typeof fetch;
    const r = await setOwnerTier("t1");
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(String(url)).toMatch(/\/admin\/tenants\/t1\/owner-tier$/);
    expect(init.method).toBe("POST");
    expect(r).toEqual({ converted: 2, skippedWithRole: 1, ownerRoleId: "r1", revokeFailed: 0 });
  });

  it("revertOwnerTier('t1') => DELETE /admin/tenants/t1/owner-tier, returns the counts", async () => {
    const fetchMock = vi.fn(async () => jsonRes({ data: { reverted: 3, revokeFailed: 1 } }));
    global.fetch = fetchMock as unknown as typeof fetch;
    const r = await revertOwnerTier("t1");
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(String(url)).toMatch(/\/admin\/tenants\/t1\/owner-tier$/);
    expect(init.method).toBe("DELETE");
    expect(r).toEqual({ reverted: 3, revokeFailed: 1 });
  });
});
