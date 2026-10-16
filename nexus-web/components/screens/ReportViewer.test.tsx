// ReportViewer (/reports/[id]) on SLDS — page-headers/record-home + cards + data-tables/base +
// panels/base (admin config, docked right) (specs/038 T030). Pins what the viewer must keep doing
// for each persona: run the report (tabular grid with relation labels, summary chart + group
// table with option labels and drill links), export CSV server-side with the filters on screen
// (client fallback on failure), say honestly why a run failed, and — admin only — open the
// config panel (also from ?config=1).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ReportViewer } from "./ReportViewer";
import {
  ApiError, downloadReportCSV, getPublishState, listFields, listReports, listUserDirectory, runReport,
  type PublishState, type ReportRunData, type ReportSummary,
} from "@/lib/api";
import { toast } from "@/components/ui/Toaster";

const auth = vi.hoisted(() => ({ user: { id: "u1", fullName: "Trần Sales Rep", email: "test@example.com", isAdmin: false } }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => auth }));

const nav = vi.hoisted(() => ({ push: vi.fn(), replace: vi.fn(), search: "" }));
vi.mock("next/navigation", () => ({
  useRouter: () => nav,
  usePathname: () => "/reports/r1",
  useSearchParams: () => new URLSearchParams(nav.search),
}));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    listReports: vi.fn(), runReport: vi.fn(), listFields: vi.fn(), listUserDirectory: vi.fn(), downloadReportCSV: vi.fn(), getPublishState: vi.fn(),
  };
});
vi.mock("@/lib/metadata/hooks", () => ({
  useEntities: () => ({ data: [{ name: "crm_opportunities", label: "Cơ hội" }, { name: "crm_contracts", label: "Hợp đồng" }] }),
  useFields: () => ({ data: [] }),
}));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), warning: vi.fn(), error: vi.fn() } }));
// The filter bar is the dashboards lane's component (T029) and the charts are recharts — both have
// their own tests; here they are stand-ins so the viewer's own wiring is what gets asserted.
vi.mock("@/components/analytics/RunFilterBar", () => ({ RunFilterBar: () => <div data-testid="run-filter-bar" /> }));
vi.mock("@/components/analytics/ReportConfigPanel", () => ({
  // The panel reads the report itself (getAdminReport) — the viewer hands it the id and a draft-save callback only.
  ReportConfigPanel: ({ reportId, onSavedToDraft }: { reportId: string; onSavedToDraft?: () => void }) => (
    <div data-testid="config-panel">{reportId}<button onClick={onSavedToDraft}>fake draft save</button></div>
  ),
}));
vi.mock("@/components/charts/NxBar", () => ({ NxBar: () => <div data-testid="chart-bar" /> }));
vi.mock("@/components/charts/NxLine", () => ({ NxLine: () => <div data-testid="chart-line" /> }));
vi.mock("@/components/charts/NxPie", () => ({ NxPie: () => <div data-testid="chart-pie" /> }));

const TABULAR: ReportSummary = {
  id: "r1", name: "Danh sách hợp đồng", entityName: "crm_contracts", type: "tabular",
  config: { columns: ["contract_no", "account_id"] }, isActive: true,
};
const SUMMARY: ReportSummary = {
  id: "r2", name: "Pipeline theo giai đoạn", entityName: "crm_opportunities", type: "summary",
  config: { metric: "count", groupBy: "stage" }, isActive: true,
};

const TABULAR_RUN: ReportRunData = {
  type: "tabular",
  columns: [{ name: "contract_no", label: "Số hợp đồng" }, { name: "account_id", label: "Khách hàng" }],
  rows: [
    { id: "c1", contract_no: "HD-001", account_id: { id: "a1", label: "Công ty ABC" } },
    { id: "c2", contract_no: "HD-002", account_id: null },
  ],
  total: 2,
};
const GROUPS = [{ key: "Proposal", value: "3", count: 3 }, { key: "Qualification", value: "5", count: 5 }];
const SUMMARY_RUN: ReportRunData = { type: "summary", groups: GROUPS };

