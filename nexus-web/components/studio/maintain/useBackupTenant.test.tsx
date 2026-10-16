// specs/079 f7-sao-luu-bang T090: the tenant id of the backup page never comes from the URL.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({ identity: {} as Record<string, unknown> }));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), listProjects: vi.fn() }));
vi.mock("@/lib/studioContext", async (orig) => ({ ...(await orig<typeof import("@/lib/studioContext")>()), useStudioIdentity: () => h.identity }));

import { listProjects } from "@/lib/api";
import { useBackupTenant } from "./useBackupTenant";

const ROW = { tenantId: "t-acme", slug: "acme", name: "Acme" };
const wrap = ({ children }: { children: ReactNode }) => (
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>{children}</QueryClientProvider>
);
const ident = (x: Record<string, unknown> = {}) => ({
  status: "ready", home: "northwind", me: { isPlatformOperator: true, tenantId: "t-home", workspace: null },
  matches: (s: string) => s === "northwind", ...x,
});

beforeEach(() => {
  vi.mocked(listProjects).mockReset().mockResolvedValue([ROW] as never);
  h.identity = ident();
});

describe("useBackupTenant", () => {
  it("customer: loading, then ok with the registry row's tenantId", async () => {
    const { result } = renderHook(() => useBackupTenant("acme"), { wrapper: wrap });
    expect(result.current.state).toBe("loading");
    await waitFor(() => expect(result.current.state).toBe("ok"));
    expect(result.current).toMatchObject({ slug: "acme", tenantId: "t-acme", isHome: false });
  });

  it("customer not in the registry ⇒ missing; registry error ⇒ error", async () => {
    const { result } = renderHook(() => useBackupTenant("ghost"), { wrapper: wrap });
    await waitFor(() => expect(result.current.state).toBe("missing"));
    vi.mocked(listProjects).mockRejectedValue(new Error("x"));
    const r2 = renderHook(() => useBackupTenant("acme"), { wrapper: wrap });
    await waitFor(() => expect(r2.result.current.state).toBe("error"));
  });

  it("home workshop: tenantId from /auth/me, registry never asked", () => {
    const { result } = renderHook(() => useBackupTenant("northwind"), { wrapper: wrap });
    expect(result.current).toMatchObject({ state: "ok", tenantId: "t-home", isHome: true });
    expect(listProjects).not.toHaveBeenCalled();
  });

  it("home with no me.tenantId, or me still loading / failed ⇒ never ok, no 'undefined' id", () => {
    h.identity = ident({ me: { isPlatformOperator: true, workspace: null } });
    expect(renderHook(() => useBackupTenant("northwind"), { wrapper: wrap }).result.current.state).toBe("error");
    h.identity = ident({ status: "loading", me: undefined });
    expect(renderHook(() => useBackupTenant("northwind"), { wrapper: wrap }).result.current.state).toBe("loading");
    h.identity = ident({ status: "error", me: undefined });
    expect(renderHook(() => useBackupTenant("northwind"), { wrapper: wrap }).result.current.state).toBe("error");
  });

  it("customer S while /auth/me says the tab is at home (S = home code of the workshop) never borrows me.tenantId", async () => {
    const { result } = renderHook(() => useBackupTenant("acme"), { wrapper: wrap });
    await waitFor(() => expect(result.current.state).toBe("ok"));
    expect(result.current.tenantId).toBe("t-acme");
  });

  it("a session in the tab ⇒ error and the registry is not asked", () => {
    h.identity = ident({ me: { isPlatformOperator: true, tenantId: "t-x", workspace: { tenantSlug: "acme" } } });
    expect(renderHook(() => useBackupTenant("acme"), { wrapper: wrap }).result.current.state).toBe("error");
    expect(listProjects).not.toHaveBeenCalled();
  });
});
