import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

let matches = true;
const listRoles = vi.fn(async () => [{ id: "r1", name: "crm_sales_rep" }]);
vi.mock("@/lib/api", () => ({ listRoles: () => listRoles() }));
vi.mock("@/lib/studioContext", () => ({ useStudioIdentity: () => ({ matches: () => matches }) }));

import { trialRolesKey, useTrialRoles } from "./useTrialRoles";

function setup() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => createElement(QueryClientProvider, { client: qc }, children);
  return { qc, wrapper };
}

beforeEach(() => {
  vi.clearAllMocks();
  matches = true;
});

describe("useTrialRoles", () => {
  it("reads listRoles under a key that starts with the project code", async () => {
    const { qc, wrapper } = setup();
    const { result } = renderHook(() => useTrialRoles("acme"), { wrapper });
    await waitFor(() => expect(result.current.data).toHaveLength(1));
    expect(qc.getQueryCache().getAll().map((q) => q.queryKey)).toEqual([["project", "acme", ...trialRolesKey]]);
  });

  it("two projects never share a cache entry", async () => {
    const { qc, wrapper } = setup();
    renderHook(() => useTrialRoles("a"), { wrapper });
    renderHook(() => useTrialRoles("b"), { wrapper });
    await waitFor(() => expect(listRoles).toHaveBeenCalledTimes(2));
    expect(qc.getQueryCache().getAll()).toHaveLength(2);
  });

  it("never calls listRoles while the token does not belong to the project", async () => {
    matches = false;
    const { wrapper } = setup();
    const { result } = renderHook(() => useTrialRoles("acme"), { wrapper });
    await new Promise((r) => setTimeout(r, 30));
    expect(listRoles).not.toHaveBeenCalled();
    expect(result.current.data).toBeUndefined();
  });

  it("is fresh: a second mount asks again instead of reusing the cached list", async () => {
    const { wrapper } = setup();
    const first = renderHook(() => useTrialRoles("acme"), { wrapper });
    await waitFor(() => expect(first.result.current.data).toBeDefined());
    first.unmount();
    renderHook(() => useTrialRoles("acme"), { wrapper });
    await waitFor(() => expect(listRoles).toHaveBeenCalledTimes(2));
  });
});
