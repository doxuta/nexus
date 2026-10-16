// EntityListPage (specs/074 lát f2-data-list, T014): the table list of a Studio project. Cases that DataModelBuilder.locked
// and DataModelBuilder tests held (form in place, Esc/focus, app choice, undo, failed create) are carried over.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, type AppSummary, type EntityMeta } from "@/lib/api";

const push = vi.fn();
const replace = vi.fn();
vi.mock("next/navigation", () => ({
  usePathname: () => "/studio/du-an/northwind/dung/du-lieu",
  useRouter: () => ({ push, replace, back: vi.fn(), forward: vi.fn(), refresh: vi.fn(), prefetch: vi.fn() }),
}));
const { toast } = vi.hoisted(() => ({ toast: { success: vi.fn(), info: vi.fn(), error: vi.fn(), withAction: vi.fn() } }));
vi.mock("@/components/ui/Toaster", () => ({ toast }));

const app = (name: string): AppSummary => ({ id: `id-${name}`, name, label: name.toUpperCase(), icon: null, color: null, isDefault: false, sortOrder: 0 });
const ent = (name: string, label: string): EntityMeta => ({ id: `e-${name}`, name, label, tableName: `obj_${name}` });
const entities = [ent("crm_accounts", "Khách hàng"), ent("pm_tasks", "Công việc")];
const made = ent("don_hang", "Đơn hàng");

const listEntities = vi.fn();
const appsCatalog = vi.fn();
const appNav = vi.fn();
const listFields = vi.fn();
const listAllRelationships = vi.fn();
const listWorkflows = vi.fn();
const createEntityWithAcl = vi.fn();
const adminApp = vi.fn();
const replaceAppNav = vi.fn();
const listRecords = vi.fn();
const getPublishState = vi.fn();
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  listEntities: (...a: unknown[]) => listEntities(...a),
  appsCatalog: (...a: unknown[]) => appsCatalog(...a),
  appNav: (...a: unknown[]) => appNav(...a),
  listFields: (...a: unknown[]) => listFields(...a),
  listAllRelationships: (...a: unknown[]) => listAllRelationships(...a),
  listWorkflows: (...a: unknown[]) => listWorkflows(...a),
  createEntityWithAcl: (...a: unknown[]) => createEntityWithAcl(...a),
  adminApp: (...a: unknown[]) => adminApp(...a),
  replaceAppNav: (...a: unknown[]) => replaceAppNav(...a),
  listRecords: (...a: unknown[]) => listRecords(...a),
  getPublishState: (...a: unknown[]) => getPublishState(...a),
}));

// Identity open (the token belongs to the project) so the usage read can run; getUsage is the one source of "Bản ghi".
const getUsage = vi.fn();
vi.mock("@/lib/admin", async (importOriginal) => ({ ...(await importOriginal<typeof import("@/lib/admin")>()), getUsage: (...a: unknown[]) => getUsage(...a) }));
vi.mock("@/lib/studioContext", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/studioContext")>()),
  useStudioIdentity: () => ({ status: "ready", matches: () => true }),
}));
const usage = (records: { entity: string; label: string; count: number }[]) => ({ current: { records } });

const undoDraftDelete = vi.fn();
vi.mock("@/lib/draftUndo", () => ({ undoDraftDelete: (...a: unknown[]) => undoDraftDelete(...a) }));

import { EntityListPage } from "./EntityListPage";

function renderPage(props: { app?: string } = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <EntityListPage slug="northwind" {...props} />
    </QueryClientProvider>,
  );
}
const navOf = (...names: string[]) => ({ app: app("x"), items: names.map((n) => ({ type: "entity", refId: n, label: n, href: `/${n}` })) });

async function openForm(wait = "Khách hàng") {
  await screen.findByRole("link", { name: wait });
  fireEvent.click(screen.getByRole("button", { name: "+ Bảng mới" }));
  return screen.findByRole("form", { name: "+ Bảng mới" });
}

