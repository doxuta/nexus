import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const listEntities = vi.fn();
const getSampleDataSummary = vi.fn();
const wipeSampleData = vi.fn();
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  listEntities: () => listEntities(),
  getSampleDataSummary: (...a: unknown[]) => getSampleDataSummary(...a),
  wipeSampleData: (...a: unknown[]) => wipeSampleData(...a),
}));
vi.mock("../useProjectQuery", () => ({
  useProjectQuery: (slug: string, key: unknown[], fn: () => Promise<unknown>) =>
    useQuery({ queryKey: ["project", slug, ...key], queryFn: fn, staleTime: 0 }),
}));

import { ApiError } from "@/lib/api";
import { invalidateAfterApply } from "@/lib/aiInvalidate";
import { SampleDataByTable } from "./SampleDataByTable";
import { invalidateSampleData, joinSampleRows } from "./useSampleSummary";

const ENTS = ["leads", "contacts", "deals", "tickets", "notes"].map((n) => ({ id: n, name: n, label: n.toUpperCase() }));
const SUMMARY = {
  total: 10,
  entities: [
    { entity: "leads", label: "Leads", count: 8, live: 6, inBin: 2, bySource: { ai: 3, generator: 5, pack: 0 } },
    { entity: "contacts", label: "Contacts", count: 2, live: 2, inBin: 0, bySource: { pack: 2 } },
  ],
};

function ui(qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })) {
  return render(
    <QueryClientProvider client={qc}>
      <SampleDataByTable slug="northwind" />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  listEntities.mockResolvedValue(ENTS);
  getSampleDataSummary.mockResolvedValue(SUMMARY);
});

