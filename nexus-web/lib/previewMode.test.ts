// specs/063-draft-publish T041: the draft-preview header. In /studio only GETs carry it; outside
// /studio nothing does; and EVERY fetch in lib/api.ts (except /auth/*) goes through previewHeaders —
// pinned on the SOURCE, because a raw fetch that forgot it would read the live version while the rest
// of the screen shows the draft.
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { clearWorkspace, setWorkspace } from "./workspace";
import {
  DRAFT_HEADER, DRAFT_PREVIEW_KEY, clearDraftPreview, draftPreviewReturnTo, inDraftPreviewTab, inStudio, onDraftScreen,
  previewHeaders, useDraftPreviewTab, usePreviewReadOnly,
} from "./previewMode";
import { clearPreview, setPreview } from "./previewSession";

function at(path: string) {
  window.history.pushState({}, "", path);
}

describe("previewHeaders", () => {
  afterEach(() => at("/"));

  it("marks GETs inside /studio", () => {
    at("/studio/apps/crm");
    expect(inStudio()).toBe(true);
    expect(previewHeaders("GET")).toEqual({ [DRAFT_HEADER]: "1" });
    expect(previewHeaders("get")).toEqual({ [DRAFT_HEADER]: "1" });
  });

  it("never marks a write, even inside /studio (Studio writes real records on the Data tab)", () => {
    at("/studio/apps/crm");
    for (const m of ["POST", "PUT", "PATCH", "DELETE"]) expect(previewHeaders(m)).toEqual({});
  });

  it("a stale tab flag does not tag Studio writes (Back button into /studio)", () => {
    window.sessionStorage.setItem(DRAFT_PREVIEW_KEY, JSON.stringify({ returnTo: "/studio" }));
    at("/studio/apps/crm");
    for (const m of ["POST", "PUT", "PATCH", "DELETE"]) expect(previewHeaders(m), m).toEqual({});
    at("/home");
    expect(previewHeaders("PUT")).toEqual({ [DRAFT_HEADER]: "1" });
    window.sessionStorage.clear();
  });

  it("marks nothing outside /studio", () => {
    for (const path of ["/", "/home", "/crm_accounts", "/studiox", "/admin/studio"]) {
      at(path);
      expect(previewHeaders("GET"), path).toEqual({});
    }
  });
});

// specs/063 S7-2 T061: a customer-app screen asks for the draft with its OWN ?draft=1 — GET only, so the
// save of an editing panel on that screen never carries the header (S7-3 would answer it 403).
describe("previewHeaders on a ?draft=1 screen", () => {
  afterEach(() => at("/"));

  it("marks GETs of a screen opened with ?draft=1", () => {
    at("/reports/r1?draft=1");
    expect(inStudio()).toBe(false);
    expect(onDraftScreen()).toBe(true);
    expect(previewHeaders("GET")).toEqual({ [DRAFT_HEADER]: "1" });
  });

  it("never marks a write on that screen (the editing panel's save is a plain write)", () => {
    at("/dashboards/d1?config=1&draft=1");
    for (const m of ["POST", "PUT", "PATCH", "DELETE"]) expect(previewHeaders(m), m).toEqual({});
  });

  it("is per screen: the same screen without the flag, or draft=0, is live", () => {
    for (const path of ["/reports/r1", "/reports/r1?draft=0", "/reports/r1?draftx=1", "/reports/r1?config=1"]) {
      at(path);
      expect(onDraftScreen(), path).toBe(false);
      expect(previewHeaders("GET"), path).toEqual({});
    }
  });
});

