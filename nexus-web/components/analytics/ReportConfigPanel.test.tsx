// ReportConfigPanel on SLDS (specs/038 T030) — the admin authoring form that lives in the viewer's
// docked panel. Pins the authoring rules it mirrors from the retired Studio builder: tabular columns
// via the dueling picklist (saved columns whose field is gone stay visible), summary groupBy gated
// by the physical index, sum/avg and LEFT refused over an N-N join, save → PUT /admin/reports with
// the built config, delete behind a prompt, and pin-to-dashboard appending to a FRESH read of the
// chosen dashboard (or creating a new one).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ReportConfigPanel } from "./ReportConfigPanel";
import {
  createDashboard, deleteReport, getAdminDashboard, getAdminReport, listAdminDashboards, listAllRelationships,
  updateDashboard, updateField, updateReport, type ReportSummary,
} from "@/lib/api";
import { toast } from "@/components/ui/Toaster";
import { WorkshopScopeProvider, type WorkshopScope } from "@/lib/workshopScope";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";

const nav = vi.hoisted(() => ({ push: vi.fn() }));
vi.mock("next/navigation", () => ({ useRouter: () => nav }));
const auth = vi.hoisted(() => ({ operator: true }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { isAdmin: true, isPlatformOperator: auth.operator } }) }));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    getAdminReport: vi.fn(), updateReport: vi.fn(), deleteReport: vi.fn(), listAllRelationships: vi.fn(), updateField: vi.fn(),
    listAdminDashboards: vi.fn(), getAdminDashboard: vi.fn(), updateDashboard: vi.fn(), createDashboard: vi.fn(),
  };
});

const FIELDS: Record<string, unknown[]> = {
  crm_opportunities: [
    { id: "f-name", name: "name", label: "Tên cơ hội", fieldType: "text", options: { maxLength: 200 } },
    { name: "stage", label: "Giai đoạn", fieldType: "select", options: { indexed: true } },
    { name: "amount", label: "Giá trị", fieldType: "currency" },
    { name: "account_id", label: "Khách hàng", fieldType: "relation" },
    { name: "close_date", label: "Ngày chốt", fieldType: "date", options: { indexed: true } },
    { name: "balance", label: "Còn nợ", fieldType: "formula", options: { formula: "amount - paid", resultType: "currency" } },
    { name: "tier", label: "Hạng", fieldType: "formula", options: { formula: "IF(amount > 1, \"A\", \"B\")", resultType: "text" } },
    { name: "enrolled", label: "Sĩ số", fieldType: "rollup" },
  ],
  crm_accounts: [{ name: "tier", label: "Hạng KH", fieldType: "text", options: { indexed: true } }],
  crm_regions: [
    { name: "zone", label: "Khu vực", fieldType: "text", options: { indexed: true } },
    { name: "quota", label: "Chỉ tiêu", fieldType: "number" },
  ],
  crm_products: [
    { name: "category", label: "Danh mục", fieldType: "text", options: { indexed: true } },
    { name: "on_sale", label: "Còn bán", fieldType: "boolean" },
    { name: "price", label: "Giá SP", fieldType: "number" },
  ],
};
vi.mock("@/lib/metadata/hooks", () => ({
  useEntities: () => ({ data: [{ name: "crm_opportunities", label: "Cơ hội" }, { name: "crm_contracts", label: "Hợp đồng" }] }),
  useFields: (entity: string) => ({ data: FIELDS[entity] ?? [] }),
}));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), warning: vi.fn(), error: vi.fn() } }));

const TABULAR: ReportSummary = {
  id: "r1", name: "Danh sách cơ hội", entityName: "crm_opportunities", type: "tabular",
  config: { columns: ["name", "legacy_col"] }, isActive: true,
};
const SUMMARY: ReportSummary = {
  id: "r2", name: "Pipeline", entityName: "crm_opportunities", type: "summary",
  config: { metric: "count", groupBy: "stage" }, isActive: true,
};

// specs/063 S7-2: the panel reads the report itself (GET /admin/reports/:id), so every test seeds that read.
const SCOPE: WorkshopScope = {
  slug: "crm", appLabel: "CRM", entityNames: new Set(["crm_opportunities"]),
  panelHref: (p, extra) => `/studio/apps/crm?panel=${p}${extra ? `&${new URLSearchParams(extra)}` : ""}`,
};