function renderViewer(id: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ReportViewer id={id} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  auth.user.isAdmin = false;
  nav.search = "";
  window.localStorage.clear();
  vi.mocked(listReports).mockResolvedValue([TABULAR, SUMMARY]);
  vi.mocked(getPublishState).mockRejectedValue(new ApiError(403, "FORBIDDEN", "builder only", {})); // not the builder by default
  vi.mocked(listFields).mockResolvedValue([
    { name: "stage", label: "Giai đoạn", fieldType: "select", options: { valueLabels: { Proposal: "Báo giá", Qualification: "Đánh giá" } } },
  ] as never);
});

describe("ReportViewer — fixed filter note (#450)", () => {
  it("rep sees which fixed conditions the report applies", async () => {
    vi.mocked(listReports).mockResolvedValue([{
      ...TABULAR, config: { columns: ["contract_no"], filters: [{ field: "stage", op: "neq", values: ["Lost"] }] },
    }]);
    vi.mocked(runReport).mockResolvedValue(TABULAR_RUN);
    renderViewer("r1");
    expect((await screen.findByText(/Điều kiện cố định/)).closest("p")).toHaveTextContent(/Giai đoạn.*khác.*Lost/);
  });
  it("names the owner in an ownerId condition instead of showing the id", async () => {
    vi.mocked(listUserDirectory).mockResolvedValue([{ id: "u-9", fullName: "KTV Nguyễn A", email: "a@example.com" }] as never);
    vi.mocked(listReports).mockResolvedValue([{
      ...TABULAR, config: { columns: ["contract_no"], filters: [{ field: "ownerId", op: "eq", values: ["u-9"] }] },
    }]);
    vi.mocked(runReport).mockResolvedValue(TABULAR_RUN);
    renderViewer("r1");
    await waitFor(() => expect(screen.getByText(/Điều kiện cố định/).closest("p")).toHaveTextContent(/Người phụ trách.*KTV Nguyễn A/));
  });
  it("shows no note when the report has no fixed filter", async () => {
    vi.mocked(runReport).mockResolvedValue(TABULAR_RUN);
    renderViewer("r1");
    await screen.findByRole("grid", { name: "Kết quả báo cáo" });
    expect(screen.queryByText(/Điều kiện cố định/)).toBeNull();
  });
});

describe("ReportViewer — tabular", () => {
  beforeEach(() => vi.mocked(runReport).mockResolvedValue(TABULAR_RUN));

  it("rep: header names the report, grid shows relation labels, first cell opens the record", async () => {
    renderViewer("r1");
    expect(await screen.findByRole("heading", { level: 1, name: /Danh sách hợp đồng/ })).toBeInTheDocument();
    const grid = await screen.findByRole("grid", { name: "Kết quả báo cáo" });
    expect(within(grid).getByText("Số hợp đồng")).toBeInTheDocument();
    expect(within(grid).getByText("Khách hàng")).toBeInTheDocument();
    // REPORT-RELATION-LABELS: {id,label} reads as the name, never the UUID / [object Object].
    expect(within(grid).getByText("Công ty ABC")).toBeInTheDocument();
    expect(within(grid).getByRole("link", { name: "HD-001" })).toHaveAttribute("href", "/crm_contracts/c1");
    expect(screen.getByText("2 bản ghi")).toBeInTheDocument();
    expect(screen.getByTestId("run-filter-bar")).toBeInTheDocument();
    // Rep: no authoring entry point, no panel.
    expect(screen.queryByRole("button", { name: /Cấu hình/ })).toBeNull();
    expect(screen.queryByTestId("config-panel")).toBeNull();
    expect(runReport).toHaveBeenCalledWith("r1", {});
  });

  it("Xuất CSV downloads the server export with the filters on screen", async () => {
    nav.search = "dateFrom=2026-09-01&dateTo=2026-09-30";
    vi.mocked(downloadReportCSV).mockResolvedValue();
    renderViewer("r1");
    await screen.findByRole("grid", { name: "Kết quả báo cáo" });
    await userEvent.click(screen.getByRole("button", { name: "Xuất CSV" }));
    await waitFor(() =>
      expect(downloadReportCSV).toHaveBeenCalledWith("r1", "Danh sách hợp đồng.csv", { dateFrom: "2026-09-01", dateTo: "2026-09-30" }),
    );
    expect(toast.warning).not.toHaveBeenCalled();
  });

  it("a failed server export falls back to a client CSV of the rows on screen and says so", async () => {
    vi.mocked(downloadReportCSV).mockRejectedValue(new Error("Máy chủ bận."));
    const createObjectURL = vi.fn(() => "blob:x");
    Object.assign(URL, { createObjectURL, revokeObjectURL: vi.fn() });
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    renderViewer("r1");
    await screen.findByRole("grid", { name: "Kết quả báo cáo" });
    await userEvent.click(screen.getByRole("button", { name: "Xuất CSV" }));
    await waitFor(() => expect(toast.warning).toHaveBeenCalled());
    expect(vi.mocked(toast.warning).mock.calls[0][0]).toMatch(/^Máy chủ bận\. Đã tải bản CSV từ dữ liệu đang hiển thị/);
    expect(click).toHaveBeenCalled();
    const blob = (createObjectURL.mock.calls[0] as unknown as [Blob])[0];
    const text = await blob.text();
    expect(text).toContain('"Số hợp đồng","Khách hàng"');
    expect(text).toContain('"HD-001","Công ty ABC"');
    click.mockRestore();
  });
});

