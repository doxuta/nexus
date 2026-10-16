// HIDDEN-COL-SIGNAL — chip mapping + envelope passthrough.
//
// 1) aclHiddenChipLabel: the pure gate behind LayoutList's "N cột ẩn bởi quyền" chip — must
//    render ONLY on a positive finite count (absent/0/garbage → null → no chip). The server
//    already omits `meta` for admins; the null cases here are the FE's belt-and-suspenders.
// 2) listRecordsPage: the additive `meta.hiddenColumns` (api-conventions §14) must pass through
//    verbatim when present and stay undefined when the envelope has no meta (old servers /
//    nothing hidden) — data/pagination parsing unchanged.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { aclHiddenChipLabel } from "@/lib/hiddenColumns";
import { listRecordsPage } from "@/lib/api";

describe("aclHiddenChipLabel", () => {
  it("renders the honest label for a positive count", () => {
    expect(aclHiddenChipLabel(2)).toBe("2 cột ẩn bởi quyền");
    expect(aclHiddenChipLabel(1)).toBe("1 cột ẩn bởi quyền");
  });
  it("floors a fractional count (defensive against odd payloads)", () => {
    expect(aclHiddenChipLabel(2.9)).toBe("2 cột ẩn bởi quyền");
  });
  it.each([
    ["zero", 0],
    ["negative", -3],
    ["undefined", undefined],
    ["null", null],
    ["NaN", Number.NaN],
    ["Infinity", Number.POSITIVE_INFINITY],
  ] as const)("returns null (no chip) for %s", (_label, v) => {
    expect(aclHiddenChipLabel(v as number | undefined | null)).toBeNull();
  });
});

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

describe("listRecordsPage — meta.hiddenColumns passthrough (§14)", () => {
  let originalFetch: FetchFn;

  beforeEach(() => {
    originalFetch = global.fetch as unknown as FetchFn;
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("passes meta.hiddenColumns through when the envelope carries it", async () => {
    global.fetch = vi.fn(async () => jsonRes({
      data: [{ id: "r1", data: { name: "Acme" }, version: 1 }],
      pagination: { page: 1, limit: 20, total: 1, totalPages: 1 },
      meta: { hiddenColumns: 2 },
    })) as unknown as typeof fetch;

    const page = await listRecordsPage("contacts", "page=1&limit=20");
    expect(page.meta?.hiddenColumns).toBe(2);
    expect(page.records).toHaveLength(1);
    expect(page.pagination.total).toBe(1); // pagination parsing unchanged
  });

  it("leaves meta undefined when the envelope has none (admin / nothing hidden / old server)", async () => {
    global.fetch = vi.fn(async () => jsonRes({
      data: [],
      pagination: { page: 1, limit: 20, total: 0, totalPages: 0 },
    })) as unknown as typeof fetch;

    const page = await listRecordsPage("contacts");
    expect(page.meta).toBeUndefined();
    expect(aclHiddenChipLabel(page.meta?.hiddenColumns)).toBeNull(); // → no chip
  });
});
