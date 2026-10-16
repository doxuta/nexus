// Tests for lib/api.ts's bulkUpdateRecords / bulkDeleteRecords (ENG-MASSOP client). Two contracts
// under test: (1) NORMALIZATION — the server envelope counts ok rows as `updated` (bulk-update)
// vs `deleted` (bulk-delete); both must fold into the FE-facing `succeeded`; (2) CHUNKING — a
// selection larger than the server cap (500) is split into sequential calls whose envelopes are
// merged, so the caller sees ONE result. Request-level 422s (BULK_TOO_MANY/BULK_EMPTY/…) follow
// req<T>()'s normal throw path — unlike createRecordsBulk there is no non-2xx-with-results case.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { bulkUpdateRecords, bulkDeleteRecords, ApiError } from "@/lib/api";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

describe("bulkUpdateRecords / bulkDeleteRecords", () => {
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

  it("bulk-update: normalizes the server's `updated` count into `succeeded` + keeps per-row results", async () => {
    global.fetch = vi.fn(async () => jsonRes(
      {
        data: {
          results: [
            { id: "a", ok: true },
            { id: "b", ok: false, code: "RECORD_DUPLICATE", details: { field: "email" } },
          ],
          updated: 1,
          failed: 1,
        },
      },
      200,
    )) as unknown as typeof fetch;

    const res = await bulkUpdateRecords("contacts", ["a", "b"], { name: "X" });
    expect(res.succeeded).toBe(1);
    expect(res.failed).toBe(1);
    expect(res.results).toHaveLength(2);
    expect(res.results[1]).toMatchObject({ id: "b", ok: false, code: "RECORD_DUPLICATE" });
  });

  it("bulk-delete: normalizes `deleted` into `succeeded`", async () => {
    global.fetch = vi.fn(async () => jsonRes(
      { data: { results: [{ id: "a", ok: true }, { id: "b", ok: true }], deleted: 2, failed: 0 } },
      200,
    )) as unknown as typeof fetch;

    const res = await bulkDeleteRecords("contacts", ["a", "b"]);
    expect(res.succeeded).toBe(2);
    expect(res.failed).toBe(0);
  });

  it("chunks a 501-id selection into two calls and merges the envelopes", async () => {
    const ids = Array.from({ length: 501 }, (_, i) => `id-${i}`);
    const fetchMock = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      const body = JSON.parse(String(init?.body)) as { ids: string[] };
      return jsonRes(
        {
          data: {
            results: body.ids.map((id) => ({ id, ok: true })),
            deleted: body.ids.length,
            failed: 0,
          },
        },
        200,
      );
    });
    global.fetch = fetchMock as unknown as typeof fetch;

    const res = await bulkDeleteRecords("contacts", ids);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    const firstBody = JSON.parse(String(fetchMock.mock.calls[0][1]?.body)) as { ids: string[] };
    const secondBody = JSON.parse(String(fetchMock.mock.calls[1][1]?.body)) as { ids: string[] };
    expect(firstBody.ids).toHaveLength(500);
    expect(secondBody.ids).toHaveLength(1);
    expect(res.succeeded).toBe(501);
    expect(res.results).toHaveLength(501);
  });

  it("request-level 422 (BULK_VALUES_EMPTY) throws ApiError with the code", async () => {
    global.fetch = vi.fn(async () => jsonRes(
      { error: "values rỗng — cần ít nhất 1 field để cập nhật", code: "BULK_VALUES_EMPTY", details: {} },
      422,
    )) as unknown as typeof fetch;

    await expect(bulkUpdateRecords("contacts", ["a"], {})).rejects.toMatchObject({
      code: "BULK_VALUES_EMPTY",
      status: 422,
    });
    const err = await bulkUpdateRecords("contacts", ["a"], {}).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
  });
});
