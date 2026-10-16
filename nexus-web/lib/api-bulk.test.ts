// Tests for lib/api.ts's createRecordsBulk — the one client function that must NOT follow
// req<T>()'s generic "!res.ok → throw" rule, because mode=atomic legitimately returns HTTP 422
// while still carrying a full {data:{results:[...]}} envelope (../specs/009-bulk-create/spec.md §2). The
// discriminator is BODY SHAPE (`data.results` array present), not HTTP status.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { createRecordsBulk, ApiError } from "@/lib/api";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

describe("createRecordsBulk", () => {
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

  it("atomic all-ok (201) resolves as a BulkResult", async () => {
    global.fetch = vi.fn(async () => jsonRes(
      {
        data: {
          mode: "atomic", created: 2, failed: 0,
          results: [
            { index: 0, status: "created", id: "a" },
            { index: 1, status: "created", id: "b" },
          ],
        },
      },
      201,
    )) as unknown as typeof fetch;

    const res = await createRecordsBulk("accounts", [{ name: "A" }, { name: "B" }], "atomic");
    expect(res.mode).toBe("atomic");
    expect(res.created).toBe(2);
    expect(res.results).toHaveLength(2);
  });

  it("atomic abort (422) resolves as a BulkResult — NOT a thrown ApiError — per-row results survive", async () => {
    global.fetch = vi.fn(async () => jsonRes(
      {
        data: {
          mode: "atomic", created: 0, failed: 1, skipped: 1,
          results: [
            { index: 0, status: "failed", code: "RECORD_REQUIRED_MISSING", error: "Thiếu trường", details: { field: "name" } },
            { index: 1, status: "skipped", code: "BULK_ATOMIC_ABORTED", error: "Bị huỷ theo dòng khác" },
          ],
        },
      },
      422,
    )) as unknown as typeof fetch;

    const res = await createRecordsBulk("accounts", [{}, { name: "B" }], "atomic");
    expect(res.created).toBe(0);
    expect(res.results[0]).toMatchObject({ index: 0, status: "failed", code: "RECORD_REQUIRED_MISSING" });
    expect(res.results[1]).toMatchObject({ index: 1, status: "skipped", code: "BULK_ATOMIC_ABORTED" });
  });

  it("partial mixed (200) resolves as a BulkResult with created+failed rows side by side", async () => {
    global.fetch = vi.fn(async () => jsonRes(
      {
        data: {
          mode: "partial", created: 1, failed: 1,
          results: [
            { index: 0, status: "created", id: "a" },
            { index: 1, status: "failed", code: "RECORD_DUPLICATE" },
          ],
        },
      },
      200,
    )) as unknown as typeof fetch;

    const res = await createRecordsBulk("accounts", [{ name: "A" }, { name: "A" }], "partial");
    expect(res.created).toBe(1);
    expect(res.failed).toBe(1);
  });

  it("BULK_EMPTY (422, generic {error,code} — no `data`) throws ApiError", async () => {
    global.fetch = vi.fn(async () => jsonRes(
      { error: "Danh sách rỗng", code: "BULK_EMPTY" },
      422,
    )) as unknown as typeof fetch;

    await expect(createRecordsBulk("accounts", [], "atomic")).rejects.toMatchObject({
      status: 422,
      code: "BULK_EMPTY",
    });
  });

  it("infra 500 (generic {error,code} — no `data`) throws ApiError, not swallowed as a BulkResult", async () => {
    global.fetch = vi.fn(async () => jsonRes(
      { error: "Lỗi hệ thống", code: "RECORD_OP_FAILED" },
      500,
    )) as unknown as typeof fetch;

    const err = await createRecordsBulk("accounts", [{ name: "A" }], "partial").catch((e) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(500);
  });
});