beforeEach(() => {
  vi.clearAllMocks();
  getUsage.mockRejectedValue(new ApiError(404, "NOT_FOUND", "Not Found"));
  listEntities.mockResolvedValue(entities);
  appsCatalog.mockResolvedValue([app("crm"), app("ticket")]);
  appNav.mockImplementation(async (n: string) => (n === "crm" ? navOf("crm_accounts") : navOf("pm_tasks")));
  listFields.mockImplementation(async (e: string) => (e === "crm_accounts" ? [{ name: "a" }, { name: "b" }, { name: "c" }] : [{ name: "x" }, { name: "y" }]));
  listAllRelationships.mockResolvedValue([{ id: "r1", name: "r", parentEntityName: "crm_accounts", childEntityName: "pm_tasks", relationshipType: "1-N" }]);
  listWorkflows.mockResolvedValue([{ id: "w1", name: "w", entityName: "pm_tasks" }]);
  createEntityWithAcl.mockResolvedValue({ entity: made, aclSeed: { appScoped: true, seeded: true } });
  adminApp.mockResolvedValue({ items: [] });
  replaceAppNav.mockResolvedValue(undefined);
  getPublishState.mockResolvedValue({ delivered: false });
});

describe("EntityListPage: states", () => {
  it("loading: a spinner first, then the rows; cells of a slow source show '…' while the others are filled", async () => {
    listWorkflows.mockReturnValue(new Promise(() => {}));
    renderPage();
    expect(document.querySelector(".slds-spinner")).not.toBeNull();
    const row = (await screen.findByRole("link", { name: "Khách hàng" })).closest("tr") as HTMLElement;
    await waitFor(() => expect(within(row).getByText("3")).toBeInTheDocument()); // Trường
    expect(within(row).getAllByText("…").length).toBeGreaterThan(0); // Luật still loading
  });

  it("data: columns Bảng · Trường · Liên kết tới · Luật · App, with real values", async () => {
    renderPage();
    const lk = await screen.findByRole("link", { name: "Công việc" });
    const row = lk.closest("tr") as HTMLElement;
    await waitFor(() => expect(within(row).getByText("TICKET")).toBeInTheDocument());
    const heads = screen.getAllByRole("columnheader").map((h) => h.textContent ?? "");
    for (const h of ["Bảng", "Trường", "Liên kết tới", "Luật", "App"]) expect(heads.some((x) => x.includes(h))).toBe(true);
    expect(within(row).getByText("2")).toBeInTheDocument(); // fields
    expect(within(row).getByText("1")).toBeInTheDocument(); // rules
    expect(within(row).getByText("Khách hàng")).toBeInTheDocument(); // links to
    expect(listRecords).not.toHaveBeenCalled();
  });

  it("error: the sentence and 'Thử lại' that loads again", async () => {
    listEntities.mockRejectedValueOnce(new ApiError(500, "INTERNAL", "boom"));
    renderPage();
    expect(await screen.findByText(/Không tải được danh sách bảng\./)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Thử lại" }));
    expect(await screen.findByRole("link", { name: "Khách hàng" })).toBeInTheDocument();
  });

  it("empty project: 'Chưa có bảng nào', one primary button, no glyph", async () => {
    listEntities.mockResolvedValue([]);
    renderPage();
    expect(await screen.findByRole("heading", { name: "Chưa có bảng nào" })).toBeInTheDocument();
    expect(document.querySelector("svg.slds-illustration__svg")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "+ Thêm bảng đầu tiên" }));
    expect(await screen.findByRole("form", { name: "+ Bảng mới" })).toBeInTheDocument();
  });

  it("?app= with no tables: 'App {tên} chưa có bảng nào' with '+ Bảng mới' and a text link to see all", async () => {
    appNav.mockResolvedValue(navOf());
    renderPage({ app: "ticket" });
    expect(await screen.findByRole("heading", { name: "App TICKET chưa có bảng nào" })).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "+ Bảng mới" })).toHaveLength(1);
    expect(screen.getByRole("link", { name: "Xem tất cả" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/du-lieu");
  });

  it("?app= unknown: says so and offers all tables, never a silent empty list", async () => {
    renderPage({ app: "ma_la" });
    expect(await screen.findByRole("heading", { name: "Không có app ma_la" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Xem tất cả" })).toBeInTheDocument();
    expect(appNav).not.toHaveBeenCalledWith("ma_la");
  });

  it("?app= filters to that app's tables and says how many are hidden", async () => {
    renderPage({ app: "crm" });
    expect(await screen.findByRole("link", { name: "Khách hàng" })).toBeInTheDocument();
    expect(screen.queryByText("Công việc")).not.toBeInTheDocument();
    expect(screen.getByText(/1 bảng của app khác đang ẩn/)).toBeInTheDocument();
  });

  it("search: filters by label or code; no match says so", async () => {
    renderPage();
    await screen.findByRole("link", { name: "Khách hàng" });
    const box = screen.getByRole("searchbox");
    fireEvent.change(box, { target: { value: "pm_t" } });
    expect(screen.queryByRole("link", { name: "Khách hàng" })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Công việc" })).toBeInTheDocument();
    fireEvent.change(box, { target: { value: "zzz" } });
    expect(screen.getByRole("heading", { name: "Không có bảng nào khớp “zzz”" })).toBeInTheDocument();
  });

  it("a table name links through studioHref and clicking the row opens it", async () => {
    renderPage();
    const link = await screen.findByRole("link", { name: "Khách hàng" });
    expect(link).toHaveAttribute("href", "/studio/du-an/northwind/dung/du-lieu/crm_accounts");
    fireEvent.click((await screen.findByRole("link", { name: "Công việc" })).closest("tr") as HTMLElement);
    expect(push.mock.calls.at(-1)?.[0]).toBe("/studio/du-an/northwind/dung/du-lieu/pm_tasks");
  });
});

describe("EntityListPage: same label twice", () => {
  it("a link target whose label another table shares is shown with its code", async () => {
    listEntities.mockResolvedValue([ent("crm_accounts", "Khách hàng"), ent("tk_customers", "Khách hàng"), ent("pm_tasks", "Công việc")]);
    listAllRelationships.mockResolvedValue([{ id: "r1", name: "r", parentEntityName: "tk_customers", childEntityName: "pm_tasks", relationshipType: "1-N" }]);
    renderPage();
    const row = (await screen.findByRole("link", { name: "Công việc" })).closest("tr") as HTMLElement;
    expect(await within(row).findByText("Khách hàng (tk_customers)")).toBeInTheDocument();
  });
});

describe("EntityListPage: Bảng mới", () => {
  it("opens a form in place (no dialog), focus on the first box; Esc closes it and returns focus to the button", async () => {
    renderPage();
    const form = await openForm();
    expect(document.querySelectorAll('[role="dialog"]')).toHaveLength(0);
    expect(within(form).getByLabelText(/Tên hiển thị/)).toHaveFocus();
    fireEvent.keyDown(within(form).getByLabelText(/Tên hiển thị/), { key: "Escape" });
    expect(screen.queryByRole("form", { name: "+ Bảng mới" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "+ Bảng mới" })).toHaveFocus();
  });

  it("the code follows the name until it is edited by hand, then stops", async () => {
    renderPage();
    const form = await openForm();
    const label = within(form).getByLabelText(/Tên hiển thị/);
    const code = within(form).getByLabelText(/Mã bảng/);
    fireEvent.change(label, { target: { value: "Đơn hàng" } });
    expect(code).toHaveValue("don_hang");
    fireEvent.change(code, { target: { value: "dh" } });
    fireEvent.change(label, { target: { value: "Đơn hàng mới" } });
    expect(code).toHaveValue("dh");
  });

  it("several apps and none chosen: 'Tạo bảng' is off with the reason, nothing is created", async () => {
    renderPage();
    const form = await openForm();
    fireEvent.change(within(form).getByLabelText(/Tên hiển thị/), { target: { value: "Đơn hàng" } });
    expect(within(form).getByRole("button", { name: "Tạo bảng" })).toBeDisabled();
    expect(within(form).getByText(/Chọn app để nhân viên thấy bảng này/)).toBeInTheDocument();
    fireEvent.submit(form);
    expect(createEntityWithAcl).not.toHaveBeenCalled();
  });

  it("exactly one app is chosen already; ?app= is preselected among several", async () => {
    appsCatalog.mockResolvedValue([app("crm")]);
    const one = renderPage();
    expect(await within(await openForm()).findByLabelText(/Thuộc app/)).toHaveValue("id-crm");
    one.unmount();
    appsCatalog.mockResolvedValue([app("crm"), app("ticket")]);
    renderPage({ app: "ticket" });
    expect(await within(await openForm("Công việc")).findByLabelText(/Thuộc app/)).toHaveValue("id-ticket");
  });

  it("creates in the chosen app: appId sent, table put in that menu, toast with Hoàn tác, then opens the table with ?moi=1", async () => {
    renderPage();
    const form = await openForm();
    fireEvent.change(within(form).getByLabelText(/Tên hiển thị/), { target: { value: "Đơn hàng" } });
    fireEvent.change(within(form).getByLabelText(/Thuộc app/), { target: { value: "id-ticket" } });
    fireEvent.click(within(form).getByRole("button", { name: "Tạo bảng" }));
    await waitFor(() => expect(createEntityWithAcl).toHaveBeenCalledWith("don_hang", "Đơn hàng", "", [], "id-ticket"));
    await waitFor(() => expect(replaceAppNav).toHaveBeenCalledWith("id-ticket", [{ itemType: "entity", refId: "e-don_hang" }]));
    await waitFor(() => expect(push.mock.calls.at(-1)?.[0]).toBe("/studio/du-an/northwind/dung/du-lieu/don_hang?moi=1"));
    expect(toast.withAction).toHaveBeenCalledTimes(1);
    expect(toast.withAction.mock.calls[0][0]).toBe("Đã thêm Đơn hàng vào menu TICKET");
    expect(document.querySelectorAll('[role="dialog"]')).toHaveLength(0);
  });

  it("Hoàn tác takes the table out of the menu and leaves the table", async () => {
    renderPage({ app: "crm" });
    const form = await openForm();
    fireEvent.change(within(form).getByLabelText(/Tên hiển thị/), { target: { value: "Đơn hàng" } });
    fireEvent.click(within(form).getByRole("button", { name: "Tạo bảng" }));
    await waitFor(() => expect(toast.withAction).toHaveBeenCalled());
    adminApp.mockResolvedValue({ items: [{ itemType: "entity", refId: "e-crm_accounts" }, { itemType: "entity", refId: "e-don_hang" }] });
    replaceAppNav.mockClear();
    toast.withAction.mock.calls[0][1].onClick();
    await waitFor(() => expect(replaceAppNav).toHaveBeenCalledWith("id-crm", [{ itemType: "entity", refId: "e-crm_accounts" }]));
    await waitFor(() => expect(toast.info).toHaveBeenCalled());
  });

  it("'Không thêm vào app nào' creates the table and shows a warning that stays on screen (no navigation)", async () => {
    createEntityWithAcl.mockResolvedValue({ entity: made, aclSeed: { appScoped: false, seeded: false } });
    renderPage();
    const form = await openForm();
    fireEvent.change(within(form).getByLabelText(/Tên hiển thị/), { target: { value: "Đơn hàng" } });
    fireEvent.change(within(form).getByLabelText(/Thuộc app/), { target: { value: "-" } });
    fireEvent.click(within(form).getByRole("button", { name: "Tạo bảng" }));
    expect(await screen.findByText(/Bảng Đơn hàng chưa thuộc app nào/)).toBeInTheDocument();
    expect(createEntityWithAcl).toHaveBeenCalledWith("don_hang", "Đơn hàng", "", [], undefined);
    expect(adminApp).not.toHaveBeenCalled();
    expect(push).not.toHaveBeenCalled();
  });

  it("aclSeed.warning from the server is shown, not swallowed", async () => {
    createEntityWithAcl.mockResolvedValue({ entity: made, aclSeed: { appScoped: true, seeded: false, warning: "Chưa cấp được quyền." } });
    renderPage({ app: "crm" });
    const form = await openForm();
    fireEvent.change(within(form).getByLabelText(/Tên hiển thị/), { target: { value: "Đơn hàng" } });
    fireEvent.click(within(form).getByRole("button", { name: "Tạo bảng" }));
    expect(await screen.findByText("Chưa cấp được quyền.")).toBeInTheDocument();
    expect(push).not.toHaveBeenCalled();
  });

  it("the menu write fails: the table is created and the page says the menu is missing it", async () => {
    replaceAppNav.mockRejectedValue(new Error("nav down"));
    renderPage({ app: "crm" });
    const form = await openForm();
    fireEvent.change(within(form).getByLabelText(/Tên hiển thị/), { target: { value: "Đơn hàng" } });
    fireEvent.click(within(form).getByRole("button", { name: "Tạo bảng" }));
    expect(await screen.findByText(/chưa thêm được vào menu app/)).toBeInTheDocument();
    expect(push).not.toHaveBeenCalled();
  });

  it("409: the error sits on the code box; other errors sit in the form", async () => {
    createEntityWithAcl.mockRejectedValueOnce(new ApiError(409, "ENTITY_EXISTS", "Mã bảng đã có."));
    renderPage({ app: "crm" });
    const form = await openForm();
    fireEvent.change(within(form).getByLabelText(/Tên hiển thị/), { target: { value: "Đơn hàng" } });
    fireEvent.click(within(form).getByRole("button", { name: "Tạo bảng" }));
    const code = await within(form).findByLabelText(/Mã bảng/);
    await waitFor(() => expect(code).toHaveAttribute("aria-invalid", "true"));
    expect(within(form).queryByLabelText(/Tên hiển thị/)).not.toHaveAttribute("aria-invalid", "true");
  });

  it("no app in the project: the form still opens, 'Không thêm' is chosen and the page says why", async () => {
    appsCatalog.mockResolvedValue([]);
    renderPage();
    const form = await openForm();
    await waitFor(() => expect(within(form).getByLabelText(/Thuộc app/)).toHaveValue("-"));
    expect(screen.getByText(/Dự án chưa có app nào/)).toBeInTheDocument();
  });
});

// lát f2-draft-labels-data-menu (T367): the builder's draft overlay of a delivered project.
describe("EntityListPage: draft labels", () => {
  it("a table created after delivery says the customer cannot see it yet", async () => {
    listEntities.mockResolvedValue([entities[0], { ...made, draftState: "new" }]);
    renderPage();
    const row = (await screen.findByRole("link", { name: "Đơn hàng" })).closest("tr")!;
    expect(within(row).getByText("Mới, khách thấy khi xuất bản")).toBeInTheDocument();
    const other = screen.getByRole("link", { name: "Khách hàng" }).closest("tr")!;
    expect(within(other).queryByText(/khi xuất bản/)).toBeNull();
  });

  it("a table marked for deletion is labelled and can be undone", async () => {
    listEntities.mockResolvedValue([entities[0], { ...made, draftState: "deleted" }]);
    undoDraftDelete.mockResolvedValue(true);
    renderPage();
    const row = (await screen.findByRole("link", { name: "Đơn hàng" })).closest("tr")!;
    expect(within(row).getByText("Sẽ xoá khi xuất bản")).toBeInTheDocument();
    fireEvent.click(within(row).getByRole("button", { name: "Hoàn tác xoá bảng Đơn hàng" }));
    await waitFor(() => expect(undoDraftDelete).toHaveBeenCalledWith("entity", "e-don_hang"));
  });
});

describe("EntityListPage: 'Áp ngay cho người dùng thật' (lát f2-direct-apply-access, T379a)", () => {
  async function fillForm() {
    const form = await openForm();
    fireEvent.change(within(form).getByLabelText(/Tên hiển thị/), { target: { value: "Đơn hàng" } });
    fireEvent.change(within(form).getByLabelText(/Thuộc app/), { target: { value: "id-ticket" } });
    return form;
  }

  it("delivered + table in an app: label names the default grants, the question comes before the POST, Huỷ sends nothing", async () => {
    getPublishState.mockResolvedValue({ delivered: true });
    renderPage();
    const form = await fillForm();
    expect(await within(form).findByText("Áp ngay cho người dùng thật")).toBeInTheDocument();
    fireEvent.click(within(form).getByRole("button", { name: "Tạo bảng" }));
    expect(await within(form).findByText(/cấp quyền mặc định cho vai Nhân viên\/Quản lý ngay/)).toBeInTheDocument();
    expect(createEntityWithAcl).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).toBeNull();
    fireEvent.click(within(form).getByRole("button", { name: "Huỷ" }));
    expect(createEntityWithAcl).not.toHaveBeenCalled();
    fireEvent.click(within(form).getByRole("button", { name: "Tạo bảng" }));
    fireEvent.click(await within(form).findByRole("button", { name: "Áp ngay" }));
    await waitFor(() => expect(createEntityWithAcl).toHaveBeenCalledWith("don_hang", "Đơn hàng", "", [], "id-ticket"));
  });

  it("not delivered: no label, no question", async () => {
    renderPage();
    const form = await fillForm();
    await waitFor(() => expect(getPublishState).toHaveBeenCalled());
    expect(within(form).queryByText("Áp ngay cho người dùng thật")).toBeNull();
    fireEvent.click(within(form).getByRole("button", { name: "Tạo bảng" }));
    await waitFor(() => expect(createEntityWithAcl).toHaveBeenCalledTimes(1));
  });
});

describe("cột Bản ghi (f2-records-column)", () => {
  it("có số liệu: cột Bản ghi căn phải, đúng số từng bảng, 0 lời gọi GET /:entity?limit=1", async () => {
    getUsage.mockResolvedValue(usage([{ entity: "crm_accounts", label: "Khách hàng", count: 1234 }]));
    renderPage();
    await screen.findByRole("columnheader", { name: /Bản ghi/ });
    const row = screen.getByRole("link", { name: "Khách hàng" }).closest("tr")!;
    expect(row).toHaveTextContent("1.234");
    expect(getUsage).toHaveBeenCalledTimes(1);
    expect(listRecords).not.toHaveBeenCalled();
  });
  it("lỗi GET /admin/usage: không có cột, bảng vẫn hiện", async () => {
    renderPage();
    await screen.findByRole("link", { name: "Khách hàng" });
    await waitFor(() => expect(getUsage).toHaveBeenCalled());
    expect(screen.queryByRole("columnheader", { name: /Bản ghi/ })).toBeNull();
  });
  it("máy chủ chưa có khối current: không có cột", async () => {
    getUsage.mockResolvedValue({ days: [] });
    renderPage();
    await screen.findByRole("link", { name: "Khách hàng" });
    await waitFor(() => expect(getUsage).toHaveBeenCalled());
    expect(screen.queryByRole("columnheader", { name: /Bản ghi/ })).toBeNull();
  });
});

// specs/074 lát f2-ai-preview-data-rules (T397, T463): the assistant's proposal as a ghost row, never a control.
import { Off, WithGhost, mkChange } from "@/components/studio/assistant/ghostTestKit";

describe("EntityListPage: assistant ghost", () => {
  const renderGhost = (change: Parameters<typeof WithGhost>[0]["change"]) => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={qc}>
        <WithGhost change={change}><Off /><EntityListPage slug="northwind" /></WithGhost>
      </QueryClientProvider>,
    );
  };

  it("create: a ghost row 'Đề xuất' with the target app, no control inside; Bỏ makes it vanish", async () => {
    renderGhost(mkChange({ docType: "entity", op: "create", label: "Bảng Đơn thuốc", appName: "Phòng khám" }));
    const ghost = await screen.findByRole("group", { name: "Đề xuất của trợ lý: Bảng Đơn thuốc (sẽ vào menu Phòng khám)" });
    expect(ghost).toHaveTextContent("Đề xuất");
    expect(ghost.closest("tr")).toHaveClass("nx-proposed");
    expect(ghost.querySelectorAll("a,button,input").length).toBe(0);
    expect(ghost.closest("tr")?.querySelectorAll("a,button").length).toBe(0);
    expect(await screen.findByRole("link", { name: "Khách hàng" })).toBeInTheDocument(); // real rows stay
    expect(document.querySelector("[aria-live=polite]")).toHaveTextContent(/Có một đề xuất của trợ lý/);
    fireEvent.click(screen.getByRole("button", { name: "ghost-off" }));
    await waitFor(() => expect(screen.queryByRole("group")).toBeNull());
    expect(document.querySelector("[aria-live=polite]")).toHaveTextContent(/không còn/);
  });

  it("clicking the ghost row opens nothing", async () => {
    renderGhost(mkChange({ docType: "entity", op: "create", label: "Bảng Đơn thuốc" }));
    fireEvent.click(await screen.findByRole("group"));
    expect(push).not.toHaveBeenCalled();
  });

  it("delete: the real row is tinted and says 'Sẽ xoá', its link still works", async () => {
    renderGhost(mkChange({ docType: "entity", op: "delete", label: "Công việc", target: { docType: "entity", entity: "pm_tasks" } }));
    const row = (await screen.findByRole("link", { name: "Công việc" })).closest("tr") as HTMLElement;
    expect(row).toHaveClass("nx-proposed");
    expect(within(row).getByRole("group")).toHaveTextContent("Sẽ xoá");
  });

  it("regression: no proposal, no ghost, no tint", async () => {
    renderGhost(undefined);
    await screen.findByRole("link", { name: "Khách hàng" });
    expect(screen.queryByRole("group")).toBeNull();
    expect(document.querySelector(".nx-proposed")).toBeNull();
  });
});
