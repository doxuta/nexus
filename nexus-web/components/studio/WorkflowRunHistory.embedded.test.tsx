// specs/078 lát u7-tu-dong-tong-quan T105: WorkflowRunHistory inside the Studio frame.
import { render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { useProjectQuery } from "@/components/studio/useProjectQuery";
import { listWorkflowRuns } from "@/lib/api";
import type { WorkflowRun } from "@/lib/workflowRuns";
import { WorkflowRunHistory } from "./WorkflowRunHistory";

vi.mock("next/navigation", () => ({ usePathname: () => "/studio/du-an/northwind/van-hanh/tu-dong/lan-chay" }));
vi.mock("@/components/studio/useProjectQuery", () => ({ useProjectQuery: vi.fn() }));
vi.mock("@/lib/api", async (o) => ({ ...(await o<typeof import("@/lib/api")>()), listWorkflowRuns: vi.fn(), listRecentWorkflowRuns: vi.fn() }));

const RUN = { id: "r1", workflowId: "w1", triggerType: "on_create", recordId: "abcdef123456", status: "success", error: null, actionsResult: null, startedAt: "2026-10-14T01:00:00Z", finishedAt: "2026-10-14T01:00:01Z" } as unknown as WorkflowRun;
const entities = (data: { name: string }[]) =>
  vi.mocked(useProjectQuery).mockReturnValue({ isPending: false, isError: false, error: null, data, refetch: vi.fn() } as never);

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(listWorkflowRuns).mockResolvedValue({ runs: [RUN], total: 1, limit: 20, offset: 0 });
});

describe("WorkflowRunHistory embedded", () => {
  it("record id links inside the frame; status is coloured text, not a Badge", async () => {
    entities([{ name: "crm_accounts" }]);
    render(<EmbeddedSurface><WorkflowRunHistory workflowId="w1" entityName="crm_accounts" /></EmbeddedSurface>);
    const grid = await screen.findByRole("grid", { name: "Lịch sử chạy" });
    expect(within(grid).getByRole("link", { name: "abcdef12" })).toHaveAttribute("href", "/studio/du-an/northwind/van-hanh/du-lieu/crm_accounts/abcdef123456");
    expect(grid.querySelector(".slds-badge")).toBeNull();
    expect(grid.querySelector(".nx-status")).not.toBeNull();
  });
  it("a table that no longer exists says Bảng đã xoá and has no link", async () => {
    entities([{ name: "crm_leads" }]);
    render(<EmbeddedSurface><WorkflowRunHistory workflowId="w1" entityName="crm_accounts" /></EmbeddedSurface>);
    const grid = await screen.findByRole("grid", { name: "Lịch sử chạy" });
    expect(within(grid).getByText("Bảng đã xoá")).toBeInTheDocument();
    expect(within(grid).queryByRole("link")).toBeNull();
  });
});