async function renderPanel(meta: ReportSummary, props: { onSavedToDraft?: () => void; workshop?: boolean; embedded?: boolean } = {}) {
  vi.mocked(getAdminReport).mockResolvedValue(meta);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const { workshop, embedded, ...rest } = props;
  const panel = (
    <QueryClientProvider client={qc}>
      <ReportConfigPanel reportId={meta.id} {...rest} />
    </QueryClientProvider>
  );
  const scoped = workshop ? <WorkshopScopeProvider value={SCOPE}>{panel}</WorkshopScopeProvider> : panel;
  const view = render(embedded ? <EmbeddedSurface>{scoped}</EmbeddedSurface> : scoped);
  await screen.findByRole("button", { name: "Lưu thay đổi" });
  return view;
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(listAllRelationships).mockResolvedValue([]);
  vi.mocked(updateReport).mockResolvedValue({} as ReportSummary);
});

describe("ReportConfigPanel — tabular", () => {
  it("columns are a dueling picklist; a saved column whose field is gone stays visible; save sends the order", async () => {
    await renderPanel(TABULAR);
    const group = screen.getByRole("group", { name: "Cột hiển thị" });
    const [source, target] = within(group).getAllByRole("listbox");
    expect(within(target).getAllByRole("option").map((o) => o.textContent)).toEqual(["Tên cơ hội", "legacy_col"]);
    // Relation fields are offered (the run resolves them to names — REPORT-RELATION-LABELS).
    await userEvent.click(within(source).getByRole("option", { name: /Khách hàng/ }));
    await userEvent.click(within(group).getByRole("button", { name: "Chuyển mục đang chọn sang Cột đã chọn" }));
    expect(within(target).getAllByRole("option").map((o) => o.textContent)).toEqual(["Tên cơ hội", "legacy_col", "Khách hàng"]);
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() =>
      expect(updateReport).toHaveBeenCalledWith("r1", {
        name: "Danh sách cơ hội", entityName: "crm_opportunities", type: "tabular",
        config: { columns: ["name", "legacy_col", "account_id"] }, isActive: true,
      }),
    );
    expect(toast.success).toHaveBeenCalled();
  });

  // specs/070 #264: "Trường tính" offers exactly what the engine sums — a currency formula yes, a
  // text formula / roll-up / text / picklist / relation no (they died with
  // AGGREGATE_FIELD_NOT_NUMERIC after being picked).
  it("the value field offers only summable fields, formula results included", async () => {
    await renderPanel(SUMMARY);
    const opts = within(screen.getByLabelText("Trường tính")).getAllByRole("option").map((o) => o.textContent);
    expect(opts).toEqual(["Tất cả", "Giá trị", "Còn nợ"]);
    await userEvent.selectOptions(screen.getByLabelText("Phép tính"), "sum");
    await userEvent.selectOptions(screen.getByLabelText("Trường tính"), "balance");
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateReport).toHaveBeenCalled());
    expect(vi.mocked(updateReport).mock.calls[0][1].config).toMatchObject({ metric: "sum", field: "balance", groupBy: "stage" });
  });

  it("switching to summary: groupBy gates on the index, count by default", async () => {
    await renderPanel(TABULAR);
    await userEvent.click(screen.getByLabelText("Tóm tắt"));
    const groupBy = screen.getByLabelText("Nhóm theo") as HTMLSelectElement;
    // Cannot take an index at all (a text formula) → a dead end, disabled.
    expect(within(groupBy).getByRole("option", { name: "Hạng — chưa bật tìm nhanh" })).toBeDisabled();
    expect(within(groupBy).getByRole("option", { name: "Giai đoạn" })).not.toBeDisabled();
    await userEvent.selectOptions(groupBy, "stage");
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateReport).toHaveBeenCalled());
    expect(vi.mocked(updateReport).mock.calls[0][1]).toMatchObject({ type: "summary", config: { metric: "count", groupBy: "stage" } });
  });

  // specs/070 #265: "Doanh thu theo bác sĩ / theo người phụ trách" — the engine groups by
  // relation / user / owner (their id column is always indexed); the picker used to hide relation
  // and lack the owner entirely.
  it("offers relation fields and the record owner as group-by axes", async () => {
    await renderPanel(SUMMARY);
    const groupBy = screen.getByLabelText("Nhóm theo") as HTMLSelectElement;
    expect(within(groupBy).getByRole("option", { name: "Khách hàng" })).not.toBeDisabled();
    expect(within(groupBy).getByRole("option", { name: "Người phụ trách" })).not.toBeDisabled();
    await userEvent.selectOptions(groupBy, "ownerId");
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateReport).toHaveBeenCalled());
    expect(vi.mocked(updateReport).mock.calls[0][1].config).toMatchObject({ groupBy: "ownerId" });
  });

  // …and an unindexed field that CAN take an index is fixed where the need is: pick it, press
  // "Bật tìm nhanh ngay" (the Studio edit: stored options + indexed), Save stays blocked meanwhile.
  it("picking an unindexed field offers to index it in place and blocks Save until then", async () => {
    vi.mocked(updateField).mockResolvedValue({} as never);
    await renderPanel(SUMMARY);
    await userEvent.selectOptions(screen.getByLabelText("Nhóm theo"), "name");
    const save = screen.getByRole("button", { name: "Lưu thay đổi" });
    expect(save).toBeDisabled();
    expect(save).toHaveAttribute("title", "Bật tìm nhanh cho trường đang chọn trước khi lưu.");
    expect(screen.getByText(/“Tên cơ hội” chưa bật tìm nhanh/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Bật tìm nhanh ngay" }));
    await waitFor(() => expect(updateField).toHaveBeenCalledWith("f-name", { options: { maxLength: 200, indexed: true } }));
    expect(updateReport).not.toHaveBeenCalled();
  });

  it("saved report filters survive a save untouched; a new entity drops them", async () => {
    const filters = [{ op: "eq", field: "stage", values: ["Proposal"] }];
    await renderPanel({ ...TABULAR, config: { ...TABULAR.config, filters } });
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateReport).toHaveBeenCalledTimes(1));
    expect(vi.mocked(updateReport).mock.calls[0][1]).toMatchObject({ config: { columns: ["name", "legacy_col"], filters } });
    await userEvent.selectOptions(screen.getByLabelText("Đối tượng dữ liệu"), "crm_contracts");
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateReport).toHaveBeenCalledTimes(2));
    expect(vi.mocked(updateReport).mock.calls[1][1].config).not.toHaveProperty("filters");
  });

  // specs/049 F0: the PUT replaces the whole config — a rename used to drop the saved sort, so the
  // rep's list silently re-ordered after an admin touched nothing but the name.
  it("a saved sort survives a rename + save", async () => {
    const filters = [{ op: "in", field: "stage", values: ["Proposal"] }];
    const sort = { field: "created_at", dir: "asc" };
    await renderPanel({ ...TABULAR, config: { ...TABULAR.config, filters, sort } });
    await userEvent.type(screen.getByLabelText(/Tên báo cáo/), " 2");
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateReport).toHaveBeenCalledTimes(1));
    expect(vi.mocked(updateReport).mock.calls[0][1]).toEqual({
      name: "Danh sách cơ hội 2", entityName: "crm_opportunities", type: "tabular",
      config: { columns: ["name", "legacy_col"], filters, sort }, isActive: true,
    });
  });

  it("a save error stays in the panel", async () => {
    vi.mocked(updateReport).mockRejectedValue(new Error("Tên đã tồn tại"));
    await renderPanel(TABULAR);
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    expect(await screen.findByText("Tên đã tồn tại")).toBeInTheDocument();
  });

  it("an empty name cannot be saved", async () => {
    await renderPanel(TABULAR);
    await userEvent.clear(screen.getByLabelText(/Tên báo cáo/));
    expect(screen.getByRole("button", { name: "Lưu thay đổi" })).toBeDisabled();
  });
});

