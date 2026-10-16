// Tests for the FE-CATCHUP-2 BREAKING contract change on GET /admin/verticals/builtin:
// items changed from bare slug strings to { slug, manifest, entityCount } objects (BE:
// handlers.AdminVerticalHandler.ListBuiltin → vertical_pack.BuiltinInfos). listBuiltinVerticals
// must parse the new shape and surface each pack's REAL version — the FE never hardcodes one.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { listBuiltinVerticals, logout } from "@/lib/api";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status, headers: { "Content-Type": "application/json" },
  });
}

describe("listBuiltinVerticals — new manifest shape", () => {
  let originalFetch: FetchFn;

  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout(); // reset lib/api module token cache
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("parses {slug, manifest, entityCount} entries and surfaces the REAL pack version", async () => {
    const fetchMock = vi.fn(async (url: RequestInfo | URL) =>
      !String(url).includes("/admin/verticals/builtin")
        ? jsonRes({ error: "unexpected", code: "UNEXPECTED" }, 500)
        : jsonRes({ data: { builtins: [
        {
          slug: "clinic",
          manifest: { name: "Clinic", slug: "clinic", version: "1.0.0", description: "Bệnh nhân và lịch hẹn khám (phòng khám cơ bản).", author: "Nexus" },
          entityCount: 2,
        },
        {
          slug: "crm",
          manifest: { name: "CRM", slug: "crm", version: "1.1.0", description: "Khách hàng, liên hệ, cơ hội, leads và hoạt động bán hàng (CRM đầy đủ).", author: "Nexus" },
          entityCount: 5,
        },
      ] } }, 200),
    );
    global.fetch = fetchMock as unknown as typeof fetch;

    const out = await listBuiltinVerticals();
    expect(String(fetchMock.mock.calls[0][0])).toContain("/admin/verticals/builtin");
    expect(out).toHaveLength(2);

    const crm = out.find((b) => b.slug === "crm");
    expect(crm).toBeDefined();
    // The deepened CRM's version is visible pre-install — this was the whole point of the change.
    expect(crm!.manifest.version).toBe("1.1.0");
    expect(crm!.manifest.name).toBe("CRM");
    expect(crm!.entityCount).toBe(5);

    const clinic = out.find((b) => b.slug === "clinic");
    expect(clinic!.manifest.version).toBe("1.0.0");
    expect(clinic!.entityCount).toBe(2);
  });

  it("degrades a null/absent manifest to a named slug-only entry (no invented version, no crash)", async () => {
    global.fetch = vi.fn(async () =>
      jsonRes({ data: { builtins: [
        { slug: "mystery", manifest: null, entityCount: 3 },
        { slug: "noCount", manifest: { name: "NoCount", version: "0.1.0" } },
      ] } }, 200),
    ) as unknown as typeof fetch;

    const out = await listBuiltinVerticals();
    expect(out[0]).toEqual({ slug: "mystery", manifest: { name: "mystery", version: "" }, entityCount: 3 });
    expect(out[1].entityCount).toBe(0);
    expect(out[1].manifest.version).toBe("0.1.0");
  });

  it("drops slug-less entries and returns [] for an empty/missing list", async () => {
    global.fetch = vi.fn(async () =>
      jsonRes({ data: { builtins: [{ manifest: { name: "Ghost", version: "1.0.0" } }] } }, 200),
    ) as unknown as typeof fetch;
    await expect(listBuiltinVerticals()).resolves.toEqual([]);

    global.fetch = vi.fn(async () => jsonRes({ data: {} }, 200)) as unknown as typeof fetch;
    await expect(listBuiltinVerticals()).resolves.toEqual([]);
  });
});