describe("SampleDataByTable", () => {
  it("one row per table (zeros included), one request each, whole project", async () => {
    ui();
    expect(await screen.findByText("10 bản ghi ở 2 bảng")).toBeTruthy();
    const rows = screen.getAllByRole("row").slice(1);
    expect(rows).toHaveLength(5);
    expect(within(rows[0]).getByText("Trợ lý 3 · Tự sinh 5")).toBeTruthy();
    expect(within(rows[1]).getByText("Gói 2")).toBeTruthy();
    expect(within(rows[2]).getAllByText("0").length).toBe(2);
    expect(listEntities).toHaveBeenCalledTimes(1);
    expect(getSampleDataSummary).toHaveBeenCalledTimes(1);
    expect(getSampleDataSummary).toHaveBeenCalledWith();
  });

  it("shows the bin column, no badge and both the generate and wipe buttons", async () => {
    const { container } = ui();
    await screen.findByText("10 bản ghi ở 2 bảng");
    expect(within(screen.getAllByRole("row")[1]).getByText("2")).toBeTruthy();
    expect(container.querySelector(".slds-badge")).toBeNull();
    expect(screen.getByRole("button", { name: "Xoá dữ liệu mẫu" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Sinh dữ liệu mẫu" })).toBeTruthy();
    expect(screen.queryByText(/chưa có trong bản Nexus/)).toBeNull();
    expect(container.querySelector(".slds-max-medium-table_stacked")).toBeTruthy();
  });

  it("says so when no table holds a sample row", async () => {
    getSampleDataSummary.mockResolvedValue({ total: 0, entities: [] });
    ui();
    expect(await screen.findByText("Chưa có dòng mẫu nào trong dự án.")).toBeTruthy();
  });

  it("403 gets its own sentence", async () => {
    getSampleDataSummary.mockRejectedValue(new ApiError(403, "FORBIDDEN", "no"));
    ui();
    expect(await screen.findByText("Việc này chỉ người dựng Nexus làm được.")).toBeTruthy();
  });

  it("invalidateSampleData refreshes exactly the sampleData key", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    ui(qc);
    await screen.findByText("10 bản ghi ở 2 bảng");
    await invalidateSampleData(qc, "northwind");
    await waitFor(() => expect(getSampleDataSummary).toHaveBeenCalledTimes(2));
    expect(listEntities).toHaveBeenCalledTimes(1);
  });
});

describe("generate controls", () => {
  it("header button opens the form with every table ticked; each row has one menu with Sinh thêm limited to that table", async () => {
    const user = userEvent.setup();
    ui();
    await screen.findByText("10 bản ghi ở 2 bảng");
    expect(screen.getAllByRole("button", { name: /^Thao tác cho bảng / })).toHaveLength(5);
    await user.click(screen.getByRole("button", { name: "Sinh dữ liệu mẫu" }));
    expect(screen.getAllByRole("checkbox").filter((c) => (c as HTMLInputElement).checked)).toHaveLength(5);
    await user.click(screen.getByRole("button", { name: "Huỷ" }));
    await user.click(screen.getByRole("button", { name: "Thao tác cho bảng DEALS" }));
    await user.click(await screen.findByRole("menuitem", { name: "Sinh thêm" }));
    const on = screen.getAllByRole("checkbox").filter((c) => (c as HTMLInputElement).checked);
    expect(on).toHaveLength(1);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

describe("wipe (T123)", () => {
  it("project-wide button sends every table holding samples with the snapshot total", async () => {
    wipeSampleData.mockResolvedValue({ deleted: [{ entity: "leads", count: 8 }], kept: [], blobsRemoved: 0, sequencesReset: [] });
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const inv = vi.spyOn(qc, "invalidateQueries");
    ui(qc);
    await screen.findByText("10 bản ghi ở 2 bảng");
    fireEvent.click(screen.getByRole("button", { name: "Xoá dữ liệu mẫu" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Xoá 10 bản ghi" }));
    await waitFor(() => expect(wipeSampleData).toHaveBeenCalledWith(10, ["leads", "contacts"]));
    await screen.findByText("Đã xoá 8.");
    const keys = inv.mock.calls.map((c) => c[0]?.queryKey);
    expect(keys).toContainEqual(["project", "northwind", "studio", "handoff"]);
    expect(keys).toContainEqual(["project", "northwind", "studio", "sampleData"]);
  });

  it("switching table while the box is open re-snapshots: never sends table A's count/label for B", async () => {
    getSampleDataSummary.mockResolvedValue({
      total: 11,
      entities: [
        { entity: "leads", label: "Leads", count: 8, live: 8, inBin: 0, bySource: { pack: 8 } },
        { entity: "contacts", label: "Contacts", count: 3, live: 3, inBin: 0, bySource: { pack: 3 } },
      ],
    });
    wipeSampleData.mockResolvedValue({ deleted: [], kept: [], blobsRemoved: 0, sequencesReset: [] });
    ui();
    await screen.findByText("11 bản ghi ở 2 bảng");
    await userEvent.click(screen.getByRole("button", { name: "Thao tác cho bảng LEADS" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Xoá mẫu của bảng này" }));
    await screen.findByRole("button", { name: "Xoá 8 bản ghi" });
    await userEvent.click(screen.getByRole("button", { name: "Thao tác cho bảng CONTACTS" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Xoá mẫu của bảng này" }));
    expect(screen.queryByRole("button", { name: "Xoá 8 bản ghi" })).toBeNull();
    fireEvent.click(await screen.findByRole("button", { name: "Xoá 3 bản ghi" }));
    await waitFor(() => expect(wipeSampleData).toHaveBeenCalledWith(3, ["contacts"]));
  });

  it("cancel closes the box and returns focus to the section heading", async () => {
    ui();
    await screen.findByText("10 bản ghi ở 2 bảng");
    fireEvent.click(screen.getByRole("button", { name: "Xoá dữ liệu mẫu" }));
    fireEvent.click(await screen.findByRole("button", { name: "Huỷ" }));
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("heading", { name: "Dữ liệu mẫu theo bảng" })));
    expect(wipeSampleData).not.toHaveBeenCalled();
  });

  it("lists the rows the server kept after a wipe", async () => {
    wipeSampleData.mockResolvedValue({ deleted: [{ entity: "leads", count: 7 }], kept: [{ entity: "leads", recordId: "L1", reason: "busy", blockedBy: [] }], blobsRemoved: 0, sequencesReset: [] });
    ui();
    await screen.findByText("10 bản ghi ở 2 bảng");
    fireEvent.click(screen.getByRole("button", { name: "Xoá dữ liệu mẫu" }));
    fireEvent.click(await screen.findByRole("button", { name: "Xoá 10 bản ghi" }));
    await screen.findByText("Đã xoá 7.");
    expect(screen.getByTestId("sample-kept-L1")).toBeTruthy();
  });

  it("row menu wipes one table only (wipe item only on tables with rows)", async () => {
    wipeSampleData.mockResolvedValue({ deleted: [], kept: [], blobsRemoved: 0, sequencesReset: [] });
    ui();
    await screen.findByText("10 bản ghi ở 2 bảng");
    expect(screen.getAllByRole("button", { name: /^Thao tác cho bảng/ })).toHaveLength(5);
    await userEvent.click(screen.getByRole("button", { name: "Thao tác cho bảng LEADS" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Xoá mẫu của bảng này" }));
    fireEvent.click(await screen.findByRole("button", { name: "Xoá 8 bản ghi" }));
    await waitFor(() => expect(wipeSampleData).toHaveBeenCalledWith(8, ["leads"]));
  });
});

describe("joinSampleRows", () => {
  it("fills missing bySource keys with 0 and marks equal labels", () => {
    const rows = joinSampleRows(
      [{ name: "a", label: "Khách" }, { name: "b", label: "Khách" }],
      { entities: [{ entity: "a", label: "x", count: 1, live: 1, inBin: 0, bySource: { pack: 1 } as never }] },
    );
    expect(rows[0].bySource).toEqual({ generator: 0, ai: 0, pack: 1 });
    expect(rows.every((r) => r.ambiguous)).toBe(true);
  });
});

// T036a: the key prefix is the contract F4's invalidateAfterApply relies on.
describe("sampleData key contract", () => {
  it("invalidateSampleData marks the project's sampleData query stale", async () => {
    const qc = new QueryClient();
    qc.setQueryData(["project", "northwind", "studio", "sampleData"], { total: 1 });
    await invalidateSampleData(qc, "northwind");
    expect(qc.getQueryState(["project", "northwind", "studio", "sampleData"])?.isInvalidated).toBe(true);
  });
  it("F4 invalidateAfterApply refreshes the sampleData key", () => {
    const qc = new QueryClient();
    qc.setQueryData(["project", "northwind", "studio", "sampleData"], { total: 1 });
    invalidateAfterApply(qc);
    expect(qc.getQueryState(["project", "northwind", "studio", "sampleData"])?.isInvalidated).toBe(true);
  });
});
