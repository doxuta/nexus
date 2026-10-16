// specs/073 kh1a T167 + T172: the connection layer between the Studio frame and the browser session.
import { renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const getMe = vi.fn();
vi.mock("@/lib/api", () => ({ getMe: (...a: unknown[]) => getMe(...a) }));

import {
  HOME_SLUG_FALLBACK,
  activeProjectSlug,
  homeSlug,
  onIdentityStorage,
  resetStudioContextForTests,
  sessionReadable,
  useReloadOnIdentityStorage,
  useStudioIdentity,
  workspaceOf,
} from "@/lib/studioContext";

const wrapper = ({ children }: { children: ReactNode }) =>
  createElement(QueryClientProvider, { client: new QueryClient({ defaultOptions: { queries: { retry: false } } }) }, children);

function blockSessionStorage() {
  // Same trap as a real browser with storage blocked: getItem throws on this one store.
  vi.spyOn(window, "sessionStorage", "get").mockReturnValue({
    ...window.sessionStorage,
    getItem: () => {
      throw new Error("blocked");
    },
    setItem: () => {},
    removeItem: () => {},
  } as unknown as Storage);
}

function setSession(slug: string | null) {
  if (slug === null) window.sessionStorage.removeItem("nexus_workspace");
  else
    window.sessionStorage.setItem(
      "nexus_workspace",
      JSON.stringify({ accessToken: "t", expiresAt: "x", homeUserId: "u", session: { id: "s", tenantId: "i", tenantSlug: slug, tenantName: "Acme", openedAt: "", expiresAt: "" } }),
    );
}

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetStudioContextForTests();
  getMe.mockReset();
});
afterEach(() => vi.restoreAllMocks());

describe("homeSlug (T167)", () => {
  it("is nexus_tenant, lower-cased", () => {
    window.localStorage.setItem("nexus_tenant", "Northwind-Lab");
    expect(homeSlug()).toBe("northwind-lab");
  });

  it("falls back to the constant when empty or missing", () => {
    expect(homeSlug()).toBe(HOME_SLUG_FALLBACK);
    resetStudioContextForTests();
    window.localStorage.setItem("nexus_tenant", "");
    expect(homeSlug()).toBe("northwind");
  });

  it("falls back when localStorage throws, and never returns an empty string", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    expect(homeSlug()).toBe("northwind");
    expect(homeSlug()).not.toBe("");
  });

  it("is read once: a later change of nexus_tenant does not move it until reload (FR-026)", () => {
    window.localStorage.setItem("nexus_tenant", "first");
    expect(homeSlug()).toBe("first");
    window.localStorage.setItem("nexus_tenant", "second");
    expect(homeSlug()).toBe("first");
  });
});

describe("session reading", () => {
  it("workspaceOf is null without a session and carries the slug with one", () => {
    expect(workspaceOf()).toBeNull();
    setSession("acme");
    expect(workspaceOf()).toMatchObject({ tenantSlug: "acme", tenantName: "Acme" });
  });

  it("activeProjectSlug is the session slug when there is one, else the home slug", () => {
    window.localStorage.setItem("nexus_tenant", "northwind");
    expect(activeProjectSlug()).toBe("northwind");
    setSession("acme");
    expect(activeProjectSlug()).toBe("acme");
  });

  it("sessionReadable is false when sessionStorage throws (workspaceOf alone would read that as 'no session')", () => {
    expect(sessionReadable()).toBe(true);
    blockSessionStorage();
    expect(sessionReadable()).toBe(false);
    expect(workspaceOf()).toBeNull();
  });
});

