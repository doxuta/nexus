// specs/078 FR-135 for van-hanh/suc-khoe: the token in this tab belongs to another customer => the health read is never sent,
// so the numbers of one customer are never painted as another's. Uses the REAL useProjectQuery.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { getTenantHealth } from "@/lib/admin";
import { HealthPage } from "./HealthPage";

let tokenOk = true;
vi.mock("@/lib/studioContext", () => ({ useStudioIdentity: () => ({ matches: () => tokenOk }) }));
vi.mock("./UsageSection", () => ({ UsageSection: () => null }));
vi.mock("@/lib/admin", async (o) => ({ ...(await o<typeof import("@/lib/admin")>()), getTenantHealth: vi.fn() }));

const wrap = () => (
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <HealthPage slug="northwind" />
  </QueryClientProvider>
);

beforeEach(() => {
  vi.clearAllMocks();
  tokenOk = true;
});

describe("HealthPage isolation", () => {
  it("token of another project: 0 fetches and no 'không lỗi'", () => {
    tokenOk = false;
    render(wrap());
    expect(getTenantHealth).not.toHaveBeenCalled();
    expect(screen.queryByText("không lỗi")).toBeNull();
  });
  it("own token: one fetch, then the sentence", async () => {
    vi.mocked(getTenantHealth).mockResolvedValue({
      status: "ok",
      generatedAt: "2026-10-14T10:00:00Z",
      queueEnabled: true,
      jobs: { failed7d: [], stuck: 0 },
      workflowRuns: { failed24h: 0, failed7d: 0, topFailing: [] },
      emails: { failed24h: 0, failed7d: 0, unresentFailed: 0 },
      inboundHooks: { failed24h: 0, byHook: [] },
      doctor: { status: "ok", critical: 0, warn: 0, checkedAt: "2026-10-14T08:00:00Z" },
    });
    render(wrap());
    await waitFor(() => expect(screen.getByText("không lỗi")).toBeInTheDocument());
    expect(getTenantHealth).toHaveBeenCalledTimes(1);
  });
});
