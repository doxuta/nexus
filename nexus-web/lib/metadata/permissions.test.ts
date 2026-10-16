// permissions hook (.ts, not .tsx: vitest.config only globs lib/**/*.test.ts) — specs/064 T060: a tab previewing a role must read the role's grants FRESH.
// The builder edits the role in another tab (Studio) while this one previews it; with the usual
// 10-minute metadata cache the preview would keep showing the old menu until the cache expires.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { focusManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";
import { usePermissionsQuery, usePermissions } from "./permissions";

const auth = vi.hoisted(() => ({ user: null as null | Record<string, unknown> }));
const api = vi.hoisted(() => ({ getMyPermissions: vi.fn() }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: auth.user }) }));
vi.mock("@/lib/api", () => ({ getMyPermissions: api.getMyPermissions }));

const VIEW_ALL = [{ entityName: "crm_opportunities", action: "view", accessLevel: "all" }];

function wrapper() {
  // Same defaults as lib/query/client.tsx: window-focus refetch OFF, 2-minute staleTime.
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: 120_000, refetchOnWindowFocus: false, retry: false } } });
  const Wrap = ({ children }: { children: ReactNode }) => createElement(QueryClientProvider, { client: qc }, children);
  return { qc, Wrap };
}

async function refocus() {
  await act(async () => {
    focusManager.setFocused(false);
    focusManager.setFocused(true);
  });
}

describe("usePermissionsQuery — freshness", () => {
  beforeEach(() => {
    api.getMyPermissions.mockReset();
    api.getMyPermissions.mockResolvedValue(VIEW_ALL);
  });

  it("previewing: staleTime 0 and a focus refetch picks up the role the builder just changed", async () => {
    auth.user = { id: "p", isAdmin: false, preview: { sessionId: "s1" } };
    const { Wrap } = wrapper();
    const { result } = renderHook(() => usePermissions(), { wrapper: Wrap });
    await waitFor(() => expect(result.current.can("view", "crm_opportunities")).toBe(true));
    expect(api.getMyPermissions).toHaveBeenCalledTimes(1);

    // The builder removes "view Cơ hội" from the role in the Studio tab, then comes back to this one.
    api.getMyPermissions.mockResolvedValue([]);
    await refocus();
    await waitFor(() => expect(result.current.can("view", "crm_opportunities")).toBe(false));
    expect(api.getMyPermissions).toHaveBeenCalledTimes(2);
  });

  it("previewing: the query is stale the moment it lands (staleTime 0)", async () => {
    auth.user = { id: "p", isAdmin: false, preview: { sessionId: "s1" } };
    const { qc, Wrap } = wrapper();
    const { result } = renderHook(() => usePermissionsQuery(), { wrapper: Wrap });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(qc.getQueryCache().getAll()[0].isStale()).toBe(true);
  });

  it("not previewing: keeps the 10-minute cache and does not refetch on focus", async () => {
    auth.user = { id: "u1", isAdmin: false };
    const { qc, Wrap } = wrapper();
    const { result } = renderHook(() => usePermissionsQuery(), { wrapper: Wrap });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(qc.getQueryCache().getAll()[0].isStale()).toBe(false);
    await refocus();
    expect(api.getMyPermissions).toHaveBeenCalledTimes(1);
  });
});

// specs/063 S7-3 T073: a draft-preview tab never writes — the write actions are withdrawn at the one
// place every screen asks (list "Mới", detail Sửa/Xoá, form, kanban drag, calendar create).
describe("usePermissions in a draft-preview tab", () => {
  beforeEach(() => {
    api.getMyPermissions.mockReset();
    api.getMyPermissions.mockResolvedValue([{ entityName: "*", action: "*", accessLevel: "all" }]);
    window.sessionStorage.clear();
  });

  it("withdraws create/edit/delete even for an admin and keeps view; an ordinary tab is untouched", async () => {
    auth.user = { id: "b", isAdmin: true };
    const { Wrap } = wrapper();
    const plain = renderHook(() => usePermissions(), { wrapper: Wrap });
    await waitFor(() => expect(plain.result.current.can("edit", "crm_accounts")).toBe(true));

    window.sessionStorage.setItem("nexus_draft_preview", JSON.stringify({ returnTo: "/studio" }));
    const draft = renderHook(() => usePermissions(), { wrapper: Wrap });
    await waitFor(() => expect(draft.result.current.can("view", "crm_accounts")).toBe(true));
    for (const a of ["create", "edit", "delete", "*"] as const) {
      expect(draft.result.current.can(a, "crm_accounts"), a).toBe(false);
    }
  });

  it("a role-preview tab (064) also withdraws writes, on purpose: the server refuses them (PREVIEW_READ_ONLY)", async () => {
    auth.user = { id: "b", isAdmin: true };
    window.sessionStorage.setItem("nexus_preview", JSON.stringify({ token: "t", sessionId: "s", role: { id: "r", name: "r" }, expiresAt: "2099-01-01T00:00:00Z", returnTo: "/studio" }));
    const { Wrap } = wrapper();
    const r = renderHook(() => usePermissions(), { wrapper: Wrap });
    await waitFor(() => expect(r.result.current.can("view", "crm_accounts")).toBe(true));
    expect(r.result.current.can("edit", "crm_accounts")).toBe(false);
  });
});
