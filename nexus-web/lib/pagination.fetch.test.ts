// FE-cursor-bootstrap-a (DAILY-QUEUE #43, specs/039-cursor-bootstrap) — the list's page client.
//
// What the record list must send, per mode (the server's contract, spec §2.1):
//   • cursor: page 1 = `paginate=cursor` + sort/order, NO `page`; the next page adds
//     `after=<nextCursor>` and re-sends the SAME sort/order (a cursor is bound to them — a
//     different sort is 400 INVALID_CURSOR). Never `page` (422 PAGINATION_MODE_CONFLICT).
//   • offset: `page=` + `limit=` exactly as before.
// And what it reads back: cursor pages have no `total` (null — the UI says "50+ mục"), and
// "Tải thêm" is offered while `nextCursor` is non-null; offset keeps page < totalPages.
import { describe, it, expect, vi, beforeEach } from "vitest";

const listRecordsPage = vi.fn();
const listRecordsCursorPage = vi.fn();
vi.mock("@/lib/api", () => ({
  listRecordsPage: (...a: unknown[]) => listRecordsPage(...a),
  listRecordsCursorPage: (...a: unknown[]) => listRecordsCursorPage(...a),
}));

import { buildListPageQuery, fetchListPage } from "./pagination";

const params = (q: string) => new URLSearchParams(q);

describe("buildListPageQuery", () => {
  const base = {
    sort: { field: "created_at", order: "desc" as const },
    expand: ["account"],
    q: "minh",
    filterQuery: "stage%5Beq%5D=won",
    limit: 50,
  };

  it("cursor page 1: paginate=cursor, sort/order, no page, no after", () => {
    const p = params(buildListPageQuery({ ...base, mode: "cursor", next: null }));
    expect(p.get("paginate")).toBe("cursor");
    expect(p.get("sort")).toBe("created_at");
    expect(p.get("order")).toBe("desc");
    expect(p.get("limit")).toBe("50");
    expect(p.get("expand")).toBe("account");
    expect(p.get("q")).toBe("minh");
    expect(p.get("stage[eq]")).toBe("won");
    expect(p.has("page")).toBe(false);
    expect(p.has("after")).toBe(false);
  });

  it("cursor next page: after=<cursor> with the SAME sort/order, still no page", () => {
    const p = params(buildListPageQuery({ ...base, mode: "cursor", next: "djN8Y3Vyc29y" }));
    expect(p.get("after")).toBe("djN8Y3Vyc29y");
    expect(p.get("sort")).toBe("created_at");
    expect(p.get("order")).toBe("desc");
    expect(p.has("page")).toBe(false);
  });

  it("offset: page=N, no paginate/after (byte-compatible with the old list query)", () => {
    const p = params(buildListPageQuery({ ...base, mode: "offset", next: 3 }));
    expect(p.get("page")).toBe("3");
    expect(p.get("limit")).toBe("50");
    expect(p.has("paginate")).toBe(false);
    expect(p.has("after")).toBe(false);
  });

  it("offset with no sort sends no sort/order (server default)", () => {
    const p = params(buildListPageQuery({ ...base, sort: null, mode: "offset", next: null }));
    expect(p.has("sort")).toBe(false);
    expect(p.get("page")).toBe("1");
  });
});

describe("fetchListPage", () => {
  beforeEach(() => {
    listRecordsPage.mockReset();
    listRecordsCursorPage.mockReset();
  });

  it("cursor mode: next = nextCursor, total unknown (null), hidden columns passed through", async () => {
    listRecordsCursorPage.mockResolvedValue({
      records: [{ id: "a" }, { id: "b" }], limit: 2, nextCursor: "c2", meta: { hiddenColumns: 2 },
    });
    const r = await fetchListPage("crm_contacts", "cursor", "paginate=cursor&limit=2");
    expect(listRecordsCursorPage).toHaveBeenCalledWith("crm_contacts", "paginate=cursor&limit=2");
    expect(listRecordsPage).not.toHaveBeenCalled();
    expect(r).toEqual({ records: [{ id: "a" }, { id: "b" }], total: null, next: "c2", hiddenColumns: 2 });
  });

  it("cursor mode, last page: next = null (the Tải thêm gate closes)", async () => {
    listRecordsCursorPage.mockResolvedValue({ records: [{ id: "z" }], limit: 50, nextCursor: null });
    const r = await fetchListPage("crm_contacts", "cursor", "q");
    expect(r.next).toBeNull();
    expect(r.hiddenColumns).toBe(0);
  });

  it("offset mode: next = page+1 while page < totalPages, total from the server", async () => {
    listRecordsPage.mockResolvedValue({
      records: [{ id: "a" }], pagination: { page: 2, limit: 1, total: 3, totalPages: 3 },
    });
    const r = await fetchListPage("crm_accounts", "offset", "page=2");
    expect(r).toEqual({ records: [{ id: "a" }], total: 3, next: 3, hiddenColumns: 0 });
  });

  it("offset mode, last page: next = null", async () => {
    listRecordsPage.mockResolvedValue({
      records: [], pagination: { page: 3, limit: 1, total: 3, totalPages: 3 }, meta: { hiddenColumns: 1 },
    });
    const r = await fetchListPage("crm_accounts", "offset", "page=3");
    expect(r.next).toBeNull();
    expect(r.hiddenColumns).toBe(1);
  });
});
