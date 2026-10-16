// Pins the exact method + path of the owner's API client (specs/065 lát B). The orphan gate is blind to both:
// a GET that should be a PUT, or /org-admin/user, would still "have a caller".
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

type Call = { url: string; method: string; body?: unknown };

const originalFetch = global.fetch;
const jsonRes = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function mockFetch(): Call[] {
  const calls: Call[] = [];
  global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ url: String(url), method: init?.method ?? "GET", body: init?.body ? JSON.parse(String(init.body)) : undefined });
    return jsonRes({ data: { users: [], user: { id: "u1" }, roles: [] } });
  }) as unknown as typeof fetch;
  return calls;
}
const path = (c: Call) => `${c.method} ${c.url.split("/api/v1")[1] ?? c.url}`;

describe("owner API client", () => {
  beforeEach(() => {
    vi.resetModules();
    window.localStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "tok");
  });
  afterEach(() => {
    global.fetch = originalFetch;
    window.localStorage.clear();
  });

  it("hits /org-admin with the right method for every call", async () => {
    const calls = mockFetch();
    const org = await import("@/lib/orgAdmin");
    await org.listOrgUsers();
    await org.getOrgUser("u1");
    await org.updateOrgUser("u1", { roleId: "" });
    await org.listOrgRoles();
    expect(calls.map(path)).toEqual([
      "GET /org-admin/users",
      "GET /org-admin/users/u1",
      "PUT /org-admin/users/u1",
      "GET /org-admin/roles",
    ]);
    expect(calls[2].body).toEqual({ roleId: "" });
  });

  it("lát C: create, invite link and sessions hit the right method + path", async () => {
    const calls = mockFetch();
    const org = await import("@/lib/orgAdmin");
    await org.createOrgUser({ email: "a@b.vn", fullName: "A", roleId: "r1" });
    await org.issueOrgInviteLink("u1");
    await org.listOrgUserSessions("u1");
    await org.endOrgUserSession("u1", "s1");
    expect(calls.map(path)).toEqual([
      "POST /org-admin/users",
      "POST /org-admin/users/u1/reset-password-link",
      "GET /org-admin/users/u1/sessions",
      "DELETE /org-admin/users/u1/sessions/s1",
    ]);
    expect(calls[0].body).toEqual({ email: "a@b.vn", fullName: "A", roleId: "r1" });
  });

  it("the builder's switch is PUT /admin/users/:id/user-manager with {enabled}", async () => {
    const calls = mockFetch();
    const admin = await import("@/lib/admin");
    await admin.setUserManager("u1", true);
    expect(path(calls[0])).toBe("PUT /admin/users/u1/user-manager");
    expect(calls[0].body).toEqual({ enabled: true });
  });
});
