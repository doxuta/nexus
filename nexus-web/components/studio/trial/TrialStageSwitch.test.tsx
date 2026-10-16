import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";

let rows: unknown[] = [];
const patchProject = vi.fn();
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  listProjects: async () => rows,
  patchProject: (...a: unknown[]) => patchProject(...a),
}));
vi.mock("@/lib/studioContext", async (orig) => ({ ...(await orig<typeof import("@/lib/studioContext")>()), homeSlug: () => "northwind" }));

import { ApiError } from "@/lib/api";
import { TrialStageSwitch } from "./TrialStageSwitch";

const row = (o: Record<string, unknown>) => ({ tenantId: "t-1", slug: "acme", stage: "building", trialSince: null, deliveredAt: null, ...o });
function ui(slug = "acme") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const inv = vi.spyOn(qc, "invalidateQueries");
  render(<QueryClientProvider client={qc}><TrialStageSwitch slug={slug} /></QueryClientProvider>);
  return inv;
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe("TrialStageSwitch", () => {
  it("building: 'Bắt đầu chạy thử' sends trial:true and refreshes the current project", async () => {
    rows = [row({})];
    patchProject.mockResolvedValue(row({ stage: "trial", trialSince: "2026-10-14" }));
    const inv = ui();
    await userEvent.click(await screen.findByRole("button", { name: "Bắt đầu chạy thử" }));
    expect(patchProject).toHaveBeenCalledWith("t-1", { trial: true });
    await waitFor(() => expect(inv).toHaveBeenCalledWith({ queryKey: ["project", "current"] }));
    expect(await screen.findByRole("button", { name: "Về Đang dựng" })).toBeTruthy();
  });

  it("trial: 'Về Đang dựng' sends trial:false", async () => {
    rows = [row({ stage: "trial", trialSince: "2026-10-01" })];
    patchProject.mockResolvedValue(row({}));
    ui();
    await userEvent.click(await screen.findByRole("button", { name: "Về Đang dựng" }));
    expect(patchProject).toHaveBeenCalledWith("t-1", { trial: false });
  });

  it("delivered: no button, never says Xong/Hoàn thành", async () => {
    rows = [row({ stage: "live", deliveredAt: "2026-10-02" })];
    ui();
    await screen.findByText("Đang vận hành");
    expect(screen.queryByRole("button")).toBeNull();
    expect(document.body.textContent).not.toMatch(/Xong|Hoàn thành/);
  });

  it("workshop project (no ledger row): nothing drawn", async () => {
    rows = [row({})];
    ui("northwind");
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("422 PROJECT_TRIAL_AFTER_DELIVERY reloads the ledger", async () => {
    rows = [row({})];
    patchProject.mockRejectedValue(new ApiError(422, "PROJECT_TRIAL_AFTER_DELIVERY", "x"));
    const inv = ui();
    await userEvent.click(await screen.findByRole("button", { name: "Bắt đầu chạy thử" }));
    await waitFor(() => expect(inv).toHaveBeenCalledWith({ queryKey: ["studio", "projects"] }));
  });
});