// specs/070 #266: "Phiếu QUÁ HẠN theo KTV", "Đơn Thắng tháng này" need a condition saved WITH the
// report; the engine had config.filters/sort all along, the panel had no box for them.
describe("ReportConfigPanel — fixed filters and sort (specs/070 #266)", () => {
  const builder = () => screen.getByRole("heading", { name: "Bộ lọc cố định" }).closest(".slds-expression") as HTMLElement;
  const builderRows = () => Array.from(builder().querySelectorAll<HTMLElement>("li.slds-expression__row"));
  async function pick(combobox: HTMLElement, option: string) {
    await userEvent.click(combobox);
    // The panel's native <select>s hold options of the same names — pick inside this listbox.
    const listbox = document.getElementById(combobox.getAttribute("aria-controls") ?? "") as HTMLElement;
    await userEvent.click(within(listbox).getByRole("option", { name: option }));
  }

  it("shows the saved condition and saves an added 'Ngày chốt nhỏ hơn Hôm nay'", async () => {
    const filters = [{ field: "stage", op: "neq", values: ["Closed Won"] }];
    await renderPanel({ ...SUMMARY, config: { ...SUMMARY.config, filters } });
    expect(builderRows()).toHaveLength(1);
    expect(within(builderRows()[0]).getByRole("combobox", { name: "Trường" })).toHaveTextContent("Giai đoạn");
    await userEvent.click(within(builder()).getByRole("button", { name: "Thêm điều kiện" }));
    const added = builderRows()[1];
    await pick(within(added).getByRole("combobox", { name: "Trường" }), "Ngày chốt");
    await pick(within(added).getByRole("combobox", { name: "Toán tử" }), "nhỏ hơn");
    await userEvent.selectOptions(within(added).getByLabelText("Giá trị"), "today");
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateReport).toHaveBeenCalled());
    expect(vi.mocked(updateReport).mock.calls[0][1].config).toEqual({
      metric: "count", groupBy: "stage",
      filters: [{ field: "stage", op: "neq", values: ["Closed Won"] }, { field: "close_date", op: "lt", values: ["today"] }],
    });
  });

  it("a half-typed condition blocks Save and says so on its row", async () => {
    await renderPanel(SUMMARY);
    await userEvent.click(within(builder()).getByRole("button", { name: "Thêm điều kiện" }));
    const row = builderRows()[0];
    await pick(within(row).getByRole("combobox", { name: "Trường" }), "Tên cơ hội");
    const save = screen.getByRole("button", { name: "Lưu thay đổi" });
    expect(save).toBeDisabled();
    expect(save).toHaveAttribute("title", "Hoàn tất hoặc xoá điều kiện đang gõ dở trước khi lưu.");
    expect(within(row).getByText("Nhập giá trị")).toBeInTheDocument();
    await userEvent.type(within(row).getByRole("textbox", { name: "Giá trị" }), "QS-1");
    expect(save).not.toBeDisabled();
    // Deleting the condition clears the report's filters.
    await userEvent.click(within(row).getByRole("button", { name: "Xoá điều kiện 1" }));
    await userEvent.click(save);
    await waitFor(() => expect(updateReport).toHaveBeenCalled());
    expect(vi.mocked(updateReport).mock.calls[0][1].config).not.toHaveProperty("filters");
  });

  it("tabular: sort by an indexed field — descending unless asked; the direction is saved", async () => {
    await renderPanel(TABULAR);
    const sort = screen.getByLabelText("Sắp xếp theo");
    expect(within(sort).getByRole("option", { name: "Hạng — chưa bật tìm nhanh" })).toBeDisabled();
    await userEvent.selectOptions(sort, "close_date");
    await userEvent.click(screen.getByLabelText("Tăng dần"));
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateReport).toHaveBeenCalled());
    expect(vi.mocked(updateReport).mock.calls[0][1].config).toEqual({
      columns: ["name", "legacy_col"], sort: { field: "close_date", dir: "asc" },
    });
  });

  it("pinning carries the report's fixed filters onto the widget", async () => {
    vi.mocked(listAdminDashboards).mockResolvedValue([]);
    vi.mocked(createDashboard).mockResolvedValue({} as never);
    const filters = [{ field: "close_date", op: "eq", values: ["this_month"] }];
    await renderPanel({ ...SUMMARY, config: { ...SUMMARY.config, filters } });
    await userEvent.click(screen.getByRole("button", { name: "Ghim lên Trang tổng quan" }));
    const dialog = await screen.findByRole("dialog", { name: "Ghim lên Trang tổng quan" });
    await userEvent.click(await within(dialog).findByRole("button", { name: "Tạo trang tổng quan mới" }));
    await waitFor(() => expect(createDashboard).toHaveBeenCalled());
    expect(vi.mocked(createDashboard).mock.calls[0][1][0]).toMatchObject({ groupBy: "stage", filters });
  });

  // Pin ships groupBy + filters as they stand. A half-typed row would be dropped from the widget
  // (a card counting wider than the report), an axis still waiting for its index would 422
  // AGGREGATE_REQUIRES_INDEXED on every dashboard run — Pin waits for the same gates as Save.
  it("a half-typed condition blocks pinning too", async () => {
    await renderPanel(SUMMARY);
    const pin = screen.getByRole("button", { name: "Ghim lên Trang tổng quan" });
    expect(pin).not.toBeDisabled();
    await userEvent.click(within(builder()).getByRole("button", { name: "Thêm điều kiện" }));
    await pick(within(builderRows()[0]).getByRole("combobox", { name: "Trường" }), "Tên cơ hội");
    expect(pin).toBeDisabled();
    expect(pin).toHaveAttribute("title", "Hoàn tất hoặc xoá điều kiện đang gõ dở trước khi lưu.");
    await userEvent.type(within(builderRows()[0]).getByRole("textbox", { name: "Giá trị" }), "QS-1");
    expect(pin).not.toBeDisabled();
  });

  it("an axis waiting for its index blocks pinning too", async () => {
    await renderPanel(SUMMARY);
    await userEvent.selectOptions(screen.getByLabelText("Nhóm theo"), "name");
    const pin = screen.getByRole("button", { name: "Ghim lên Trang tổng quan" });
    expect(pin).toBeDisabled();
    expect(pin).toHaveAttribute("title", "Bật tìm nhanh cho trường đang chọn trước khi lưu.");
  });

  it("a tabular report keeps its stale group-by out of a pinned card until it is indexed", async () => {
    // The pinned card groups by the panel's groupBy even on a tabular report (Save ignores it there).
    await renderPanel({ ...TABULAR, config: { ...TABULAR.config, groupBy: "name" } });
    const pin = screen.getByRole("button", { name: "Ghim lên Trang tổng quan" });
    expect(screen.getByRole("button", { name: "Lưu thay đổi" })).not.toBeDisabled();
    expect(pin).toBeDisabled();
  });
});

