// specs/078 lát u7-du-lieu-thung-rac-xoa-han T092: the orphan-endpoint gate is blind to HTTP method and path prefix,
// so purgeRecord is pinned to the exact method + path + body here; canPurge / purgeSummary / purgeRefusal are pure.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, logout, purgeRecord } from "@/lib/api";
import { canPurge, purgeRefusal, purgeSummary } from "@/lib/operateData";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const REPORT = {
  entity: "crm_accounts", id: "r1", dryRun: true,
  records: [{ entity: "crm_accounts", id: "r1" }, { entity: "crm_contacts", id: "c1" }],
  cleared: [{ entity: "crm_deals", id: "d1", field: "account_id" }, { entity: "crm_deals", id: "d2", field: "account_id" }],
  blocked: [], tables: [{ table: "obj_crm_accounts", rows: 1 }, { table: "sys_files", rows: 3 }], totalRows: 6, blobsRemoved: 0,
  blobsNote: "3 tệp đính kèm sẽ bị xoá khỏi kho lưu trữ",
};

describe("purgeRecord", () => {
  let originalFetch: typeof fetch;
  beforeEach(async () => {
    originalFetch = global.fetch;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout();
    window.localStorage.clear();
    window.sessionStorage.clear();
  });
  afterEach(() => { global.fetch = originalFetch; });

  const call = async (dryRun: boolean, status = 200, body: unknown = { data: REPORT }) => {
    const f = vi.fn(async () => json(body, status));
    global.fetch = f as unknown as typeof fetch;
    const out = await purgeRecord("crm_accounts", "r1", { dryRun }).catch((e) => e);
    const [url, init] = f.mock.calls[0] as unknown as [string, RequestInit];
    return { out, url: String(url), init };
  };

  it.each([true, false])("POST /crm_accounts/r1/purge with an EXPLICIT dryRun:%s body", async (dryRun) => {
    const { out, url, init } = await call(dryRun);
    expect(url).toMatch(/\/crm_accounts\/r1\/purge$/);
    expect(init.method).toBe("POST");
    expect(JSON.parse(String(init.body))).toEqual({ dryRun });
    expect(out.records).toHaveLength(2);
    expect(out.tables[1]).toEqual({ table: "sys_files", rows: 3 });
  });

  it("arrays are always arrays, even when the server leaves them out", async () => {
    const { out } = await call(true, 200, { data: { entity: "e", id: "i", dryRun: true, totalRows: 0, blobsRemoved: 0 } });
    expect(out).toMatchObject({ records: [], cleared: [], blocked: [], tables: [] });
  });

  it("encodes entity and id: '../x' never becomes another route", async () => {
    const f = vi.fn(async () => json({ data: REPORT }));
    global.fetch = f as unknown as typeof fetch;
    await purgeRecord("crm_accounts", "../x", { dryRun: true });
    expect(String((f.mock.calls[0] as unknown as [string])[0])).toMatch(/\/crm_accounts\/\.\.%2Fx\/purge$/);
    f.mockClear();
    await expect(purgeRecord("..", "r1", { dryRun: true })).rejects.toThrow();
    expect(f).not.toHaveBeenCalled();
  });

  it.each([
    [409, "RECORD_PURGE_LIVE_CHILDREN", { blockers: [{ entity: "crm_deals", field: "account_id", count: 2 }] }],
    [409, "RECORD_NOT_IN_RECYCLE_BIN", {}],
    [422, "RECORD_PURGE_TOO_LARGE", { max: 500 }],
    [503, "RECORD_WRITE_CONFLICT", {}],
    [403, "FORBIDDEN", {}],
    [404, "RECORD_NOT_FOUND", {}],
  ])("%i %s becomes an ApiError with that code", async (status, code, details) => {
    const { out } = await call(false, status, { error: "x", code, details });
    expect(out).toBeInstanceOf(ApiError);
    expect(out).toMatchObject({ status, code });
    expect(out.details).toMatchObject(details);
  });
});

