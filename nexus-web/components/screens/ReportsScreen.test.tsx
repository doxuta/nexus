// ReportsScreen (/reports) on SLDS — ObjectHomePage (page-headers/object-home) + DataTable +
// Modal (specs/038 T030). Pins what the catalog must still let each persona do: read the list
// (name → viewer, entity, type, status), narrow it by type (list-view switcher) and by search,
// and — admin only — create a report through the minimal dialog that lands on the viewer with the
// config panel open (?config=1).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ReportsScreen } from "./ReportsScreen";
import { ApiError, createReport, getPublishState, listFields, listReports, type ReportSummary } from "@/lib/api";

const auth = vi.hoisted(() => ({ user: { id: "u1", fullName: "Trần Sales Rep", email: "test@example.com", isAdmin: false } }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => auth }));

const nav = vi.hoisted(() => ({ push: vi.fn(), replace: vi.fn(), search: "" }));
vi.mock("next/navigation", () => ({
  useRouter: () => nav,
  usePathname: () => "/reports",
  useSearchParams: () => new URLSearchParams(nav.search),
}));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, listReports: vi.fn(), createReport: vi.fn(), listFields: vi.fn(), getPublishState: vi.fn() };
});

const meta = vi.hoisted(() => ({
  entities: [
    { name: "crm_contracts", label: "Hợp đồng" },
    { name: "crm_opportunities", label: "Cơ hội" },
  ],
}));
vi.mock("@/lib/metadata/hooks", () => ({ useEntities: () => ({ data: meta.entities }) }));

const REPORTS: ReportSummary[] = [
  { id: "r1", name: "Danh sách hợp đồng hiệu lực", entityName: "crm_contracts", type: "tabular", isActive: true },
  { id: "r2", name: "Pipeline theo giai đoạn", entityName: "crm_opportunities", type: "summary", isActive: false },
];

function renderList(entity?: string, clearEntityHref?: string, onOpen?: (id: string) => void) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ReportsScreen entity={entity} clearEntityHref={clearEntityHref} onOpen={onOpen} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  auth.user.isAdmin = false;
  nav.search = "";
  vi.mocked(listReports).mockResolvedValue(REPORTS);
  vi.mocked(getPublishState).mockRejectedValue(new ApiError(403, "FORBIDDEN", "builder only", {}));
});