describe("ReportConfigPanel — grid keys set outside the panel (specs/049 F0)", () => {
  const HAVING = { on: "value", op: "gt", values: ["10000"] };
  const MATRIX: ReportSummary = {
    id: "r3", name: "Giai đoạn × tháng", entityName: "crm_opportunities", type: "matrix",
    config: { metric: "count", groupBy: "stage", columnGroup: { field: "close_date", bucket: "month" }, having: HAVING },
    isActive: true,
  };

  it("they survive a save; pinning is refused with the one-axis reason", async () => {
    await renderPanel(MATRIX);
    const pin = screen.getByRole("button", { name: "Ghim lên Trang tổng quan" });
    expect(pin).toBeDisabled();
    expect(pin).toHaveAttribute("title", expect.stringMatching(/thẻ trang tổng quan chỉ có một trục/));
    expect(screen.getByText(/thẻ trang tổng quan chỉ có một trục/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateReport).toHaveBeenCalled());
    expect(vi.mocked(updateReport).mock.calls[0][1].config).toEqual(MATRIX.config);
  });

  it("matrix → Tóm tắt drops the column axis, keeps the threshold", async () => {
    await renderPanel(MATRIX);
    await userEvent.click(screen.getByLabelText("Tóm tắt"));
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateReport).toHaveBeenCalled());
    const cfg = vi.mocked(updateReport).mock.calls[0][1].config as Record<string, unknown>;
    expect(cfg).not.toHaveProperty("columnGroup");
    expect(cfg.having).toEqual(HAVING);
  });

  // specs/049 F3 (T095): the panel authors the column axis, next group level and the threshold.
  it("summary: pick next group + threshold ⇒ saved; duplicate axis blocks Save", async () => {
    await renderPanel(SUMMARY);
    await userEvent.selectOptions(screen.getByLabelText("Nhóm tiếp theo"), "close_date");
    await userEvent.type(screen.getByLabelText("Giá trị"), "500");
    expect(screen.getByText("Chỉ giữ nhóm Ngày chốt có")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateReport).toHaveBeenCalled());
    expect(vi.mocked(updateReport).mock.calls[0][1].config).toMatchObject({
      groupBy: "stage", subGroups: [{ field: "close_date" }], having: { on: "value", op: "gt", values: ["500"] },
    });
    await userEvent.selectOptions(screen.getByLabelText("Nhóm tiếp theo"), "stage");
    expect(screen.getByRole("button", { name: "Lưu thay đổi" })).toBeDisabled();
  });

  it("matrix: column axis with its own time bucket + cell threshold", async () => {
    await renderPanel({ ...SUMMARY, id: "r5", type: "matrix" });
    expect(screen.getByText(/ma trận chỉ hiện tổng theo từng hàng/)).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByLabelText("Trục cột"), "close_date");
    await userEvent.selectOptions(screen.getAllByLabelText("Gom theo thời gian").at(-1)!, "month");
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateReport).toHaveBeenCalled());
    expect(vi.mocked(updateReport).mock.calls[0][1].config).toMatchObject({ columnGroup: { field: "close_date", bucket: "month" } });
  });

  it("a plain one-axis report can still be pinned", async () => {
    await renderPanel(SUMMARY);
    expect(screen.getByRole("button", { name: "Ghim lên Trang tổng quan" })).not.toBeDisabled();
    expect(screen.queryByText(/chỉ có một trục/)).toBeNull();
  });
});

