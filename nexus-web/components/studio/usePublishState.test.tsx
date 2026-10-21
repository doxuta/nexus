import { QueryClientProvider } from "@tanstack/react-query";
import { act, render, renderHook, waitFor } from "@testing-library/react";
import { StrictMode, type ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { makeClient } from "@/lib/query/client";
import { emitDraftsChanged } from "@/lib/draftEvents";

const matches = vi.fn<(s: string) => boolean>();
vi.mock("@/lib/studioContext", () => ({ useStudioIdentity: () => ({ matches }) }));
const getPublishState = vi.fn();
vi.mock("@/lib/api", () => ({ getPublishState: () => getPublishState() }));

import { READ_TIMEOUT_MS, usePublishState } from "./usePublishState";

// Issue #46: the header, the draft bar, the assistant… each mount usePublishState. One save announces once on the
// drafts bus, and that must cost ONE GET /admin/publish/state, not one per mounted reader (each invalidate cancels the
// read in flight and sends another, and getPublishState takes no signal, so every cancelled read still hits the server).
function Readers({ n }: { n: number }) {
  return <>{Array.from({ length: n }, (_, i) => <Reader key={i} />)}</>;
}
function Reader() {
  usePublishState("acme");
  return null;
}
// Reads one announcement costs, once every read it started has answered (a stray second listener would show here).
async function readsPerEmit(c: ReturnType<typeof makeClient>): Promise<number> {
  const before = getPublishState.mock.calls.length;
  act(() => emitDraftsChanged("screen-save"));
  await new Promise((r) => setTimeout(r, 20));
  await waitFor(() => expect(c.isFetching()).toBe(0));
  return getPublishState.mock.calls.length - before;
}

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

  it("one announcement on the drafts bus sends one read, however many readers are mounted (#46)", async () => {
    matches.mockReturnValue(true);
    getPublishState.mockResolvedValue({ delivered: true, pendingCount: 0 });
    const c = makeClient(); // the app's 2-minute staleTime: only the announcement re-reads
    const { rerender, unmount } = render(<QueryClientProvider client={c}><Readers n={5} /></QueryClientProvider>);
    await waitFor(() => expect(getPublishState).toHaveBeenCalledTimes(1));
    act(() => emitDraftsChanged("screen-save"));
    await waitFor(() => expect(c.isFetching()).toBe(0));
    expect(getPublishState).toHaveBeenCalledTimes(2);

    // The listener lives as long as one reader does, and goes with the last one.
    rerender(<QueryClientProvider client={c}><Readers n={1} /></QueryClientProvider>);
    act(() => emitDraftsChanged("screen-save"));
    await waitFor(() => expect(getPublishState).toHaveBeenCalledTimes(3));
    unmount();
    act(() => emitDraftsChanged("screen-save"));
    await new Promise((r) => setTimeout(r, 20));
    expect(getPublishState).toHaveBeenCalledTimes(3);
  });

  // The shared listener must come back after it went away: a dead entry left behind (or a live one never removed) would
  // turn the next announcement into no read, or two.
  describe("the listener is registered again once every reader has gone (#46)", () => {
    beforeEach(() => {
      matches.mockReturnValue(true);
      getPublishState.mockResolvedValue({ delivered: true, pendingCount: 0 });
    });

    it("full unmount, then remount", async () => {
      const c = makeClient();
      const first = render(<QueryClientProvider client={c}><Readers n={3} /></QueryClientProvider>);
      await waitFor(() => expect(getPublishState).toHaveBeenCalledTimes(1));
      first.unmount();
      render(<QueryClientProvider client={c}><Readers n={3} /></QueryClientProvider>);
      expect(await readsPerEmit(c)).toBe(1);
      expect(await readsPerEmit(c)).toBe(1);
    });

    it("StrictMode mounts, cleans up and mounts again", async () => {
      const c = makeClient();
      render(<StrictMode><QueryClientProvider client={c}><Readers n={3} /></QueryClientProvider></StrictMode>);
      await waitFor(() => expect(getPublishState).toHaveBeenCalled());
      await waitFor(() => expect(c.isFetching()).toBe(0));
      expect(await readsPerEmit(c)).toBe(1);
    });

    it("1 → 0 → 1 in one commit (the only reader replaced by another)", async () => {
      const c = makeClient();
      const { rerender } = render(<QueryClientProvider client={c}><Reader key="a" /></QueryClientProvider>);
      await waitFor(() => expect(getPublishState).toHaveBeenCalledTimes(1));
      rerender(<QueryClientProvider client={c}><Reader key="b" /></QueryClientProvider>);
      expect(await readsPerEmit(c)).toBe(1);
    });
  });
});
