// specs/073 kh1a T165 + T170: every token-scoped read in the project frame goes through this hook, which is
// closed unless the token belongs to the project, and never paints another project's data.
import { QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { makeClient } from "@/lib/query/client";

const matches = vi.fn<(s: string) => boolean>();
vi.mock("@/lib/studioContext", () => ({ useStudioIdentity: () => ({ matches }) }));

import { useProjectQuery } from "./useProjectQuery";

// The app's REAL defaults (keepPreviousData, refetchOnWindowFocus false, retry 1), not a bare test client.
const client = () => {
  const c = makeClient();
  c.setDefaultOptions({ queries: { ...c.getDefaultOptions().queries, retry: false } });
  return c;
};
const wrap = (c: ReturnType<typeof client>) =>
  function Wrap({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={c}>{children}</QueryClientProvider>;
  };

beforeEach(() => matches.mockReset());

describe("enabled follows projectTokenMatches (T165)", () => {
  it("does not call the fetcher when the token does not match, and stays pending (not errored)", async () => {
    matches.mockReturnValue(false);
    const fetcher = vi.fn().mockResolvedValue("x");
    const { result } = renderHook(() => useProjectQuery("acme", ["health"], fetcher), { wrapper: wrap(client()) });
    await new Promise((r) => setTimeout(r, 20));
    expect(fetcher).not.toHaveBeenCalled();
    expect(result.current.isError).toBe(false);
    expect(result.current.fetchStatus).toBe("idle");
  });

  it("still does not call the fetcher after refetch() or a focus refetch", async () => {
    matches.mockReturnValue(false);
    const fetcher = vi.fn().mockResolvedValue("x");
    const { result } = renderHook(() => useProjectQuery("acme", ["health"], fetcher), { wrapper: wrap(client()) });
    await act(async () => {
      await result.current.refetch();
      window.dispatchEvent(new Event("focus"));
    });
    expect(fetcher).not.toHaveBeenCalled();
  });

  it("calls the fetcher when the token matches", async () => {
    matches.mockReturnValue(true);
    const fetcher = vi.fn().mockResolvedValue("hello");
    const { result } = renderHook(() => useProjectQuery("acme", ["health"], fetcher), { wrapper: wrap(client()) });
    await waitFor(() => expect(result.current.data).toBe("hello"));
    expect(matches).toHaveBeenCalledWith("acme");
  });
});

describe("cache isolation between projects (T170, FR-025)", () => {
  it("while B loads, data is undefined — never A's data — even with keepPreviousData as the app default", async () => {
    matches.mockReturnValue(true);
    let resolveB: (v: string) => void = () => {};
    const fetcher = vi.fn((slug: string) => (slug === "a" ? Promise.resolve("A-data") : new Promise<string>((r) => (resolveB = r))));
    const { result, rerender } = renderHook(({ slug }) => useProjectQuery(slug, ["health"], () => fetcher(slug)), {
      wrapper: wrap(client()),
      initialProps: { slug: "a" },
    });
    await waitFor(() => expect(result.current.data).toBe("A-data"));

    rerender({ slug: "b" });
    await waitFor(() => expect(fetcher).toHaveBeenCalledWith("b"));
    expect(result.current.data).toBeUndefined();
    expect(result.current.isPlaceholderData).toBe(false);

    await act(async () => resolveB("B-data"));
    await waitFor(() => expect(result.current.data).toBe("B-data"));
  });

  it("the key carries the project code, and the tenant id on a `nha` page", async () => {
    matches.mockReturnValue(true);
    const c = client();
    const a = renderHook(() => useProjectQuery("acme", ["backups"], async () => 1), { wrapper: wrap(c) });
    const b = renderHook(() => useProjectQuery("acme", ["backups"], async () => 2, { tenantId: "t-1" }), { wrapper: wrap(c) });
    await waitFor(() => expect(a.result.current.data).toBe(1));
    await waitFor(() => expect(b.result.current.data).toBe(2));
    const keys = c.getQueryCache().getAll().map((q) => q.queryKey);
    expect(keys).toContainEqual(["project", "acme", "backups"]);
    expect(keys).toContainEqual(["project", "acme", "t-1", "backups"]);
  });
});
