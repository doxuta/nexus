// specs/073 so1a T454: the project-registry client (062 L5) — token, verbs, bodies, `data` unwrapping, typed errors.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

type Call = { method: string; path: string; auth: string | undefined; body: unknown };

function mockServer(answer: (c: Call) => Response): Call[] {
  const calls: Call[] = [];
  global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const call: Call = {
      method: init?.method ?? "GET",
      path: String(url).replace(/^https?:\/\/[^/]+\/api\/v1/, ""),
      auth: (init?.headers as Record<string, string> | undefined)?.Authorization,
      body: init?.body ? JSON.parse(String(init.body)) : undefined,
    };
    calls.push(call);
    return answer(call);
  }) as unknown as typeof fetch;
  return calls;
}
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

describe("project registry client", () => {
  const originalFetch = global.fetch;
  beforeEach(() => {
    vi.resetModules();
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.localStorage.setItem("nexus_token", "HOME-TOK");
  });
  afterEach(() => {
    global.fetch = originalFetch;
    window.localStorage.clear();
  });

  it("listProjects: GET /admin/projects with the home token, unwraps data", async () => {
    const calls = mockServer(() => json({ data: [{ tenantId: "t-1", slug: "x", stage: "building" }] }));
    const { listProjects } = await import("@/lib/api");
    const rows = await listProjects();
    expect(calls).toEqual([{ method: "GET", path: "/admin/projects", auth: "Bearer HOME-TOK", body: undefined }]);
    expect(rows).toHaveLength(1);
    expect(rows[0].slug).toBe("x");
  });

  it("listProjects: a null data is an empty list; a 403 stays an ApiError the screen can read", async () => {
    mockServer(() => json({ data: null }));
    const api = await import("@/lib/api");
    expect(await api.listProjects()).toEqual([]);
    mockServer(() => json({ error: "Không đủ quyền", code: "FORBIDDEN" }, 403));
    await expect(api.listProjects()).rejects.toMatchObject({ status: 403, code: "FORBIDDEN" });
  });

  it("patchProject: PATCH sends exactly the two keys it was given, returns the updated row", async () => {
    const calls = mockServer(() => json({ data: { tenantId: "t-1", stage: "trial" } }));
    const { patchProject } = await import("@/lib/api");
    const row = await patchProject("t-1", { trial: true });
    expect(calls[0]).toMatchObject({ method: "PATCH", path: "/admin/projects/t-1", body: { trial: true } });
    expect(row.stage).toBe("trial");
    await patchProject("t-1", { industry: null });
    expect(calls[1].body).toEqual({ industry: null });
    await patchProject("t/1?x", { industry: "Thú y", trial: false });
    expect(calls[2].path).toBe("/admin/projects/t%2F1%3Fx");
  });

  it("patchProject: the typed refusals reach the caller with their code", async () => {
    mockServer(() => json({ error: "dự án đã giao — không bật Chạy thử", code: "PROJECT_TRIAL_AFTER_DELIVERY" }, 422));
    const { patchProject } = await import("@/lib/api");
    await expect(patchProject("t-1", { trial: true })).rejects.toMatchObject({ status: 422, code: "PROJECT_TRIAL_AFTER_DELIVERY" });
    mockServer(() => json({ error: "ngành quá dài", code: "PROJECT_INDUSTRY_TOO_LONG" }, 422));
    await expect(patchProject("t-1", { industry: "x".repeat(300) })).rejects.toMatchObject({ code: "PROJECT_INDUSTRY_TOO_LONG" });
  });

  it("healthCheckProject: POST …/health-check, no body, unwraps the stored snapshot", async () => {
    const calls = mockServer(() => json({ data: { status: "warn", critical: 0, warn: 2, checks: [], appsNotReady: [], checkedAt: "2026-10-09T00:00:00Z" } }));
    const { healthCheckProject } = await import("@/lib/api");
    const h = await healthCheckProject("t-1");
    expect(calls[0]).toMatchObject({ method: "POST", path: "/admin/projects/t-1/health-check", auth: "Bearer HOME-TOK" });
    expect(h.status).toBe("warn");
  });

  it("createProject: POST /admin/projects with the home token, unwraps project/start/one-time password, typed refusals", async () => {
    const calls = mockServer(() => json({ data: { project: { slug: "x" }, admin: { email: "a@b.vn" }, start: { mode: "pack", status: "failed", error: { code: "E", message: "m" } }, oneTimeCredentials: { password: "pw-1" } } }, 201));
    const { createProject } = await import("@/lib/api");
    const out = await createProject({ name: "X", slug: "x", adminEmail: "a@b.vn", generatePassword: true, start: { mode: "pack", pack: "clinic" } });
    expect(calls[0]).toMatchObject({ method: "POST", path: "/admin/projects", auth: "Bearer HOME-TOK", body: { slug: "x", start: { mode: "pack", pack: "clinic" } } });
    expect(out.password).toBe("pw-1");
    expect(out.start.status).toBe("failed");
    expect(out.slug).toBe("x");
    mockServer(() => json({ error: "đã có", code: "TENANT_ALREADY_EXISTS" }, 409));
    await expect(createProject({ name: "X", slug: "x", adminEmail: "a@b.vn", start: { mode: "blank" } })).rejects.toMatchObject({ status: 409, code: "TENANT_ALREADY_EXISTS" });
  });
});
