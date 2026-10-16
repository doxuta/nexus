// Tests for lib/api.ts importRecords — IMPORT-DEDUP-FE wiring: the optional dedupField form
// param the BE reads at import_handler.go:114, and the response envelope's real field names
// ({total, inserted, failed, errors[{row,field,code,message}]} — `inserted`, not `created`).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { importRecords } from "@/lib/api";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

describe("importRecords", () => {
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

  it("appends dedupField to the multipart body when provided", async () => {
    let capturedUrl = "";
    let captured: FormData | null = null;
    global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
      capturedUrl = String(url);
      captured = init?.body as FormData;
      return jsonRes({
        data: {
          total: 3, inserted: 2, failed: 1,
          errors: [{ row: 2, field: "email", code: "RECORD_DUPLICATE", message: 'giá trị "a@b.c" đã tồn tại' }],
        },
      }, 200);
    }) as unknown as typeof fetch;

    const res = await importRecords(
      "accounts",
      new File(["email\na@b.c\n"], "a.csv", { type: "text/csv" }),
      { email: "email" },
      { dedupField: "email" },
    );

    expect(capturedUrl).toMatch(/\/accounts\/import$/);
    expect(captured!.get("dedupField")).toBe("email");
    expect(captured!.get("mapping")).toBe(JSON.stringify({ email: "email" }));
    // Envelope maps 1:1 — `inserted` (BE json tag), errors carry `code`.
    expect(res.inserted).toBe(2);
    expect(res.failed).toBe(1);
    expect(res.errors?.[0]).toMatchObject({ row: 2, code: "RECORD_DUPLICATE" });
  });

  it("omits dedupField from the body entirely when not selected", async () => {
    let captured: FormData | null = null;
    global.fetch = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      captured = init?.body as FormData;
      return jsonRes({ data: { total: 1, inserted: 1, failed: 0, errors: [] } }, 200);
    }) as unknown as typeof fetch;

    await importRecords("accounts", new File(["email\na@b.c\n"], "a.csv", { type: "text/csv" }), { email: "email" });

    expect(captured!.get("dedupField")).toBeNull();
    expect(captured!.get("dryRun")).toBeNull();
    expect(captured!.get("file")).not.toBeNull();
  });

  // A column set to "— Bỏ qua —" is mapped to "" on screen; the server refuses a mapping to a field
  // named "" (RECORD_UNKNOWN_FIELD), so a skipped column never goes on the wire.
  it("leaves skipped columns out of the mapping", async () => {
    let captured: FormData | null = null;
    global.fetch = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      captured = init?.body as FormData;
      return jsonRes({ data: { total: 1, inserted: 1, failed: 0, errors: [] } }, 200);
    }) as unknown as typeof fetch;

    await importRecords("accounts", new File(["email,Ghi chú\na@b.c,x\n"], "a.csv"), { email: "email", "Ghi chú": "" });

    expect(JSON.parse(String(captured!.get("mapping")))).toEqual({ email: "email" });
  });

  // specs/061 FR-010/FR-011: the preview posts dryRun=true and reads back valid/dryRun.
  it("sends dryRun for the preview and returns the valid count", async () => {
    let captured: FormData | null = null;
    global.fetch = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      captured = init?.body as FormData;
      return jsonRes({ data: { total: 20, inserted: 0, failed: 1, valid: 19, dryRun: true, errors: [] } }, 200);
    }) as unknown as typeof fetch;

    const res = await importRecords("accounts", new File(["a\n1\n"], "a.csv"), { a: "a" }, { dryRun: true });

    expect(captured!.get("dryRun")).toBe("true");
    expect(captured!.get("matchBy")).toBeNull(); // no choice ⇒ nothing sent
    expect(res).toMatchObject({ valid: 19, dryRun: true, inserted: 0 });
  });

  // specs/061 FR-012: "Khớp theo" rides as matchBy JSON.
  it("sends matchBy when a relation column has a chosen match field", async () => {
    let captured: FormData | null = null;
    global.fetch = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      captured = init?.body as FormData;
      return jsonRes({ data: { total: 1, inserted: 1, failed: 0, errors: [] } }, 200);
    }) as unknown as typeof fetch;

    await importRecords("deals", new File(["p\nPGR\n"], "d.csv"), { p: "project" }, { matchBy: { project: "ma_du_an" } });

    expect(captured!.get("matchBy")).toBe(JSON.stringify({ project: "ma_du_an" }));
  });
});