describe("ReportConfigPanel — summary with an N-N join", () => {
  beforeEach(() => {
    vi.mocked(listAllRelationships).mockResolvedValue([
      { name: "opp_products", relationshipType: "N-N", parentEntityName: "crm_opportunities", childEntityName: "crm_products" },
    ] as never);
  });

  it("over N-N: sum/avg are refused and the LEFT toggle is locked", async () => {
    await renderPanel(SUMMARY);
    const join = await screen.findByLabelText("Ghép đối tượng liên quan");
    await userEvent.selectOptions(join, "opp_products");
    const metric = screen.getByLabelText("Phép tính");
    expect(within(metric).getByRole("option", { name: "Tổng — N-N sẽ đếm trùng" })).toBeDisabled();
    expect(within(metric).getByRole("option", { name: "Đếm" })).not.toBeDisabled();
    expect(screen.getByLabelText("Giữ bản ghi không có liên kết (LEFT)")).toBeDisabled();
    // Related fields show up as "<rel>.<field>" choices, through the same gates as the base
    // pickers: the axis needs an index on the related entity, the sum target must be numeric.
    const groupBy = screen.getByLabelText("Nhóm theo");
    expect(within(groupBy).getByRole("option", { name: "Còn bán — chưa bật tìm nhanh" })).toBeDisabled();
    const valueField = screen.getByLabelText("Trường tính");
    expect(within(valueField).getByRole("option", { name: "Giá SP" })).toHaveValue("opp_products.price");
    expect(within(valueField).queryByRole("option", { name: "Danh mục" })).toBeNull();
    // Picking a related axis marks the pill; the pin button stays usable (queue #294: the card carries the joins).
    await userEvent.selectOptions(groupBy, "opp_products.category");
    expect(screen.getByText("qua opp_products")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Ghim lên Trang tổng quan" })).not.toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateReport).toHaveBeenCalled());
    expect(vi.mocked(updateReport).mock.calls[0][1]).toMatchObject({
      config: { metric: "count", groupBy: "opp_products.category", joins: [{ relationship: "opp_products" }] },
    });
  });
});

// Queue #293 + #294: a second step from the entity the first one reaches, and pinning such a report.
describe("ReportConfigPanel — two chained joins", () => {
  beforeEach(() => {
    const ALL = [
      { name: "opp_account", relationshipType: "1-N", parentEntityName: "crm_accounts", childEntityName: "crm_opportunities" },
      { name: "account_region", relationshipType: "1-N", parentEntityName: "crm_regions", childEntityName: "crm_accounts" },
    ];
    vi.mocked(listAllRelationships).mockImplementation(async (entity?: string) =>
      ALL.filter((r) => r.parentEntityName === entity || r.childEntityName === entity) as never);
  });

  it("offers a second step only after the first, lists its fields, and saves both hops in order", async () => {
    await renderPanel(SUMMARY);
    expect(screen.queryByLabelText("Ghép thêm một bước")).toBeNull();
    await userEvent.selectOptions(await screen.findByLabelText("Ghép đối tượng liên quan"), "opp_account");
    const second = await screen.findByLabelText("Ghép thêm một bước");
    // Hop 2 starts from crm_accounts: only ITS other relationship is offered (never the one just used).
    expect(within(second).getAllByRole("option").map((o) => o.getAttribute("value"))).toEqual(["", "account_region"]);
    await userEvent.selectOptions(second, "account_region");
    const groupBy = screen.getByLabelText("Nhóm theo");
    expect(within(groupBy).getByRole("option", { name: "Khu vực" })).toHaveValue("account_region.zone");
    expect(within(groupBy).getByRole("option", { name: "Hạng KH" })).toHaveValue("opp_account.tier");
    await userEvent.selectOptions(groupBy, "account_region.zone");
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateReport).toHaveBeenCalled());
    expect(vi.mocked(updateReport).mock.calls[0][1]).toMatchObject({
      config: { groupBy: "account_region.zone", joins: [{ relationship: "opp_account" }, { relationship: "account_region" }] },
    });
  });

  it("changing the first step clears the second and any axis on it", async () => {
    await renderPanel({ ...SUMMARY, config: {
      metric: "count", groupBy: "account_region.zone",
      joins: [{ relationship: "opp_account" }, { relationship: "account_region" }],
    } });
    expect(await screen.findByLabelText("Ghép thêm một bước")).toHaveValue("account_region");
    await userEvent.selectOptions(screen.getByLabelText("Ghép đối tượng liên quan"), "");
    expect(screen.queryByLabelText("Ghép thêm một bước")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateReport).toHaveBeenCalled());
    const cfg = (vi.mocked(updateReport).mock.calls[0][1] as { config: Record<string, unknown> }).config;
    expect(cfg.joins).toBeUndefined();
    expect(cfg.groupBy).toBeUndefined();
  });

  it("pinning carries both hops onto the card", async () => {
    vi.mocked(listAdminDashboards).mockResolvedValue([]);
    vi.mocked(createDashboard).mockResolvedValue({} as never);
    await renderPanel({ ...SUMMARY, config: {
      metric: "count", groupBy: "account_region.zone",
      joins: [{ relationship: "opp_account" }, { relationship: "account_region", optional: true }],
    } });
    const pin = screen.getByRole("button", { name: "Ghim lên Trang tổng quan" });
    expect(pin).not.toBeDisabled();
    await userEvent.click(pin);
    const dialog = await screen.findByRole("dialog", { name: "Ghim lên Trang tổng quan" });
    await userEvent.click(await within(dialog).findByRole("button", { name: "Tạo trang tổng quan mới" }));
    await waitFor(() => expect(createDashboard).toHaveBeenCalled());
    expect(vi.mocked(createDashboard).mock.calls[0][1][0]).toMatchObject({
      entity: "crm_opportunities", groupBy: "account_region.zone",
      joins: [{ relationship: "opp_account" }, { relationship: "account_region", optional: true }],
    });
  });
});