describe("canPurge (fail closed, FR-058)", () => {
  it("loading / error => false", () => {
    expect(canPurge({ status: "loading", user: { isPlatformOperator: true } })).toBe(false);
    expect(canPurge({ status: "error", user: { isPlatformOperator: true } })).toBe(false);
  });
  it("ok + operator => true; legacy customer admin => false", () => {
    expect(canPurge({ status: "ok", user: { isPlatformOperator: true } })).toBe(true);
    expect(canPurge({ status: "ok", user: { isPlatformOperator: false } })).toBe(false);
    expect(canPurge({ status: "ok", user: {} })).toBe(false);
    expect(canPurge({ status: "ok" })).toBe(false);
  });
  it("ok + flag missing + server-sent workspace => true; raw sessionStorage is never read", () => {
    expect(canPurge({ status: "ok", user: {}, workspace: { tenantSlug: "acme" } })).toBe(true);
    window.sessionStorage.setItem("nexus_workspace", JSON.stringify({ session: { tenantSlug: "acme" } }));
    expect(canPurge({ status: "ok", user: {} })).toBe(false);
    window.sessionStorage.clear();
  });
});

describe("purgeSummary", () => {
  const labels = { entity: (n: string) => (n === "crm_deals" ? "Cơ hội" : n), field: (_e: string, f: string) => (f === "account_id" ? "Khách hàng" : f) };
  it("counts records, children in the bin, attachments (sys_files), and names what gets emptied", () => {
    const s = purgeSummary(REPORT, labels);
    expect(s.headline).toBe("Sẽ xoá hẳn 2 bản ghi (kể cả 1 bản ghi con trong thùng), 3 tệp đính kèm; không khôi phục được.");
    expect(s.cleared).toEqual(['2 bản ghi Cơ hội khác sẽ bị bỏ trống liên kết "Khách hàng".']);
    expect(s.blobsNote).toBe("3 tệp đính kèm sẽ bị xoá khỏi kho lưu trữ");
    expect(JSON.stringify(s)).not.toMatch(/obj_|sys_files|ở \d+ bảng/);
  });
  it("a lone record with no files and nothing emptied", () => {
    const s = purgeSummary({ ...REPORT, records: [REPORT.records[0]], cleared: [], tables: [], blobsNote: undefined }, labels);
    expect(s.headline).toBe("Sẽ xoá hẳn 1 bản ghi; không khôi phục được.");
    expect(s.cleared).toEqual([]);
    expect(s.blobsNote).toBeUndefined();
  });
  it("blocked => one line per blocker, in labels", () => {
    const s = purgeSummary({ ...REPORT, blocked: [{ entity: "crm_deals", field: "account_id", count: 4 }] }, labels);
    expect(s.blocked).toEqual(["4 bản ghi Cơ hội còn đang dùng bản ghi này."]);
  });
});

describe("purgeRefusal", () => {
  const labels = { entity: (n: string) => (n === "crm_deals" ? "Cơ hội" : n), field: (_e: string, f: string) => f };
  const err = (status: number, code: string, details = {}) => new ApiError(status, code, "m", details);
  it("each code has its own sentence", () => {
    expect(purgeRefusal(err(422, "RECORD_PURGE_TOO_LARGE", { max: 500 }), labels)).toMatchObject({ text: "Quá 500 bản ghi cho một lần; không xoá gì cả." });
    expect(purgeRefusal(err(503, "RECORD_WRITE_CONFLICT"), labels).text).toBe("Hệ thống đang bận, bấm lại.");
    expect(purgeRefusal(err(409, "RECORD_NOT_IN_RECYCLE_BIN"), labels).text).toMatch(/không còn trong thùng rác/);
    expect(purgeRefusal(err(409, "RECORD_PURGE_LIVE_CHILDREN", { blockers: [{ entity: "crm_deals", field: "f", count: 2 }] }), labels).text).toBe("2 bản ghi Cơ hội còn đang dùng bản ghi này.");
    expect(purgeRefusal(err(403, "FORBIDDEN"), labels)).toMatchObject({ text: "Việc này cần quyền người dựng Nexus.", hide: true });
    expect(purgeRefusal(err(404, "RECORD_NOT_FOUND"), labels)).toMatchObject({ gone: true });
  });
  it("an unrouted 404 (operator tier not wired) hides the button", () => {
    expect(purgeRefusal(err(404, ""), labels)).toMatchObject({ hide: true });
  });
});
