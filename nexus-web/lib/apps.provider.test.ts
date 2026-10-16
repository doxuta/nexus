// ActiveAppProvider — specs/064 FR-014 (found on a real browser, T065): the sidebar of a preview tab comes
// from GET /apps/:name/nav, which the SERVER filters by the previewed role. Fresh grants alone
// (permissions.test.ts) are not enough: the menu must be re-read too, or "Cơ hội" stays after the builder
// removed it from the role.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { focusManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";
import { ActiveAppProvider, useActiveApp } from "./apps";

const auth = vi.hoisted(() => ({ user: null as null | Record<string, unknown> }));
const api = vi.hoisted(() => ({ appsCatalog: vi.fn(), appNav: vi.fn() }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: auth.user }) }));
vi.mock("@/lib/api", async (importOriginal) => ({ ...(await importOriginal<typeof import("@/lib/api")>()), appsCatalog: api.appsCatalog, appNav: api.appNav }));

const APP = { name: "crm", label: "CRM", isDefault: true };
const nav = (...hrefs: string[]) => ({ items: hrefs.map((h) => ({ href: h, label: h })) });

function wrapper() {
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: 120_000, refetchOnWindowFocus: false, retry: false } } });
  const Wrap = ({ children }: { children: ReactNode }) =>
    createElement(QueryClientProvider, { client: qc }, createElement(ActiveAppProvider, null, children));
  return Wrap;
}

async function refocus() {
  await act(async () => {
    focusManager.setFocused(false);
    focusManager.setFocused(true);
  });
}

describe("ActiveAppProvider — nav freshness", () => {
  beforeEach(() => {
    window.localStorage.clear();
    api.appsCatalog.mockReset().mockResolvedValue([APP]);
    api.appNav.mockReset().mockResolvedValue(nav("/crm_accounts", "/crm_opportunities"));
  });

  it("previewing: coming back to the tab re-reads the role's nav", async () => {
    auth.user = { id: "p", preview: { sessionId: "s1" } };
    const { result } = renderHook(() => useActiveApp(), { wrapper: wrapper() });
    await waitFor(() => expect(result.current.activeNav?.map((i) => i.href)).toEqual(["/crm_accounts", "/crm_opportunities"]));

    api.appNav.mockResolvedValue(nav("/crm_accounts")); // the builder removed Cơ hội from the role
    await refocus();
    await waitFor(() => expect(result.current.activeNav?.map((i) => i.href)).toEqual(["/crm_accounts"]));
  });

  it("not previewing: the 5-minute cache stands, no refetch on focus", async () => {
    auth.user = { id: "u1" };
    const { result } = renderHook(() => useActiveApp(), { wrapper: wrapper() });
    await waitFor(() => expect(result.current.activeNav).not.toBeNull());
    const calls = api.appNav.mock.calls.length;
    await refocus();
    expect(api.appNav.mock.calls.length).toBe(calls);
    expect(api.appsCatalog).toHaveBeenCalledTimes(1);
  });
});

// specs/073 FR-032: inside a Studio project `?app=` is the project's own "create into" choice (StudioScopeOverride reads
// it). The shared lens must neither persist it to localStorage nor strip it from the URL.
describe("ActiveAppProvider — ?app= inside a Studio project", () => {
  beforeEach(() => {
    window.localStorage.clear();
    auth.user = { id: "u" };
    api.appsCatalog.mockReset().mockResolvedValue([APP]);
    api.appNav.mockReset().mockResolvedValue(nav("/crm_accounts"));
  });

  it("leaves localStorage and the URL alone, and does not adopt it as the shared lens", async () => {
    api.appsCatalog.mockResolvedValue([APP, { name: "ticket", label: "Ticket", isDefault: false }]);
    window.history.replaceState(null, "", "/studio/du-an/northwind/dung/du-lieu?app=ticket");
    const { result } = renderHook(() => useActiveApp(), { wrapper: wrapper() });
    await waitFor(() => expect(result.current.apps).toHaveLength(2));
    expect(result.current.activeApp).toBe("crm");
    expect(Object.keys(window.localStorage).filter((k) => k.startsWith("nexus.activeApp"))).toEqual([]);
    expect(window.location.search).toBe("?app=ticket");
    window.history.replaceState(null, "", "/");
  });

  it("elsewhere a ?app= deep link is still adopted once and cleaned", () => {
    window.history.replaceState(null, "", "/crm_accounts?app=ticket");
    renderHook(() => useActiveApp(), { wrapper: wrapper() });
    expect(window.location.search).toBe("");
    window.history.replaceState(null, "", "/");
  });
});
