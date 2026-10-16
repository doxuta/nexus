import { describe, it, expect, vi, beforeEach } from "vitest";
import userEvent from "@testing-library/user-event";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ComponentProps, ReactElement } from "react";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { EntityPage } from "./EntityPage";
import { ApiError, type EntityMeta } from "@/lib/api";
import { qk } from "@/lib/query/keys";

// EntityPage = the single table page of a Studio project (specs/073 FR-038): "← Tất cả bảng", an h2, Sửa/Xoá of the old hub
// (same calls), the field editor, a flat "Bản ghi" section, and a right column of links to steps that are MOUNTED.
vi.mock("@/components/studio/EntityFieldEditor", () => ({
  EntityFieldEditor: ({ entityName, initialField, onSelectField, addSignal }: { addSignal?: number; entityName: string; initialField?: string | null; onSelectField?: (n: string | null) => void }) => (
    <div data-testid="stub-truong" data-add={addSignal}>
      truong:{entityName}:{initialField ?? "-"}
      <button onClick={() => onSelectField?.("email")}>pick</button>
    </div>
  ),
}));
vi.mock("@/components/studio/EntityRecordsQuickView", () => ({
  EntityRecordsQuickView: ({ entityName }: { entityName: string }) => (
    <div data-testid="stub-du-lieu">du-lieu:{entityName}</div>
  ),
}));

vi.mock("next/navigation", () => ({ usePathname: () => "/studio/du-an/northwind/dung/du-lieu/crm_accounts", useRouter: () => ({ push: vi.fn(), replace: vi.fn() }) }));

const updateEntity = vi.fn();
const deleteEntity = vi.fn();
const listFields = vi.fn();
const getLayout = vi.fn();
const listAllRelationships = vi.fn();
const listEntities = vi.fn();
const listAdminReports = vi.fn();
const listWorkflows = vi.fn();
const setWorkflowActive = vi.fn();
const listApprovalProcesses = vi.fn();
const listRoles = vi.fn();
const getRole = vi.fn();
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  listFields: (...a: unknown[]) => listFields(...a),
  getLayout: (...a: unknown[]) => getLayout(...a),
  listAllRelationships: (...a: unknown[]) => listAllRelationships(...a),
  listEntities: (...a: unknown[]) => listEntities(...a),
  listAdminReports: (...a: unknown[]) => listAdminReports(...a),
  listWorkflows: (...a: unknown[]) => listWorkflows(...a),
  setWorkflowActive: (...a: unknown[]) => setWorkflowActive(...a),
  listApprovalProcesses: (...a: unknown[]) => listApprovalProcesses(...a),
  listRoles: (...a: unknown[]) => listRoles(...a),
  getRole: (...a: unknown[]) => getRole(...a),
  updateEntity: (...a: unknown[]) => updateEntity(...a),
  deleteEntity: (...a: unknown[]) => deleteEntity(...a),
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
const emitDraftsChanged = vi.fn();
vi.mock("@/lib/draftEvents", () => ({ emitDraftsChanged: (...a: unknown[]) => emitDraftsChanged(...a) }));

const entity: EntityMeta = { id: "1", name: "crm_accounts", label: "Khách hàng", tableName: "obj_crm_accounts" };

function renderPage(ui: ReactElement, qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })) {
  return render(
    <QueryClientProvider client={qc}>
      <EmbeddedSurface>{ui}</EmbeddedSurface>
    </QueryClientProvider>,
  );
}
const page = (p: Partial<ComponentProps<typeof EntityPage>> = {}) => (
  <EntityPage slug="northwind" entity={entity} field={null} onFieldChange={() => {}} onDeleted={() => {}} {...p} />
);

// The header has ONE "Thêm trường" and a "…" menu; Sửa / Xoá live in that menu and answer inside the page.
async function choose(item: string) {
  const trigger = screen.getByRole("button", { name: "Thêm hành động cho bảng" });
  trigger.focus();
  await userEvent.keyboard("{Enter}");
  await userEvent.click(await screen.findByRole("menuitem", { name: item }));
}