describe("ReportConfigPanel — delete and pin", () => {
  it("delete asks first, then removes the report and returns to the catalog", async () => {
    vi.mocked(deleteReport).mockResolvedValue();
    await renderPanel(SUMMARY);
    await userEvent.click(screen.getByRole("button", { name: "Xoá báo cáo" }));
    const prompt = await screen.findByRole("alertdialog", { name: "Xoá báo cáo?" });
    expect(within(prompt).getByText(/Pipeline/)).toBeInTheDocument();
    await userEvent.click(within(prompt).getByRole("button", { name: "Xoá báo cáo" }));
    await waitFor(() => expect(deleteReport).toHaveBeenCalledWith("r2"));
    await waitFor(() => expect(nav.push).toHaveBeenCalledWith("/reports"));
  });

  // Queue #302: inside an app workshop the way back and the index link stay in the workshop.
  it("inside a workshop, deleting returns to the app's report panel and the index link opens the app's table panel", async () => {
    vi.mocked(deleteReport).mockResolvedValue();
    await renderPanel(SUMMARY, { workshop: true });
    // Index link: the table page of the project (specs/073 kh6), not the old workshop panel.
    expect(screen.getByRole("link", { name: /tìm nhanh/i })).toHaveAttribute("href", "/studio/du-an/northwind/dung/du-lieu/crm_opportunities");
    await userEvent.click(screen.getByRole("button", { name: "Xoá báo cáo" }));
    const prompt = await screen.findByRole("alertdialog", { name: "Xoá báo cáo?" });
    await userEvent.click(within(prompt).getByRole("button", { name: "Xoá báo cáo" }));
    await waitFor(() => expect(nav.push).toHaveBeenCalledWith("/studio/apps/crm?panel=bao-cao"));
  });

  it("pin appends a widget to a fresh read of the chosen dashboard", async () => {
    vi.mocked(listAdminDashboards).mockResolvedValue([
      { id: "d1", name: "Tổng quan bán hàng", widgets: [{ id: "w0" }] },
    ] as never);
    vi.mocked(getAdminDashboard).mockResolvedValue({
      id: "d1", name: "Tổng quan bán hàng", description: "", widgets: [{ id: "w0" }, { id: "w_new_since_list" }],
    } as never);
    vi.mocked(updateDashboard).mockResolvedValue({} as never);
    await renderPanel(SUMMARY);
    await userEvent.click(screen.getByRole("button", { name: "Ghim lên Trang tổng quan" }));
    const dialog = await screen.findByRole("dialog", { name: "Ghim lên Trang tổng quan" });
    await userEvent.click(await within(dialog).findByLabelText(/Tổng quan bán hàng/));
    await userEvent.click(within(dialog).getByRole("button", { name: "Ghim vào bảng đã chọn" }));
    await waitFor(() => expect(updateDashboard).toHaveBeenCalled());
    const [id, name, widgets] = vi.mocked(updateDashboard).mock.calls[0];
    expect([id, name]).toEqual(["d1", "Tổng quan bán hàng"]);
    expect(widgets.map((w) => w.id).slice(0, 2)).toEqual(["w0", "w_new_since_list"]);
    expect(widgets[2]).toMatchObject({ type: "bar", entity: "crm_opportunities", metric: "count", groupBy: "stage", title: "Pipeline" });
    expect(createDashboard).not.toHaveBeenCalled();
  });

  it("pin can create a new dashboard instead", async () => {
    vi.mocked(listAdminDashboards).mockResolvedValue([]);
    vi.mocked(createDashboard).mockResolvedValue({} as never);
    await renderPanel(SUMMARY);
    await userEvent.click(screen.getByRole("button", { name: "Ghim lên Trang tổng quan" }));
    const dialog = await screen.findByRole("dialog", { name: "Ghim lên Trang tổng quan" });
    expect(await within(dialog).findByText(/Chưa có trang tổng quan nào/)).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: "Tạo trang tổng quan mới" }));
    await waitFor(() => expect(createDashboard).toHaveBeenCalled());
    expect(vi.mocked(createDashboard).mock.calls[0][0]).toBe("Pipeline");
  });
});