describe("ReportsScreen", () => {
  it("lists reports in the SLDS grid: name links to the viewer, entity label, type, status", async () => {
    renderList();
    const grid = await screen.findByRole("grid", { name: "Danh sách báo cáo" });
    const link = await within(grid).findByRole("link", { name: "Danh sách hợp đồng hiệu lực" });
    expect(link).toHaveAttribute("href", "/reports/r1");
    expect(within(grid).getByText("Hợp đồng")).toBeInTheDocument();
    expect(within(grid).getByText("Bảng chi tiết")).toBeInTheDocument();
    expect(within(grid).getByText("Tóm tắt")).toBeInTheDocument();
    expect(within(grid).getByText("Hoạt động")).toBeInTheDocument();
    expect(within(grid).getByText("Tắt")).toBeInTheDocument();
    expect(screen.getByText("2 báo cáo")).toBeInTheDocument();
    // Rep: no authoring entry point.
    expect(screen.queryByRole("button", { name: "Tạo báo cáo" })).toBeNull();
  });

  it("search narrows by name or entity", async () => {
    renderList();
    await screen.findByRole("grid", { name: "Danh sách báo cáo" });
    await userEvent.type(screen.getByPlaceholderText("Tìm báo cáo…"), "pipeline");
    const grid = screen.getByRole("grid", { name: "Danh sách báo cáo" });
    expect(within(grid).queryByText("Danh sách hợp đồng hiệu lực")).toBeNull();
    expect(within(grid).getByText("Pipeline theo giai đoạn")).toBeInTheDocument();
    expect(screen.getByText("1 báo cáo")).toBeInTheDocument();
  });

  it("the list-view switcher filters by report type", async () => {
    renderList();
    await screen.findByRole("grid", { name: "Danh sách báo cáo" });
    screen.getByRole("button", { name: "Chọn loại báo cáo" }).focus();
    await userEvent.keyboard("{Enter}");
    await userEvent.click(await screen.findByRole("menuitemcheckbox", { name: "Tóm tắt" }));
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Tóm tắt");
    const grid = screen.getByRole("grid", { name: "Danh sách báo cáo" });
    expect(within(grid).queryByText("Danh sách hợp đồng hiệu lực")).toBeNull();
    expect(within(grid).getByText("Pipeline theo giai đoạn")).toBeInTheDocument();
  });

  it("empty catalog: the rep is told who creates reports", async () => {
    vi.mocked(listReports).mockResolvedValue([]);
    renderList();
    expect(await screen.findByText("Chưa có báo cáo nào")).toBeInTheDocument();
    expect(screen.getByText(/Liên hệ quản trị viên để được cấp/)).toBeInTheDocument();
  });

  it("a failed catalog read says so", async () => {
    vi.mocked(listReports).mockRejectedValue(new Error("mất kết nối"));
    renderList();
    expect(await screen.findByText("mất kết nối")).toBeInTheDocument();
  });

  it("specs/074: with onOpen the host opens the new report, the router is not touched; the customer face still wraps the form in a Modal", async () => {
    auth.user.isAdmin = true;
    vi.mocked(listFields).mockResolvedValue([{ name: "contract_no", label: "Số hợp đồng", fieldType: "text" }] as never);
    vi.mocked(createReport).mockResolvedValue({ id: "new1", name: "Doanh thu" } as ReportSummary);
    const onOpen = vi.fn();
    renderList(undefined, undefined, onOpen);
    await screen.findByRole("grid", { name: "Danh sách báo cáo" });
    await userEvent.click(screen.getByRole("button", { name: "Tạo báo cáo" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText("Tên báo cáo"), "Doanh thu");
    await userEvent.click(within(dialog).getByRole("button", { name: "Tạo & mở cấu hình" }));
    await waitFor(() => expect(onOpen).toHaveBeenCalledWith("new1"));
    expect(nav.push).not.toHaveBeenCalled();
  });

  it("admin creates a tabular report and lands on the viewer with the config panel open", async () => {
    auth.user.isAdmin = true;
    vi.mocked(listFields).mockResolvedValue([{ name: "contract_no", label: "Số hợp đồng", fieldType: "text" }] as never);
    vi.mocked(createReport).mockResolvedValue({ id: "new1", name: "Doanh thu" } as ReportSummary);
    renderList();
    await screen.findByRole("grid", { name: "Danh sách báo cáo" });
    await userEvent.click(screen.getByRole("button", { name: "Tạo báo cáo" }));
    const dialog = await screen.findByRole("dialog");
    const submit = within(dialog).getByRole("button", { name: "Tạo & mở cấu hình" });
    expect(submit).toBeDisabled(); // no name yet
    await userEvent.type(within(dialog).getByLabelText("Tên báo cáo"), "Doanh thu");
    await userEvent.selectOptions(within(dialog).getByLabelText("Đối tượng dữ liệu"), "crm_opportunities");
    await userEvent.click(submit);
    await waitFor(() => expect(nav.push).toHaveBeenCalledWith("/reports/new1?config=1"));
    expect(listFields).toHaveBeenCalledWith("crm_opportunities");
    expect(createReport).toHaveBeenCalledWith(
      expect.objectContaining({
        name: "Doanh thu",
        entityName: "crm_opportunities",
        type: "tabular",
        isActive: true,
        config: expect.objectContaining({ columns: ["contract_no"] }),
      }),
    );
  });

  it("?bang= (entity prop) lists only that table's reports, says so, offers the way back, and preselects it on create", async () => {
    auth.user.isAdmin = true;
    renderList("crm_opportunities", "/studio/du-an/acme/dung/bao-cao");
    const grid = await screen.findByRole("grid", { name: "Danh sách báo cáo" });
    expect(await within(grid).findByText("Pipeline theo giai đoạn")).toBeInTheDocument();
    expect(within(grid).queryByText("Danh sách hợp đồng hiệu lực")).toBeNull();
    expect(screen.getByText(/Đang chỉ hiện báo cáo của bảng Cơ hội/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Xem tất cả báo cáo" })).toHaveAttribute("href", "/studio/du-an/acme/dung/bao-cao");
    await userEvent.click(screen.getAllByRole("button", { name: "Tạo báo cáo" })[0]);
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByLabelText("Đối tượng dữ liệu")).toHaveValue("crm_opportunities");
  });

  it("a table with no reports says so (not the generic empty) and still offers Tạo", async () => {
    auth.user.isAdmin = true;
    vi.mocked(listReports).mockResolvedValue([REPORTS[0]]);
    renderList("crm_opportunities");
    expect(await screen.findByText("Bảng này chưa có báo cáo nào")).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "Tạo báo cáo" }).length).toBeGreaterThan(0);
  });

  it("a create error stays in the dialog", async () => {
    auth.user.isAdmin = true;
    vi.mocked(listFields).mockResolvedValue([] as never);
    vi.mocked(createReport).mockRejectedValue(new Error("Tên đã tồn tại"));
    renderList();
    await screen.findByRole("grid", { name: "Danh sách báo cáo" });
    await userEvent.click(screen.getByRole("button", { name: "Tạo báo cáo" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText("Tên báo cáo"), "Trùng{Enter}");
    expect(await within(dialog).findByText("Tên đã tồn tại")).toBeInTheDocument();
    expect(nav.push).not.toHaveBeenCalled();
  });
});

// specs/063-draft-publish S7-2 T061: the builder lists the catalog over the draft, per screen.
describe("ReportsScreen — draft view", () => {
  it("on ?draft=1 a draft-only report is listed with its badge and every link keeps the draft view", async () => {
    auth.user.isAdmin = true;
    nav.search = "draft=1";
    vi.mocked(getPublishState).mockResolvedValue({ delivered: true } as never);
    vi.mocked(listReports).mockResolvedValue([...REPORTS, { id: "rn", name: "Chỉ có trong nháp", entityName: "crm_contracts", type: "tabular", isActive: true, draftState: "new" }]);
    renderList();
    const link = await screen.findByRole("link", { name: "Chỉ có trong nháp" });
    expect(link).toHaveAttribute("href", "/reports/rn?draft=1");
    expect(screen.getByText("Mới")).toBeInTheDocument();
    expect(await screen.findByRole("button", { name: "Xem bản đang chạy" })).toBeInTheDocument();
  });

  it("off the draft view the links are plain and the builder is offered 'Xem bản nháp'", async () => {
    auth.user.isAdmin = true;
    vi.mocked(getPublishState).mockResolvedValue({ delivered: true } as never);
    renderList();
    const link = await screen.findByRole("link", { name: REPORTS[0].name });
    expect(link).toHaveAttribute("href", `/reports/${REPORTS[0].id}`);
    await userEvent.click(await screen.findByRole("button", { name: "Xem bản nháp" }));
    expect(nav.replace).toHaveBeenCalledWith("/reports?draft=1");
  });

  it("a staff member sees no draft control", async () => {
    renderList();
    await screen.findByRole("link", { name: REPORTS[0].name });
    expect(screen.queryByRole("button", { name: /bản nháp|bản đang chạy/ })).toBeNull();
    expect(getPublishState).not.toHaveBeenCalled();
  });
});