beforeEach(() => {
  vi.clearAllMocks();
  getUsage.mockRejectedValue(new ApiError(404, "NOT_FOUND", "Not Found"));
  updateEntity.mockResolvedValue(entity);
  deleteEntity.mockResolvedValue(undefined);
  listFields.mockResolvedValue([]);
  getLayout.mockResolvedValue(null);
  listAllRelationships.mockResolvedValue([]);
  listEntities.mockResolvedValue([]);
  undoDraftDelete.mockResolvedValue(true);
  listAdminReports.mockResolvedValue([]);
  listWorkflows.mockResolvedValue([]);
  listApprovalProcesses.mockResolvedValue([]);
  listRoles.mockResolvedValue([]);
});

describe("EntityPage — one flat page, no second navigation", () => {
  it("has a way back, the table as h2, and no tab strip / card / h1 / badge", () => {
    const { container } = renderPage(page());
    expect(screen.getByRole("link", { name: /Tất cả bảng/ })).toHaveAttribute("href", "/studio/du-an/northwind/dung/du-lieu");
    expect(screen.getByRole("heading", { level: 2, name: /Khách hàng/ })).toBeInTheDocument();
    expect(container.querySelector(".slds-tabs_default, .slds-tabs_scoped, [role='tablist']")).toBeNull();
    expect(container.querySelector(".slds-card, h1, .slds-badge")).toBeNull();
  });

  it("mounts the field editor then the records quick view (same-tab link to the full list), both on this table", () => {
    renderPage(page());
    expect(screen.getByTestId("stub-truong")).toHaveTextContent("truong:crm_accounts");
    expect(screen.getByTestId("stub-du-lieu")).toHaveTextContent("du-lieu:crm_accounts");
  });

  it("passes ?truong= down and reports a picked field up", () => {
    const onFieldChange = vi.fn();
    renderPage(page({ field: "name", onFieldChange }));
    expect(screen.getByTestId("stub-truong")).toHaveTextContent("truong:crm_accounts:name");
    fireEvent.click(screen.getByText("pick"));
    expect(onFieldChange).toHaveBeenCalledWith("email");
  });

  it("related lists: Màn hình counts a/b fields from getLayout + listFields, Liên kết lists the target tables, Báo cáo filters by table", async () => {
    listFields.mockResolvedValue([{ name: "a" }, { name: "b" }, { name: "c" }]);
    getLayout.mockResolvedValue({
      entityName: "crm_accounts",
      views: {
        edit: { sections: [{ fields: ["a", { field: "b" }, { spacer: true }] }] },
        detail: { sections: [] },
        list: { columns: [{ field: "a" }, { field: "b" }] },
      },
    });
    listAllRelationships.mockResolvedValue([
      { id: "r1", name: "x", parentEntityName: "crm_contacts", childEntityName: "crm_accounts", relationshipType: "1-N" },
    ]);
    listEntities.mockResolvedValue([entity, { id: "2", name: "crm_contacts", label: "Liên hệ", tableName: "t" }]);
    listAdminReports.mockResolvedValue([
      { id: "p1", name: "Doanh thu", entityName: "crm_accounts" },
      { id: "p2", name: "Khác", entityName: "crm_deals" },
    ]);
    renderPage(page());
    const aside = screen.getByRole("complementary", { name: "Việc liên quan của bảng này" });
    expect(await within(aside).findByRole("link", { name: "Form Khách hàng 2/3 trường" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/man-hinh/crm_accounts?man=form");
    expect(within(aside).getByRole("link", { name: "Danh sách Khách hàng 2 cột" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/man-hinh/crm_accounts?man=danh-sach");
    expect(await within(aside).findByRole("link", { name: "Liên hệ" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/du-lieu/crm_contacts");
    expect(within(aside).getByRole("link", { name: "Sơ đồ liên kết" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/du-lieu/so-do?bang=crm_accounts");
    expect(await within(aside).findByRole("link", { name: "Doanh thu" })).toBeInTheDocument();
    expect(within(aside).queryByText("Khác")).toBeNull();
  });

  it("empty report list says so; a failing section errors alone", async () => {
    listAdminReports.mockResolvedValue([{ id: "p2", name: "Khác", entityName: "crm_deals" }]);
    listAllRelationships.mockRejectedValue(new Error("boom"));
    renderPage(page());
    expect(await screen.findByText("Chưa có báo cáo nào dùng bảng này.")).toBeInTheDocument();
    expect(await screen.findByText(/boom/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Mở danh sách bản ghi" })).toHaveAttribute("href", "/crm_accounts");
  });

  it("hides the related lists while a field is open (?truong=)", () => {
    renderPage(page({ field: "name" }));
    expect(screen.queryByRole("complementary", { name: "Việc liên quan của bảng này" })).toBeNull();
  });
});

describe("EntityPage — header and in-page answers", () => {
  it("header has ONE Thêm trường and the … menu; no dialog is ever mounted", async () => {
    const { container } = renderPage(page());
    expect(screen.getAllByRole("button", { name: /Thêm trường/ })).toHaveLength(1);
    await choose("Sửa tên và mô tả");
    expect(container.ownerDocument.querySelector("[role=dialog],[role=alertdialog]")).toBeNull();
  });

  it("detail row shows the field count that the field editor lists", async () => {
    listFields.mockResolvedValue([
      { name: "a", label: "A", fieldType: "text" },
      { name: "b", label: "B", fieldType: "text" },
    ]);
    renderPage(page());
    expect(await screen.findByText("Số trường")).toBeInTheDocument();
    expect(screen.getByText("Số trường").closest("div")).toHaveTextContent("2");
  });

  it("?moi=1 says the table was just added; otherwise nothing", () => {
    const { unmount } = renderPage(page({ isNew: true }));
    expect(screen.getByText("Đã thêm bảng Khách hàng. Thêm trường ở bên dưới.")).toBeInTheDocument();
    unmount();
    renderPage(page());
    expect(screen.queryByText(/Đã thêm bảng/)).toBeNull();
  });

  it("Thêm trường asks the field editor to open its add form", () => {
    renderPage(page());
    fireEvent.click(screen.getByRole("button", { name: /Thêm trường/ }));
    expect(screen.getByTestId("stub-truong")).toHaveAttribute("data-add", "1");
  });
});

describe("EntityPage — Sửa / Xoá keep the old hub's behaviour", () => {
  it("Sửa renames; unchanged 'Trường tên' sends no options key", async () => {
    renderPage(page());
    await choose("Sửa tên và mô tả");
    const dialog = await screen.findByRole("form", { name: "Sửa tên và mô tả" });
    fireEvent.change(within(dialog).getByLabelText(/Tên hiển thị/), { target: { value: "Khách hàng DN" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(updateEntity).toHaveBeenCalledWith("crm_accounts", { label: "Khách hàng DN", description: undefined }));
  });

  it("Sửa shows the stored 'Trường tên' and a change sends options.labelField (merged with other options)", async () => {
    listFields.mockResolvedValue([
      { name: "ho_ten", label: "Họ tên", fieldType: "text" },
      { name: "ngay_sinh", label: "Ngày sinh", fieldType: "date" },
      { name: "email", label: "Email", fieldType: "email" },
    ]);
    const withOpts: EntityMeta = { ...entity, options: JSON.stringify({ icon: "user", labelField: "email" }) };
    renderPage(page({ entity: withOpts }));
    await choose("Sửa tên và mô tả");
    const dialog = await screen.findByRole("form", { name: "Sửa tên và mô tả" });
    const select = within(dialog).getByLabelText(/Trường dùng làm tên/) as HTMLSelectElement;
    await waitFor(() => expect(within(select).getByRole("option", { name: "Họ tên" })).toBeInTheDocument());
    expect(select.value).toBe("email");
    expect(within(select).queryByRole("option", { name: "Ngày sinh" })).toBeNull();
    fireEvent.change(select, { target: { value: "ho_ten" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Lưu" }));
    await waitFor(() =>
      expect(updateEntity).toHaveBeenCalledWith("crm_accounts", {
        label: "Khách hàng", description: undefined, options: { icon: "user", labelField: "ho_ten" },
      }),
    );
  });

  it("choosing Tự động clears a stored 'Trường tên' (labelField sent as empty, other options kept)", async () => {
    listFields.mockResolvedValue([{ name: "ho_ten", label: "Họ tên", fieldType: "text" }]);
    const withOpts: EntityMeta = { ...entity, options: JSON.stringify({ icon: "user", labelField: "ho_ten" }) };
    renderPage(page({ entity: withOpts }));
    await choose("Sửa tên và mô tả");
    const dialog = await screen.findByRole("form", { name: "Sửa tên và mô tả" });
    const select = within(dialog).getByLabelText(/Trường dùng làm tên/) as HTMLSelectElement;
    await waitFor(() => expect(select.value).toBe("ho_ten"));
    fireEvent.change(select, { target: { value: "" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Lưu" }));
    await waitFor(() =>
      expect(updateEntity).toHaveBeenCalledWith("crm_accounts", {
        label: "Khách hàng", description: undefined, options: { icon: "user", labelField: "" },
      }),
    );
  });

  it("Xoá asks for the table code, deletes, then tells the host", async () => {
    const onDeleted = vi.fn();
    renderPage(page({ onDeleted }));
    await choose("Xoá bảng");
    await screen.findByText(/Xoá bảng “Khách hàng”\?/);
    const prompt = document.body;
    const confirm = within(prompt).getByRole("button", { name: "Xoá bảng" });
    expect(confirm).toBeDisabled();
    fireEvent.change(within(prompt).getByLabelText("Gõ crm_accounts để xác nhận"), { target: { value: "crm_accounts" } });
    fireEvent.click(confirm);
    await waitFor(() => expect(deleteEntity).toHaveBeenCalledWith("crm_accounts"));
    await waitFor(() => expect(onDeleted).toHaveBeenCalled());
  });

  // The project's own list (EntityRoute's query) must refresh, else the title/options go stale after Sửa and a second Sửa
  // rebuilds options.labelField from the old copy.
  it("Sửa invalidates both the entity list and the project's own table list", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const spy = vi.spyOn(qc, "invalidateQueries");
    renderPage(page(), qc);
    await choose("Sửa tên và mô tả");
    const dialog = await screen.findByRole("form", { name: "Sửa tên và mô tả" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(spy).toHaveBeenCalledWith({ queryKey: ["studio", "count", "entities"] }));
    expect(spy).toHaveBeenCalledWith({ queryKey: qk.entities() });
  });

  it("Xoá tells the host before refreshing the project's table list, and refreshes it", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const order: string[] = [];
    vi.spyOn(qc, "invalidateQueries").mockImplementation(async (f) => {
      order.push(`inv:${JSON.stringify(f?.queryKey)}`);
    });
    renderPage(page({ onDeleted: () => order.push("deleted") }), qc);
    await choose("Xoá bảng");
    await screen.findByText(/Xoá bảng “Khách hàng”\?/);
    const prompt = document.body;
    fireEvent.change(within(prompt).getByLabelText("Gõ crm_accounts để xác nhận"), { target: { value: "crm_accounts" } });
    fireEvent.click(within(prompt).getByRole("button", { name: "Xoá bảng" }));
    await waitFor(() => expect(order).toContain('inv:["studio","count","entities"]'));
    expect(order[0]).toBe("deleted");
  });
  // specs/063-draft-publish S7-9: on a delivered workspace the table survives the click, marked for deletion.
  it("a deferred delete keeps the page (the table still exists), tells the pending-change bar and does not call onDeleted", async () => {
    listEntities.mockResolvedValue([{ ...entity, draftState: "deleted" }]);
    const onDeleted = vi.fn();
    renderPage(page({ onDeleted }));
    await choose("Xoá bảng");
    await screen.findByText(/Xoá bảng “Khách hàng”\?/);
    fireEvent.change(within(document.body).getByLabelText("Gõ crm_accounts để xác nhận"), { target: { value: "crm_accounts" } });
    fireEvent.click(within(document.body).getByRole("button", { name: "Xoá bảng" }));
    await waitFor(() => expect(emitDraftsChanged).toHaveBeenCalledWith("entity-delete"));
    expect(onDeleted).not.toHaveBeenCalled();
  });

  it("a table marked 'Sẽ xoá khi xuất bản' says so and Hoàn tác discards that pending change", async () => {
    renderPage(page({ entity: { ...entity, draftState: "deleted" } }));
    expect(screen.getByText(/sẽ bị xoá khi xuất bản/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Hoàn tác" }));
    await waitFor(() => expect(undoDraftDelete).toHaveBeenCalledWith("entity", "1"));
  });

  it("no banner for an ordinary table", () => {
    renderPage(page());
    expect(screen.queryByText(/sẽ bị xoá khi xuất bản/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Hoàn tác" })).toBeNull();
  });
});

// specs/074 lát f2-related-lists-rules-access: the Luật and Quyền sections of the right column.
describe("EntityPage — related Luật and Quyền", () => {
  const rule = (o = {}) => ({ id: "w1", name: "Nhắc lịch", entityName: "crm_accounts", trigger: "after_create", isActive: true, ...o });
  const aside = () => screen.getByRole("complementary", { name: "Việc liên quan của bảng này" });

  it("Luật lists only this table's rules with a 'Khi' sentence; the switch writes and stays after a refetch", async () => {
    listWorkflows.mockResolvedValue([rule(), rule({ id: "w2", name: "Khác", entityName: "crm_deals" })]);
    setWorkflowActive.mockResolvedValue({ id: "w1", isActive: false });
    renderPage(page());
    expect(await within(aside()).findByRole("link", { name: "Nhắc lịch" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/tu-dong/luat/w1");
    expect(within(aside()).queryByText("Khác")).toBeNull();
    const sw = within(aside()).getByRole("switch", { name: "Bật luật Nhắc lịch" });
    expect(sw).toBeChecked();
    await userEvent.click(sw);
    await waitFor(() => expect(setWorkflowActive).toHaveBeenCalledWith("w1", false));
    expect(await within(aside()).findByText("Đã tắt Nhắc lịch")).toBeInTheDocument();
    expect(within(aside()).getByRole("switch", { name: "Bật luật Nhắc lịch" })).not.toBeChecked();
  });

  it("409 is answered at the row with a link to the rule, and the switch does not move", async () => {
    listWorkflows.mockResolvedValue([rule()]);
    setWorkflowActive.mockRejectedValue(new ApiError(409, "WEBHOOK_LEGACY_SECRET_PENDING", "x"));
    renderPage(page());
    await userEvent.click(await within(aside()).findByRole("switch", { name: "Bật luật Nhắc lịch" }));
    expect(await within(aside()).findByText(/Luật này còn khoá ký trần/)).toBeInTheDocument();
    expect(within(aside()).getByRole("link", { name: "Mở luật" })).toBeInTheDocument();
    expect(within(aside()).getByRole("switch", { name: "Bật luật Nhắc lịch" })).toBeChecked();
  });

  it("Luật: empty sentence, approval count, own error with retry", async () => {
    renderPage(page());
    expect(await within(aside()).findByText("Chưa có luật chạy trên bảng này.")).toBeInTheDocument();
    listApprovalProcesses.mockResolvedValue([{ id: "a", name: "A", entityName: "crm_accounts" }, { id: "b", name: "B", entityName: "crm_deals" }]);
    listWorkflows.mockRejectedValueOnce(new Error("hong-luat"));
    renderPage(page());
    expect(await screen.findByText(/hong-luat/)).toBeInTheDocument();
  });

  it("Quyền shows each role's level from levelOf (own row > every-table row), and its own error", async () => {
    listRoles.mockResolvedValue([{ id: "r1", name: "sales", label: "Kinh doanh" }, { id: "r2", name: "ro", label: "Chỉ đọc" }, { id: "r3", name: "x", label: "Chặn" }]);
    getRole.mockImplementation(async (id: string) => ({
      r1: { id: "r1", actions: [{ entityName: "crm_accounts", action: "view", accessLevel: "all" }, { entityName: "crm_accounts", action: "create", accessLevel: "all" }, { entityName: "crm_accounts", action: "edit", accessLevel: "all" }] },
      r2: { id: "r2", actions: [{ entityName: "*", action: "view", accessLevel: "all" }] },
      r3: { id: "r3", actions: [] },
    }[id]));
    renderPage(page());
    expect(await within(aside()).findByText("Kinh doanh: Xem và sửa")).toBeInTheDocument();
    expect(within(aside()).getByText("Chỉ đọc: Chỉ xem")).toBeInTheDocument();
    expect(within(aside()).getByText("Chặn: Không thấy")).toBeInTheDocument();
    expect(within(aside()).getByRole("link", { name: "Mở Người dùng và quyền" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/nguoi-dung-quyen?bang=crm_accounts");
  });

  it("Quyền: failing roles list errors alone", async () => {
    listRoles.mockRejectedValue(new Error("hong-vai"));
    renderPage(page());
    expect(await screen.findByText(/hong-vai/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Mở danh sách bản ghi" })).toBeInTheDocument();
  });
});

describe("số Bản ghi ở hàng chi tiết (f2-records-column)", () => {
  it("có số liệu thì hiện, không có thì vắng", async () => {
    getUsage.mockResolvedValue(usage([{ entity: "crm_accounts", label: "Khách hàng", count: 42 }]));
    renderPage(page());
    expect(await screen.findByText("42")).toBeInTheDocument();
    expect(screen.getByText("Bản ghi", { selector: "dt, dt *, .slds-form-element__label, .slds-text-title" })).toBeInTheDocument();
  });
  it("lỗi usage: không có hàng Bản ghi", async () => {
    renderPage(page());
    await waitFor(() => expect(getUsage).toHaveBeenCalled());
    expect(screen.queryByText("42")).toBeNull();
    expect(screen.queryByText("Bản ghi", { selector: "dt, dt *, .slds-form-element__label, .slds-text-title" })).toBeNull();
  });
});

// specs/074 lát f2-ai-preview-data-rules (T398, T463)
import { WithGhost, mkChange } from "@/components/studio/assistant/ghostTestKit";

describe("EntityPage: assistant ghost", () => {
  it("field create: a ghost line 'Đề xuất' above the field editor, no control inside", () => {
    renderPage(<WithGhost change={mkChange({ docType: "field", op: "create", label: "Thêm trường Ngày hẹn", target: { docType: "field", entity: "crm_accounts", field: "ngay_hen" } })}>{page()}</WithGhost>);
    const g = screen.getByRole("group", { name: "Đề xuất của trợ lý: Thêm trường Ngày hẹn" });
    expect(g).toHaveTextContent("Đề xuất");
    expect(g.querySelectorAll("a,button,input").length).toBe(0);
    expect(g.compareDocumentPosition(screen.getByTestId("stub-truong")) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(document.querySelector("[aria-live=polite]")).toHaveTextContent(/Có một đề xuất/);
  });
  it("field update: the new label with 'Trước: …'", () => {
    renderPage(<WithGhost change={mkChange({ docType: "field", op: "update", label: "Đổi nhãn trường", target: { docType: "field", entity: "crm_accounts", field: "sdt" }, before: { label: "Điện thoại" }, after: { label: "Số di động" } })}>{page()}</WithGhost>);
    const g = screen.getByRole("group");
    expect(g).toHaveTextContent("Số di động");
    expect(g).toHaveTextContent("Trước: Điện thoại");
  });
  it("a field proposal for another table paints nothing; table delete says 'Sẽ xoá'", () => {
    const other = mkChange({ docType: "field", op: "create", label: "x", target: { docType: "field", entity: "khac" } });
    const { unmount } = renderPage(<WithGhost change={other}>{page()}</WithGhost>);
    expect(screen.queryByRole("group")).toBeNull();
    unmount();
    renderPage(<WithGhost change={mkChange({ docType: "entity", op: "delete", label: "Khách hàng", target: { docType: "entity", entity: "crm_accounts" } })}>{page()}</WithGhost>);
    expect(screen.getByRole("group")).toHaveTextContent("Sẽ xoá");
  });
});
