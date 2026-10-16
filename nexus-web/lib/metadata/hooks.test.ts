// useEntities / useFields / useLayout `draftRead` (specs/074 FR-064, lát f2-draft-labels-data-menu):
// the flag puts the draft header on THAT screen's GET only (a delivered project), and keeps the draft reading under
// its own cache key so the customer app's cache never holds the builder's overlay.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";

vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { id: "u1" } }) }));

import { useEntities, useFields, useLayout } from "./hooks";

const fetchMock = vi.fn();
const wrapper = () => {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return function Wrapper({ children }: { children: ReactNode }) {
    return createElement(QueryClientProvider, { client: qc }, children);
  };
};
const headerOf = (i: number) => (fetchMock.mock.calls[i][1] as RequestInit).headers as Record<string, string>;

beforeEach(() => {
  fetchMock.mockReset();
  fetchMock.mockResolvedValue(new Response(JSON.stringify({ data: [] }), { status: 200, headers: { "Content-Type": "application/json" } }));
  vi.stubGlobal("fetch", fetchMock);
});
afterEach(() => vi.unstubAllGlobals());

describe("metadata hooks draftRead", () => {
  it("sends X-Nexus-Draft on the GET when draftRead is on", async () => {
    const { result } = renderHook(() => useEntities({ draftRead: true }), { wrapper: wrapper() });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(headerOf(0)["X-Nexus-Draft"]).toBe("1");
  });

  it("sends no draft header when draftRead is off (project not delivered)", async () => {
    const { result } = renderHook(() => useEntities(), { wrapper: wrapper() });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(headerOf(0)["X-Nexus-Draft"]).toBeUndefined();
  });

  it("covers fields and layout, and a later non-draft call is not tagged", async () => {
    const f = renderHook(() => useFields("hk_deals", { draftRead: true }), { wrapper: wrapper() });
    await waitFor(() => expect(f.result.current.isSuccess).toBe(true));
    const l = renderHook(() => useLayout("hk_deals", { draftRead: true }), { wrapper: wrapper() });
    await waitFor(() => expect(l.result.current.isSuccess).toBe(true));
    expect(headerOf(0)["X-Nexus-Draft"]).toBe("1");
    expect(headerOf(1)["X-Nexus-Draft"]).toBe("1");
    const plain = renderHook(() => useFields("hk_deals"), { wrapper: wrapper() });
    await waitFor(() => expect(plain.result.current.isSuccess).toBe(true));
    expect(headerOf(2)["X-Nexus-Draft"]).toBeUndefined();
  });
});