describe("lib/api.ts goes through previewHeaders", () => {
  it("every fetch( outside /auth/* is paired with previewHeaders(", () => {
    const src = readFileSync(join(process.cwd(), "lib/api.ts"), "utf8");
    const calls = [...src.matchAll(/fetch\(`\$\{BASE\}([^`]*)`/g)];
    expect(calls.length).toBeGreaterThanOrEqual(7);
    const unmarked: string[] = [];
    for (const m of calls) {
      if (m[1].startsWith("/auth/")) continue;
      // The call itself, plus the few lines above it where req()/bulk build their headers object.
      const idx = m.index ?? 0;
      let start = idx;
      for (let i = 0; i < 6; i++) start = src.lastIndexOf("\n", start - 1);
      const end = src.indexOf("\n  });", idx);
      const before = src.slice(Math.max(0, start), idx);
      const after = src.slice(idx, end);
      if (!/previewHeaders\(/.test(before + after)) unmarked.push(m[1]);
    }
    expect(unmarked, `fetch calls without previewHeaders: ${unmarked.join(", ")}`).toEqual([]);
  });
});

describe("req() sends the header from /studio", () => {
  const calls: { url: string; headers: Record<string, string> }[] = [];
  beforeEach(() => {
    calls.length = 0;
    vi.stubGlobal("fetch", vi.fn(async (url: string, init: { headers: Record<string, string> }) => {
      calls.push({ url, headers: init?.headers ?? {} });
      return new Response(JSON.stringify({ data: {} }), { status: 200 });
    }));
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    at("/");
  });

  it("GET carries X-Nexus-Draft in /studio, a PUT does not, and nothing does elsewhere", async () => {
    const { getWorkflow, updateWorkflow } = await import("./api");
    at("/studio/apps/crm");
    await getWorkflow("w1").catch(() => undefined);
    await updateWorkflow("w1", {}).catch(() => undefined);
    at("/home");
    await getWorkflow("w1").catch(() => undefined);
    expect(calls[0].headers[DRAFT_HEADER]).toBe("1");
    expect(calls[1].headers[DRAFT_HEADER]).toBeUndefined();
    expect(calls[2].headers[DRAFT_HEADER]).toBeUndefined();
  });

  // specs/063 S7-4 T084: the app switcher (AppLauncher) and the sidebar menu (GlobalNavigation) read
  // GET /apps and GET /apps/:name/nav through these two functions — pinning the header here is what
  // makes the draft menu show in the preview tab without touching either component.
  it("the app catalog and the app menu carry the header in a draft-preview tab, and never for a plain tab", async () => {
    const { appsCatalog, appNav } = await import("./api");
    at("/home?app=crm&preview=draft");
    await appsCatalog().catch(() => undefined);
    await appNav("crm").catch(() => undefined);
    expect(calls.map((c) => [c.url.replace(/^.*\/api\/v1/, ""), c.headers[DRAFT_HEADER]])).toEqual([["/apps", "1"], ["/apps/crm/nav", "1"]]);

    calls.length = 0;
    clearDraftPreview();
    at("/home?app=crm");
    await appsCatalog().catch(() => undefined);
    await appNav("crm").catch(() => undefined);
    expect(calls.map((c) => c.headers[DRAFT_HEADER])).toEqual([undefined, undefined]);
  });
});


// specs/063 S7-3 T072: "Xem thử bản nháp" — the flag belongs to THIS tab and every request of it is marked.
describe("draft-preview tab (?preview=draft)", () => {
  beforeEach(() => {
    window.sessionStorage.clear();
    window.localStorage.clear();
  });
  afterEach(() => {
    clearDraftPreview();
    clearPreview();
    at("/");
  });

  it("?preview=draft sets the tab flag (sessionStorage only) and marks EVERY method, outside /studio too", () => {
    at("/home?app=crm&preview=draft");
    expect(inDraftPreviewTab()).toBe(true);
    expect(window.sessionStorage.getItem(DRAFT_PREVIEW_KEY)).not.toBeNull();
    expect(window.localStorage.getItem(DRAFT_PREVIEW_KEY)).toBeNull(); // never the shared storage
    expect(window.location.search).toBe("?app=crm"); // the params are consumed, the rest of the query stays
    for (const m of ["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"]) {
      expect(previewHeaders(m), m).toEqual({ [DRAFT_HEADER]: "1" });
    }
  });

  it("the flag outlives the query string (the tab navigates on), and a plain tab never has it", () => {
    at("/home?preview=draft");
    expect(inDraftPreviewTab()).toBe(true);
    at("/crm_accounts/42");
    expect(inDraftPreviewTab()).toBe(true);
    expect(previewHeaders("POST")).toEqual({ [DRAFT_HEADER]: "1" });
    clearDraftPreview();
    expect(inDraftPreviewTab()).toBe(false);
    expect(previewHeaders("POST")).toEqual({});
  });

  it("?preview=other does nothing", () => {
    at("/home?preview=role");
    expect(inDraftPreviewTab()).toBe(false);
  });

  it("return= is honoured only as a same-origin absolute path (no open redirect)", () => {
    for (const [ret, want] of [
      ["/studio/apps/crm", "/studio/apps/crm"],
      ["https://evil.example/x", "/studio"],
      ["//evil.example/x", "/studio"],
      ["/\\evil.example", "/studio"],
      ["/\t/evil.example", "/studio"],
      ["/\n/evil.example", "/studio"],
      ["/\r/evil.example", "/studio"],
    ] as const) {
      window.sessionStorage.clear();
      at(`/home?preview=draft&return=${encodeURIComponent(ret)}`);
      expect(inDraftPreviewTab()).toBe(true);
      expect(draftPreviewReturnTo(), ret).toBe(want);
    }
  });

  it("usePreviewReadOnly is true in that tab (and in a role preview), false elsewhere; follows clear", () => {
    const { result } = renderHook(() => usePreviewReadOnly());
    expect(result.current).toBe(false);
    act(() => at("/home?preview=draft"));
    const draft = renderHook(() => useDraftPreviewTab());
    expect(draft.result.current).toBe(true);
    expect(renderHook(() => usePreviewReadOnly()).result.current).toBe(true);
    act(() => clearDraftPreview());
    expect(draft.result.current).toBe(false);
    setPreview({ token: "T", sessionId: "s", role: { id: "r", name: "n" }, expiresAt: new Date(Date.now() + 60_000).toISOString(), returnTo: "/studio" });
    expect(renderHook(() => usePreviewReadOnly()).result.current).toBe(true);
  });

  it("the next person at this tab does not inherit it: opening or closing a workspace session, a sign-out and a sign-in clear it", async () => {
    const api = await import("./api");
    at("/home?preview=draft");
    expect(inDraftPreviewTab()).toBe(true);
    clearWorkspace(); // close / leave / sign-in (login() clears first) / void state
    expect(inDraftPreviewTab()).toBe(false);

    at("/home?preview=draft");
    expect(inDraftPreviewTab()).toBe(true);
    vi.stubGlobal("fetch", vi.fn(async () => new Response("{}", { status: 200 })));
    await api.logout();
    vi.unstubAllGlobals();
    at("/");
    expect(inDraftPreviewTab()).toBe(false);
  });

  it("a workspace token renewal (setWorkspace) keeps the flag", () => {
    const ws = { accessToken: "A", expiresAt: "2099-01-01T00:00:00Z", homeUserId: "u", session: { id: "s", tenantId: "t", tenantSlug: "acme", tenantName: "Acme", openedAt: "2026-10-09T00:00:00Z", expiresAt: "2099-01-01T00:00:00Z" } };
    at("/home?preview=draft");
    expect(inDraftPreviewTab()).toBe(true);
    setWorkspace(ws);
    setWorkspace({ ...ws, accessToken: "B" });
    expect(inDraftPreviewTab()).toBe(true);
    clearWorkspace();
  });
});