describe("useStudioIdentity (T172) — the server decides, closed when unsure", () => {
  it("loading and error never match", async () => {
    getMe.mockReturnValue(new Promise(() => {}));
    const { result } = renderHook(() => useStudioIdentity(), { wrapper });
    expect(result.current.status).toBe("loading");
    expect(result.current.matches("northwind")).toBe(false);

    getMe.mockReset();
    getMe.mockRejectedValue(new Error("500"));
    const err = renderHook(() => useStudioIdentity(), { wrapper });
    await waitFor(() => expect(err.result.current.status).toBe("error"));
    expect(err.result.current.matches("northwind")).toBe(false);
  });

  it("operator at home matches the home project only", async () => {
    window.localStorage.setItem("nexus_tenant", "northwind");
    getMe.mockResolvedValue({ isPlatformOperator: true });
    const { result } = renderHook(() => useStudioIdentity(), { wrapper });
    await waitFor(() => expect(result.current.status).toBe("ready"));
    expect(result.current.matches("northwind")).toBe(true);
    expect(result.current.matches("acme")).toBe(false);
  });

  it("a session token matches its own project only, and not the home project", async () => {
    getMe.mockResolvedValue({ isPlatformOperator: true, workspace: { tenantSlug: "acme" } });
    const { result } = renderHook(() => useStudioIdentity(), { wrapper });
    await waitFor(() => expect(result.current.status).toBe("ready"));
    expect(result.current.matches("acme")).toBe(true);
    expect(result.current.matches("northwind")).toBe(false);
  });

  it("sessionStorage throwing while the server reports a workspace closes the gate", async () => {
    getMe.mockResolvedValue({ isPlatformOperator: true, workspace: { tenantSlug: "acme" } });
    blockSessionStorage();
    const { result } = renderHook(() => useStudioIdentity(), { wrapper });
    await waitFor(() => expect(result.current.status).toBe("ready"));
    expect(result.current.sessionReadable).toBe(false);
    expect(result.current.matches("acme")).toBe(false);
    expect(result.current.matches("northwind")).toBe(false);
  });

  it("localStorage throwing falls back to northwind, but only an operator confirmed by the server may use it", async () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation((k: string) => {
      if (k === "nexus_tenant") throw new Error("blocked");
      return null;
    });
    getMe.mockResolvedValue({});
    const a = renderHook(() => useStudioIdentity(), { wrapper });
    await waitFor(() => expect(a.result.current.status).toBe("ready"));
    expect(a.result.current.matches("northwind")).toBe(false);

    getMe.mockResolvedValue({ isPlatformOperator: true });
    const b = renderHook(() => useStudioIdentity(), { wrapper });
    await waitFor(() => expect(b.result.current.status).toBe("ready"));
    expect(b.result.current.matches("northwind")).toBe(true);
  });

  it("does not call the server when disabled (invalid slug upstream)", () => {
    renderHook(() => useStudioIdentity({ enabled: false }), { wrapper });
    expect(getMe).not.toHaveBeenCalled();
  });
});

const jwt = (sub: string) => `h.${btoa(JSON.stringify({ sub })).replace(/=/g, "")}.s`;
const ev = (key: string, oldValue: string | null, newValue: string | null) => ({ key, oldValue, newValue }) as StorageEvent;

describe("onIdentityStorage (FR-026)", () => {
  it("another tenant signed in from another tab reloads", () => {
    const reload = vi.fn();
    onIdentityStorage(ev("nexus_tenant", "northwind", "acme"), reload);
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it("a token refresh of the same user does NOT reload (silent refresh rewrites nexus_token)", () => {
    const reload = vi.fn();
    onIdentityStorage(ev("nexus_token", jwt("u1"), jwt("u1")), reload);
    expect(reload).not.toHaveBeenCalled();
  });

  it("a token cleared, or a token of another user, reloads", () => {
    const reload = vi.fn();
    onIdentityStorage(ev("nexus_token", jwt("u1"), null), reload);
    onIdentityStorage(ev("nexus_token", jwt("u1"), jwt("u2")), reload);
    expect(reload).toHaveBeenCalledTimes(2);
  });

  it("other keys do nothing", () => {
    const reload = vi.fn();
    onIdentityStorage(ev("nexus.prefs.v1", null, "x"), reload);
    expect(reload).not.toHaveBeenCalled();
  });
});

describe("useReloadOnIdentityStorage", () => {
  it("adds a storage listener on mount and removes it on unmount", () => {
    const add = vi.spyOn(window, "addEventListener");
    const remove = vi.spyOn(window, "removeEventListener");
    const { unmount } = renderHook(() => useReloadOnIdentityStorage());
    expect(add).toHaveBeenCalledWith("storage", expect.any(Function));
    unmount();
    expect(remove).toHaveBeenCalledWith("storage", expect.any(Function));
    add.mockRestore();
    remove.mockRestore();
  });
});
