// specs/079 f7-sao-luu-bang T093: method + path of every backup call (the orphan gate is blind to method) and the
// download contract (header token, no token in the URL, abort ⇒ no click).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createTenantBackup, deleteTenantBackup, downloadTenantBackup, getTenantBackup, listTenantBackups } from "@/lib/api";

const calls: { url: string; init?: RequestInit }[] = [];
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const zip = () => new Response(new Blob(["PK"]), { status: 200 });

beforeEach(() => {
  calls.length = 0;
  window.localStorage.setItem("nexus_remember", "1");
  window.localStorage.setItem("nexus_token", "tok-1");
  URL.createObjectURL = vi.fn(() => "blob:x");
  URL.revokeObjectURL = vi.fn();
});
afterEach(() => {
  vi.unstubAllGlobals();
  window.localStorage.clear();
  window.sessionStorage.clear();
});

const stub = (fn: (url: string) => Response | Promise<Response>) =>
  vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => { calls.push({ url, init }); return fn(url); }));

describe("backup calls", () => {
  it("method + path of each", async () => {
    stub((u) => (u.endsWith("/backups") && calls.at(-1)?.init?.method === "POST" ? json({ data: { id: "b" } }, 202) : json({ data: [], meta: {} })));
    await listTenantBackups("t 1", { limit: 20, after: "c" });
    await createTenantBackup("t1", " ghi chú ");
    stub(() => json({ data: { id: "b" } }));
    await getTenantBackup("t1", "b1");
    stub(() => new Response(null, { status: 204 }));
    await deleteTenantBackup("t1", "b1");
    const seen = calls.map((c) => `${c.init?.method} ${c.url.replace(/^.*\/api\/v1/, "")}`);
    expect(seen).toEqual([
      "GET /admin/tenants/t%201/backups?limit=20&after=c",
      "POST /admin/tenants/t1/backups",
      "GET /admin/tenants/t1/backups/b1",
      "DELETE /admin/tenants/t1/backups/b1",
    ]);
    expect(JSON.parse(String(calls[1].init?.body))).toEqual({ note: "ghi chú" });
  });
});

describe("downloadTenantBackup", () => {
  it("GET …/file with the header token, none in the URL; clicks one <a download> and revokes at once", async () => {
    stub(() => zip());
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    const r = await downloadTenantBackup("t1", "b1", "acme-20261014-0805.zip");
    expect(calls[0].url).toMatch(/\/admin\/tenants\/t1\/backups\/b1\/file$/);
    expect(calls[0].url).not.toMatch(/token/i);
    expect((calls[0].init?.headers as Record<string, string>).Authorization).toBe("Bearer tok-1");
    expect(click).toHaveBeenCalledTimes(1);
    expect(URL.revokeObjectURL).toHaveBeenCalledWith("blob:x");
    expect(r.sizeBytes).toBe(2);
    click.mockRestore();
  });

  it("aborted while the bytes came ⇒ rejects and no link is clicked", async () => {
    const ac = new AbortController();
    stub(() => { ac.abort(); return zip(); });
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    await expect(downloadTenantBackup("t1", "b1", "x.zip", ac.signal)).rejects.toThrow();
    expect(click).not.toHaveBeenCalled();
    click.mockRestore();
  });

  it("a server refusal carries its code", async () => {
    stub(() => json({ code: "BACKUP_FILE_MISSING", error: "x" }, 409));
    await expect(downloadTenantBackup("t1", "b1", "x.zip")).rejects.toMatchObject({ status: 409, code: "BACKUP_FILE_MISSING" });
  });
});
