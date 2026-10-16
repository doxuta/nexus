import { afterEach, describe, expect, it, vi } from "vitest";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { DRAFT_HEADER } from "@/lib/previewMode";
import { updateLayout } from "@/lib/api";
import { readForBuilder } from "./readForBuilder";

const seen: Array<{ method: string; headers: Record<string, string> }> = [];
function stubFetch() {
  seen.length = 0;
  vi.stubGlobal("fetch", vi.fn(async (_url: string, init?: RequestInit) => {
    seen.push({ method: init?.method ?? "GET", headers: (init?.headers ?? {}) as Record<string, string> });
    return new Response(JSON.stringify({ data: [] }), { status: 200, headers: { "Content-Type": "application/json" } });
  }));
}
afterEach(() => vi.unstubAllGlobals());

describe("readForBuilder (specs/075 T190)", () => {
  it("delivered: every read is a GET carrying the draft header", async () => {
    stubFetch();
    await Promise.all([
      readForBuilder.getLayout(true, "t"), readForBuilder.listFields(true, "t"),
      readForBuilder.listEntities(true), readForBuilder.listAllRelationships(true),
    ]);
    expect(seen).toHaveLength(4);
    for (const s of seen) {
      expect(s.method).toBe("GET");
      expect(s.headers[DRAFT_HEADER]).toBe("1");
    }
  });

  it("live=true on a delivered project, even inside /studio: no draft header (T198), writes still untagged", async () => {
    stubFetch();
    window.history.pushState({}, "", "/studio/x");
    await readForBuilder.getLayout(true, "t", true);
    await readForBuilder.listFields(true, "t", true);
    await readForBuilder.getLayout(true, "t"); // the draft read right after is back to tagged
    window.history.pushState({}, "", "/");
    expect(seen[0].headers[DRAFT_HEADER]).toBeUndefined();
    expect(seen[1].headers[DRAFT_HEADER]).toBeUndefined();
    expect(seen[2].headers[DRAFT_HEADER]).toBe("1");
  });

  it("not delivered: no header", async () => {
    stubFetch();
    await readForBuilder.listFields(false, "t");
    await readForBuilder.listEntities(false);
    expect(seen).toHaveLength(2);
    expect(seen.every((s) => s.headers[DRAFT_HEADER] === undefined)).toBe(true);
  });

  it("the flag does not leak: a write right after a draft read carries no header", async () => {
    stubFetch();
    await readForBuilder.getLayout(true, "t");
    await updateLayout("L1", {});
    const put = seen.find((s) => s.method === "PUT");
    expect(put).toBeDefined();
    expect(put!.headers[DRAFT_HEADER]).toBeUndefined();
  });

  // Pin: no metadata read of the two editors may bypass the slot.
  it("components/studio/screen and relmap never call the four reads directly", () => {
    const bad: string[] = [];
    for (const dir of ["components/studio/screen", "components/studio/relmap"]) {
      for (const f of readdirSync(dir)) {
        if (!/\.(ts|tsx)$/.test(f) || /\.test\./.test(f)) continue;
        const src = readFileSync(join(dir, f), "utf8");
        if (/(?<![.\w])(getLayout|listFields|listEntities|listAllRelationships)\(/.test(src)) bad.push(`${dir}/${f}`);
      }
    }
    expect(bad).toEqual([]);
  });
});