// specs/063-draft-publish S7-2 US3 #6/#7 (T060): the form's source is the report ITSELF, a save into the
// draft says so, and a second save still carries what the first one added.
describe("ReportConfigPanel — draft workspace", () => {
  it("seeds the form from getAdminReport (the draft), not from a catalog row", async () => {
    const draft: ReportSummary = { ...TABULAR, config: { columns: ["name", "stage"] }, draftState: "edited" };
    await renderPanel(draft);
    expect(getAdminReport).toHaveBeenCalledWith("r1");
    const group = screen.getByRole("group", { name: "Cột hiển thị" });
    expect(within(group).getAllByText(/Giai đoạn/).length).toBeGreaterThan(0);
    // an already-draft report says so before any save
    expect(screen.getByText("Đã lưu vào bản nháp · Khách chưa thấy")).toBeInTheDocument();
  });

  it("a save answered with draftState says 'khách chưa thấy' and hands the viewer over to the draft", async () => {
    const onSavedToDraft = vi.fn();
    vi.mocked(updateReport).mockResolvedValue({ ...TABULAR, draftState: "edited" });
    await renderPanel(TABULAR, { onSavedToDraft });
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(onSavedToDraft).toHaveBeenCalledTimes(1));
    expect(toast.success).toHaveBeenCalledWith("Đã lưu vào bản nháp · Khách chưa thấy");
    expect(screen.getByText("Đã lưu vào bản nháp · Khách chưa thấy")).toBeInTheDocument();
  });

  it("a live workspace save keeps the plain toast and does not switch the viewer", async () => {
    const onSavedToDraft = vi.fn();
    await renderPanel(TABULAR, { onSavedToDraft }); // updateReport resolves {} — no draftState
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(toast.success).toHaveBeenCalled());
    expect(onSavedToDraft).not.toHaveBeenCalled();
    expect(toast.success).not.toHaveBeenCalledWith("Đã lưu vào bản nháp · Khách chưa thấy");
  });

  it("a second save carries the column the first draft added (the form was seeded from the draft)", async () => {
    // Reopen after a draft save: getAdminReport answers the draft config with the NEW column.
    await renderPanel({ ...TABULAR, config: { columns: ["name", "stage"] }, draftState: "edited" });
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateReport).toHaveBeenCalled());
    const cfg = vi.mocked(updateReport).mock.calls[0][1].config as { columns: string[] };
    expect(cfg.columns).toEqual(["name", "stage"]);
  });
});

