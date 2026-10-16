import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, listPackInstalls } from "./api";
import { isEngineMissing } from "./engineGap";

describe("isEngineMissing", () => {
  it.each<[string, unknown, boolean]>([
    ["bare gin 404", new ApiError(404, "UNKNOWN", "x"), true],
    ["named 404 VERSION_NOT_FOUND", new ApiError(404, "VERSION_NOT_FOUND", "x"), false],
    ["named 404 TENANT_NOT_FOUND", new ApiError(404, "TENANT_NOT_FOUND", "x"), false],
    ["500 UNKNOWN", new ApiError(500, "UNKNOWN", "x"), false],
    ["403", new ApiError(403, "FORBIDDEN", "x"), false],
    ["plain Error", new Error("net"), false],
    ["undefined", undefined, false],
    ["string", "404", false],
  ])("%s", (_n, e, want) => expect(isEngineMissing(e)).toBe(want));
});

describe("bare 404 reaches isEngineMissing through req()", () => {
  afterEach(() => vi.unstubAllGlobals());
  it("404 text body `404 page not found` => ApiError code UNKNOWN", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("404 page not found", { status: 404 })));
    const err = await listPackInstalls().catch((e) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).code).toBe("UNKNOWN");
    expect(isEngineMissing(err)).toBe(true);
  });
});
