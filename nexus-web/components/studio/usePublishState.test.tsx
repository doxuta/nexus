import { QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { makeClient } from "@/lib/query/client";

const matches = vi.fn<(s: string) => boolean>();
vi.mock("@/lib/studioContext", () => ({ useStudioIdentity: () => ({ matches }) }));
const getPublishState = vi.fn();
vi.mock("@/lib/api", () => ({ getPublishState: () => getPublishState() }));

import { READ_TIMEOUT_MS, usePublishState } from "./usePublishState";

const wrap = (c: ReturnType<typeof makeClient>) =>
  function W({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={c}>{children}</QueryClientProvider>;
  };
beforeEach(() => {
  matches.mockReset();
  getPublishState.mockReset();
});

describe("usePublishState", () => {
  it("reads publish state when the token belongs to the project; key carries the slug", async () => {
    matches.mockReturnValue(true);
    getPublishState.mockResolvedValue({ delivered: true, pendingCount: 2 });
    const c = makeClient();
    const { result } = renderHook(() => usePublishState("acme"), { wrapper: wrap(c) });
    await waitFor(() => expect(result.current.data?.pendingCount).toBe(2));
    expect(c.getQueryCache().getAll()[0].queryKey).toEqual(["project", "acme", "publish", "state"]);
  });

  it("never calls the route for a project the token does not belong to", async () => {
    matches.mockReturnValue(false);
    renderHook(() => usePublishState("acme"), { wrapper: wrap(makeClient()) });
    await new Promise((r) => setTimeout(r, 20));
    expect(getPublishState).not.toHaveBeenCalled();
  });

  it("re-reads when the window regains focus", async () => {
    matches.mockReturnValue(true);
    getPublishState.mockResolvedValue({ delivered: true, pendingCount: 0 });
    const c = makeClient();
    c.setDefaultOptions({ queries: { ...c.getDefaultOptions().queries, staleTime: 0 } });
    renderHook(() => usePublishState("acme"), { wrapper: wrap(c) });
    await waitFor(() => expect(getPublishState).toHaveBeenCalledTimes(1));
    document.dispatchEvent(new Event("visibilitychange", { bubbles: true }));
    await waitFor(() => expect(getPublishState).toHaveBeenCalledTimes(2));
  });

  it("a read that never answers turns into an error after 8 s, with no retry (FR-130)", async () => {
    vi.useFakeTimers();
    try {
      matches.mockReturnValue(true);
      getPublishState.mockReturnValue(new Promise(() => {}));
      const { result } = renderHook(() => usePublishState("acme"), { wrapper: wrap(makeClient()) });
      await vi.advanceTimersByTimeAsync(READ_TIMEOUT_MS - 1);
      expect(result.current.isError).toBe(false);
      await vi.advanceTimersByTimeAsync(2);
      expect(result.current.isError).toBe(true);
      await vi.advanceTimersByTimeAsync(20000);
      expect(getPublishState).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });
});