describe("ReportConfigPanel — the way into Studio", () => {
  it("shows the index link to the builder only: a customer admin has no Studio to open", async () => {
    auth.operator = false;
    try {
      await renderPanel(SUMMARY);
      expect(screen.queryByRole("link", { name: /tìm nhanh/i })).toBeNull();
    } finally {
      auth.operator = true;
    }
  });
});

// specs/074 lát f2-report-config-dashboard T266: inside the Studio frame nothing opens a dialog; outside it the old ones stay.
describe("ReportConfigPanel — embedded in the Studio frame", () => {
  it("delete asks in place (no dialog), then returns to the project's Báo cáo page", async () => {
    vi.mocked(deleteReport).mockResolvedValue();
    await renderPanel(SUMMARY, { embedded: true });
    await userEvent.click(screen.getByRole("button", { name: "Xoá báo cáo" }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(deleteReport).not.toHaveBeenCalled();
    expect(screen.getByText(/Pipeline/)).toBeInTheDocument();
    const buttons = screen.getAllByRole("button", { name: "Xoá báo cáo" });
    await userEvent.click(buttons[buttons.length - 1]);
    await waitFor(() => expect(deleteReport).toHaveBeenCalledWith("r2"));
    await waitFor(() => expect(nav.push).toHaveBeenCalledWith(expect.stringMatching(/^\/studio\/du-an\/[a-z0-9-]+\/dung\/bao-cao$/)));
  });

  it("pin picks a dashboard in place (no dialog) and appends to it", async () => {
    vi.mocked(listAdminDashboards).mockResolvedValue([{ id: "d1", name: "Tổng quan bán hàng", widgets: [{ id: "w0" }] }] as never);
    vi.mocked(getAdminDashboard).mockResolvedValue({ id: "d1", name: "Tổng quan bán hàng", description: "", widgets: [{ id: "w0" }] } as never);
    vi.mocked(updateDashboard).mockResolvedValue({} as never);
    await renderPanel(SUMMARY, { embedded: true });
    await userEvent.click(screen.getByRole("button", { name: "Ghim lên Trang tổng quan" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    await userEvent.click(await screen.findByLabelText(/Tổng quan bán hàng/));
    await userEvent.click(screen.getByRole("button", { name: "Ghim vào bảng đã chọn" }));
    await waitFor(() => expect(updateDashboard).toHaveBeenCalled());
    expect(vi.mocked(updateDashboard).mock.calls[0][0]).toBe("d1");
  });
});