describe("ReportViewer — summary", () => {
  beforeEach(() => vi.mocked(runReport).mockResolvedValue(SUMMARY_RUN));

  it("shows the group chart and a table with option labels whose keys drill to the filtered list", async () => {
    renderViewer("r2");
    const grid = await screen.findByRole("grid", { name: "Bảng số liệu theo nhóm" });
    expect(screen.getByTestId("chart-bar")).toBeInTheDocument();
    const link = await within(grid).findByRole("link", { name: "Báo giá" });
    expect(link).toHaveAttribute("href", "/crm_opportunities?stage%5Beq%5D=Proposal");
    expect(within(grid).getByText("Đánh giá")).toBeInTheDocument();
  });

  it("chart card and group table stack on SLDS card+card spacing alone (no extra flex gap)", async () => {
    renderViewer("r2");
    const grid = await screen.findByRole("grid", { name: "Bảng số liệu theo nhóm" });
    const tableCard = grid.closest("article.slds-card")!;
    const chartCard = screen.getByTestId("chart-bar").closest("article.slds-card")!;
    // `.slds-card+.slds-card{margin-top:1rem}` spaces them; a gap on the wrapper would double it.
    expect(chartCard.nextElementSibling).toBe(tableCard);
    expect(tableCard.parentElement!.className).not.toMatch(/\bgap-/);
  });

  it("the chart type switch shows the donut and remembers it per report", async () => {
    renderViewer("r2");
    await screen.findByRole("grid", { name: "Bảng số liệu theo nhóm" });
    const pie = screen.getByRole("button", { name: "Tròn" });
    expect(pie).toHaveAttribute("aria-pressed", "false");
    await userEvent.click(pie);
    expect(screen.getByTestId("chart-pie")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Tròn" })).toHaveAttribute("aria-pressed", "true");
    expect(window.localStorage.getItem("nexus.report.viz.v1.r2")).toBe("pie");
  });

  // specs/049 F0: the old "runs on one axis" note keyed off data.type === "matrix", which the
  // server never returns for a one-axis run (it answers "summary") — it was dead. The truth now
  // comes from the report's own type (admin) and the result's shape (everyone).
  it("a matrix report without a column axis: the admin is told it runs as Tóm tắt, staff see no note", async () => {
    vi.mocked(listReports).mockResolvedValue([{ ...SUMMARY, type: "matrix" }]);
    vi.mocked(runReport).mockResolvedValue(SUMMARY_RUN);
    const rep = renderViewer("r2");
    await screen.findByRole("grid", { name: "Bảng số liệu theo nhóm" });
    expect(screen.queryByText(/trục cột/)).toBeNull();
    expect(screen.queryByText(/chạy một trục/)).toBeNull();
    rep.unmount();
    auth.user.isAdmin = true;
    renderViewer("r2");
    expect(await screen.findByText(/Báo cáo Ma trận này chưa có trục cột — đang chạy như Tóm tắt/)).toBeInTheDocument();
  });

  it("a grid result (column axis) draws the cross-tab with row and column totals (F1)", async () => {
    vi.mocked(listReports).mockResolvedValue([{ ...SUMMARY, type: "matrix" }]);
    vi.mocked(runReport).mockResolvedValue({
      type: "matrix", groups: GROUPS, rows: GROUPS,
      rowAxis: { field: "stage" }, columnAxis: { field: "close_date", bucket: "month" },
      columns: [{ key: "2026-09", value: "8", count: 8 }], cells: [{ r: 0, c: 0, value: "3", count: 3 }],
      total: { value: "8", count: 8 }, having: { on: "value", op: "gt", values: ["2"] },
    });
    renderViewer("r2");
    const pivot = await screen.findByRole("table", { name: "Bảng chéo" });
    expect(screen.queryByText(/lưới theo cột chưa hiển thị/)).toBeNull();
    expect(screen.getByText("Chỉ giữ ô có giá trị > 2.")).toBeInTheDocument();
    expect(within(pivot).getByRole("rowheader", { name: "Proposal" })).toBeInTheDocument();
    expect(within(pivot).getByRole("columnheader", { name: "2026-09" })).toBeInTheDocument();
    expect(within(pivot).getByRole("rowheader", { name: "Tổng" })).toBeInTheDocument();
    // The one-axis group table is gone: the cross-tab replaces it.
    expect(screen.queryByRole("grid", { name: "Bảng số liệu theo nhóm" })).toBeNull();
  });

  const LEVELLED: ReportRunData = {
    type: "summary", levels: [{ field: "stage" }, { field: "close_date", bucket: "month" }],
    groups: [
      { key: "Proposal", value: "3", count: 3, children: [{ key: "2026-09", value: "3", count: 3 }] },
      { key: "Qualification", value: "5", count: 5, children: [{ key: "2026-10", value: "5", count: 5 }] },
    ],
    total: { value: "8", count: 8 }, having: { on: "count", op: "gte", values: ["1"] },
  };
  const TREE_NAME = "Bảng nhóm có tổng phụ";

  it("a levelled result draws a tree grid: aria-level, level 1 open, subtotals, Tổng cộng, no 'first level only' note (F2)", async () => {
    vi.mocked(listFields).mockResolvedValue([
      { name: "stage", label: "Giai đoạn", fieldType: "select", options: { valueLabels: { Proposal: "Báo giá", Qualification: "Đánh giá" } } },
      { name: "close_date", label: "Ngày chốt", fieldType: "date" },
    ] as never);
    vi.mocked(runReport).mockResolvedValue(LEVELLED);
    renderViewer("r2");
    const tree = await screen.findByRole("treegrid", { name: TREE_NAME });
    expect(screen.queryByText(/các cấp con chưa hiển thị/)).toBeNull();
    expect(await screen.findByText("Chỉ giữ nhóm Ngày chốt có số lượng ≥ 1.")).toBeInTheDocument();
    expect(screen.getByTestId("chart-bar")).toBeInTheDocument(); // the chart keeps drawing level 1
    const rows = within(tree).getAllByRole("row").slice(1);
    // Báo giá, 2026-09, Đánh giá, 2026-10, Tổng cộng
    expect(rows.map((r) => r.getAttribute("aria-level"))).toEqual(["1", "2", "1", "2", "1"]);
    expect(within(rows[0]).getByRole("link", { name: "Báo giá" })).toHaveAttribute("href", "/crm_opportunities?stage%5Beq%5D=Proposal");
    expect(rows[4]).toHaveTextContent("Tổng cộng");
    expect(rows[4]).toHaveTextContent("8");
    expect(rows[4]).not.toHaveAttribute("aria-expanded");
  });

  it("ArrowLeft collapses a level-1 row and hides its children", async () => {
    vi.mocked(runReport).mockResolvedValue(LEVELLED);
    renderViewer("r2");
    const tree = await screen.findByRole("treegrid", { name: TREE_NAME });
    const first = within(tree).getAllByRole("row")[1];
    expect(first).toHaveAttribute("aria-expanded", "true");
    first.focus();
    await userEvent.keyboard("{ArrowLeft}");
    expect(first).toHaveAttribute("aria-expanded", "false");
    expect(within(tree).getAllByRole("row")).toHaveLength(1 + 2 + 1 + 1); // header, 2 groups, 1 child left, total
  });

  it("a one-level result with a threshold is a flat tree plus Tổng cộng (N=1)", async () => {
    vi.mocked(runReport).mockResolvedValue({
      type: "summary", levels: [{ field: "stage" }], groups: [{ key: "Proposal", value: "3", count: 3 }],
      total: { value: "3", count: 3 }, having: { on: "value", op: "gt", values: ["2"] },
    });
    renderViewer("r2");
    const tree = await screen.findByRole("treegrid", { name: TREE_NAME });
    expect(within(tree).getByRole("link", { name: "Báo giá" })).toBeInTheDocument();
    expect(within(tree).getByText("Tổng cộng")).toBeInTheDocument();
  });

  it("a threshold that removes every group says so instead of an empty scalar", async () => {
    vi.mocked(runReport).mockResolvedValue({
      type: "summary", levels: [{ field: "stage" }], groups: [], total: { value: null as unknown as string, count: 0 },
      having: { on: "value", op: "gt", values: ["1000000000"] },
    });
    renderViewer("r2");
    expect(await screen.findByText(/Không nhóm nào đạt ngưỡng/)).toBeInTheDocument();
    expect(screen.queryByRole("treegrid")).toBeNull();
  });

  it("an empty levelled result without a threshold says there is no data, not a threshold message", async () => {
    vi.mocked(runReport).mockResolvedValue({
      type: "summary", levels: [{ field: "stage" }], groups: [], total: { value: null as unknown as string, count: 0 },
    });
    renderViewer("r2");
    expect(await screen.findByText("Không có dữ liệu.")).toBeInTheDocument();
    expect(screen.queryByText(/Không nhóm nào đạt ngưỡng/)).toBeNull();
  });

  it("a one-axis group shows the server's label (customer name); drill stays on the code (SC-008)", async () => {
    vi.mocked(runReport).mockResolvedValue({
      type: "summary", groups: [{ key: "11111111-aaaa", label: "Công ty An Phát", value: "30", count: 3 }],
    });
    renderViewer("r2");
    const grid = await screen.findByRole("grid", { name: "Bảng số liệu theo nhóm" });
    const link = within(grid).getByRole("link", { name: "Công ty An Phát" });
    expect(link).toHaveAttribute("href", "/crm_opportunities?stage%5Beq%5D=11111111-aaaa");
    expect(within(grid).queryByText("11111111-aaaa")).toBeNull();
  });

  it("config.bucket: a zero-filled time series (line by default), bucket keys do not drill", async () => {
    vi.mocked(listReports).mockResolvedValue([
      { ...SUMMARY, config: { metric: "count", groupBy: "close_date", bucket: "month" } },
    ]);
    vi.mocked(runReport).mockResolvedValue({
      type: "summary",
      groups: [{ key: "2026-01", value: "2", count: 2 }, { key: "2026-03", value: "1", count: 1 }],
    });
    renderViewer("r2");
    const grid = await screen.findByRole("grid", { name: "Bảng số liệu theo nhóm" });
    expect(screen.getByText("Chuỗi thời gian")).toBeInTheDocument();
    expect(screen.getByTestId("chart-line")).toBeInTheDocument();
    expect(within(grid).getByText("Kỳ (Tháng)")).toBeInTheDocument();
    // The empty month in between is filled with 0 — three rows, not two.
    expect(within(grid).getAllByRole("row")).toHaveLength(4);
    expect(within(grid).queryByRole("link")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Cột" }));
    expect(screen.getByTestId("chart-bar")).toBeInTheDocument();
    expect(window.localStorage.getItem("nexus.report.viz.v1.r2")).toBe("bar");
  });

  it("no groups: the scalar result card", async () => {
    vi.mocked(runReport).mockResolvedValue({ type: "summary", value: "42", count: 42 });
    renderViewer("r2");
    expect(await screen.findByText("Kết quả tổng hợp")).toBeInTheDocument();
    expect(screen.getByText("42 bản ghi")).toBeInTheDocument();
  });
});

describe("ReportViewer — errors and admin", () => {
  it("FORBIDDEN gets the permission sentence, not the index hint", async () => {
    vi.mocked(runReport).mockRejectedValue(new ApiError(403, "FORBIDDEN", 'không có quyền xem entity "pm_tasks"', {}));
    renderViewer("r1");
    expect(await screen.findByText("Báo cáo không chạy được")).toBeInTheDocument();
    expect(screen.getByText(/đề nghị quản trị viên cấp quyền xem/)).toBeInTheDocument();
    expect(screen.queryByText(/đánh index/)).toBeNull();
    expect(screen.getByRole("button", { name: "Xuất CSV" })).toBeDisabled();
    // FR-024: staff never read the server's raw sentence (it names the table) nor the machine code.
    expect(screen.queryByText(/không có quyền xem entity/)).toBeNull();
    expect(screen.queryByText(/FORBIDDEN|Mã lỗi/)).toBeNull();
  });

  it("admin: the same error keeps the raw server sentence and the code for diagnosis", async () => {
    auth.user.isAdmin = true;
    vi.mocked(runReport).mockRejectedValue(new ApiError(403, "FORBIDDEN", 'không có quyền xem entity "pm_tasks"', {}));
    renderViewer("r1");
    expect(await screen.findByText(/đề nghị quản trị viên cấp quyền xem/)).toBeInTheDocument();
    expect(screen.getByText(/không có quyền xem entity "pm_tasks"/)).toBeInTheDocument();
    expect(screen.getByText(/Mã lỗi: FORBIDDEN/)).toBeInTheDocument();
  });

  it("a config error points at the groupBy index", async () => {
    vi.mocked(runReport).mockRejectedValue(new ApiError(422, "AGGREGATE_REQUIRES_INDEXED", "Trường nhóm chưa có index", {}));
    renderViewer("r2");
    expect(await screen.findByText(/cần được đánh index/)).toBeInTheDocument();
    expect(screen.getByText(/Liên hệ quản trị viên để cập nhật/)).toBeInTheDocument();
    expect(screen.queryByText(/AGGREGATE_REQUIRES_INDEXED/)).toBeNull();
    expect(screen.queryByText(/Trường nhóm chưa có index/)).toBeNull();
  });

  it("an error that is not about an index never gets the index hint", async () => {
    vi.mocked(runReport).mockRejectedValue(new ApiError(422, "REPORT_TOO_MANY_GROUPS", "quá nhiều nhóm", { cap: 1000, groups: 1001, lowerBound: true }));
    renderViewer("r2");
    expect(await screen.findByText("Báo cáo vượt giới hạn nhóm")).toBeInTheDocument();
    expect(screen.getByText(/Báo cáo có ít nhất 1\.001 nhóm, vượt giới hạn 1\.000 nhóm\./)).toBeInTheDocument();
    expect(screen.queryByText(/đánh index/)).toBeNull();
  });

  it("a Matrix with a column axis but no rows/columns says so instead of drawing a header-only table", async () => {
    vi.mocked(listReports).mockResolvedValue([{ ...SUMMARY, type: "matrix" }]);
    vi.mocked(runReport).mockResolvedValue({
      type: "matrix", groups: [], rows: [], rowAxis: { field: "stage" }, columnAxis: { field: "close_date" }, columns: [], cells: [],
    });
    renderViewer("r2");
    expect(await screen.findByText("Chưa có dữ liệu trong khoảng đã chọn.")).toBeInTheDocument();
    expect(screen.queryByRole("table", { name: "Bảng chéo" })).toBeNull();
  });

  it("export of a grid result: no 3-column client CSV (it would lie about the grid) — the hint in a toast", async () => {
    vi.mocked(listReports).mockResolvedValue([{ ...SUMMARY, type: "matrix" }]);
    vi.mocked(runReport).mockResolvedValue({
      type: "matrix", groups: GROUPS, rows: GROUPS,
      rowAxis: { field: "stage" }, columnAxis: { field: "close_date" }, columns: [{ key: "2026-01", value: "1", count: 1 }], cells: [],
    });
    vi.mocked(downloadReportCSV).mockRejectedValue(new ApiError(422, "REPORT_EXPORT_GRID_UNSUPPORTED", "raw export text", {}));
    const createObjectURL = vi.fn(() => "blob:x");
    Object.assign(URL, { createObjectURL, revokeObjectURL: vi.fn() });
    renderViewer("r2");
    await screen.findByRole("table", { name: "Bảng chéo" });
    await userEvent.click(screen.getByRole("button", { name: "Xuất CSV" }));
    await waitFor(() => expect(toast.error).toHaveBeenCalled());
    expect(vi.mocked(toast.error).mock.calls[0][0]).toMatch(/chưa xuất CSV được/);
    expect(vi.mocked(toast.error).mock.calls[0][0]).not.toMatch(/raw export text|REPORT_/);
    expect(createObjectURL).not.toHaveBeenCalled();
    expect(toast.warning).not.toHaveBeenCalled();
  });

  it("a failed tabular export still falls back, but staff read the hint, not the server's sentence", async () => {
    vi.mocked(runReport).mockResolvedValue(TABULAR_RUN);
    vi.mocked(downloadReportCSV).mockRejectedValue(new ApiError(403, "FORBIDDEN", 'không có quyền xem entity "crm_contracts"', {}));
    Object.assign(URL, { createObjectURL: vi.fn(() => "blob:x"), revokeObjectURL: vi.fn() });
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    renderViewer("r1");
    await screen.findByRole("grid", { name: "Kết quả báo cáo" });
    await userEvent.click(screen.getByRole("button", { name: "Xuất CSV" }));
    await waitFor(() => expect(toast.warning).toHaveBeenCalled());
    const msg = vi.mocked(toast.warning).mock.calls[0][0];
    expect(msg).toMatch(/đề nghị quản trị viên cấp quyền xem/);
    expect(msg).toMatch(/Đã tải bản CSV từ dữ liệu đang hiển thị/);
    expect(msg).not.toMatch(/entity|FORBIDDEN/);
    expect(click).toHaveBeenCalled();
    click.mockRestore();
  });

  it("admin: Cấu hình toggles the config panel", async () => {
    auth.user.isAdmin = true;
    vi.mocked(runReport).mockResolvedValue(TABULAR_RUN);
    renderViewer("r1");
    const btn = await screen.findByRole("button", { name: "Cấu hình" });
    expect(btn).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(btn);
    expect(screen.getByTestId("config-panel")).toHaveTextContent("r1");
    expect(btn).toHaveAttribute("aria-expanded", "true");
    await userEvent.click(btn);
    expect(screen.queryByTestId("config-panel")).toBeNull();
  });

  it("admin: ?config=1 (from the create dialog) opens the panel ready", async () => {
    auth.user.isAdmin = true;
    nav.search = "config=1";
    vi.mocked(runReport).mockResolvedValue(TABULAR_RUN);
    renderViewer("r1");
    expect(await screen.findByTestId("config-panel")).toBeInTheDocument();
  });
});

// specs/063-draft-publish S7-2 T060/T061: the builder looks at the unpublished draft on THIS screen only.
describe("ReportViewer — draft view (builder of a delivered workspace)", () => {
  const DELIVERED = { delivered: true, pendingCount: 1 } as PublishState;
  beforeEach(() => {
    auth.user.isAdmin = true;
    vi.mocked(getPublishState).mockResolvedValue(DELIVERED);
    vi.mocked(runReport).mockResolvedValue(TABULAR_RUN);
  });

  it("offers 'Xem bản nháp' only to the builder of a delivered workspace, and it turns the flag on for this screen", async () => {
    renderViewer("r1");
    await userEvent.click(await screen.findByRole("button", { name: "Xem bản nháp" }));
    expect(nav.replace).toHaveBeenCalledWith("/reports/r1?draft=1");
  });

  it("an admin who is not the builder (publish state refused) gets no draft button", async () => {
    vi.mocked(getPublishState).mockRejectedValue(new ApiError(403, "FORBIDDEN", "builder only", {}));
    renderViewer("r1");
    await screen.findByRole("grid", { name: "Kết quả báo cáo" });
    expect(screen.queryByRole("button", { name: "Xem bản nháp" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Xem bản đang chạy" })).toBeNull();
  });

  it("a staff member never calls the publish state at all", async () => {
    auth.user.isAdmin = false;
    renderViewer("r1");
    await screen.findByRole("grid", { name: "Kết quả báo cáo" });
    expect(getPublishState).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "Xem bản nháp" })).toBeNull();
  });

  it("on ?draft=1 a run answered with draftState says the customer does not see it, and 'Xem bản đang chạy' goes back", async () => {
    nav.search = "draft=1&dateFrom=2026-09-01";
    vi.mocked(runReport).mockResolvedValue({ ...TABULAR_RUN, draftState: "edited" } as ReportRunData);
    renderViewer("r1");
    expect(await screen.findByText(/Khách chưa thấy thay đổi này/)).toBeInTheDocument();
    const live = screen.getAllByRole("button", { name: "Xem bản đang chạy" });
    await userEvent.click(live[live.length - 1]);
    expect(nav.replace).toHaveBeenCalledWith("/reports/r1?dateFrom=2026-09-01");
  });

  it("a report with no draft of its own shows no banner even on the draft view", async () => {
    nav.search = "draft=1";
    vi.mocked(runReport).mockResolvedValue({ ...TABULAR_RUN, draftState: "live" } as ReportRunData);
    renderViewer("r1");
    await screen.findByRole("grid", { name: "Kết quả báo cáo" });
    expect(screen.queryByText(/Khách chưa thấy thay đổi này/)).toBeNull();
  });

  it("a draft save in the config panel switches the screen onto the draft", async () => {
    nav.search = "config=1";
    renderViewer("r1");
    await userEvent.click(await screen.findByRole("button", { name: "fake draft save" }));
    expect(nav.replace).toHaveBeenCalledWith("/reports/r1?config=1&draft=1");
  });
});

// specs/074 lát f2-report-list-view (T251): the viewer inside the Studio work surface.
describe("ReportViewer — embedded (Studio)", () => {
  function renderEmbedded(props: Partial<React.ComponentProps<typeof ReportViewer>> = {}) {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={qc}>
        <ReportViewer id="r1" embedded {...props} />
      </QueryClientProvider>,
    );
  }
  it("draws no h1 of its own (the frame owns it) and the config panel opens without ?config=1", async () => {
    auth.user.isAdmin = true;
    vi.mocked(runReport).mockResolvedValue(TABULAR_RUN);
    const onConfigure = vi.fn();
    renderEmbedded({ onConfigure });
    expect(await screen.findByRole("heading", { level: 2, name: /Danh sách hợp đồng/ })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { level: 1 })).toBeNull();
    expect(screen.queryByTestId("config-panel")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Cấu hình" }));
    expect(await screen.findByTestId("config-panel")).toHaveTextContent("r1");
    expect(onConfigure).toHaveBeenCalled();
  });
  it("a report with no rows says there is no data yet, in the maker's words", async () => {
    vi.mocked(runReport).mockResolvedValue({ type: "tabular", columns: [], rows: [], total: 0 });
    renderEmbedded();
    expect(await screen.findByText(/^Chưa có dữ liệu\. Báo cáo sẽ có số khi khách bắt đầu nhập/)).toBeInTheDocument();
  });
});
