// Hub tab "Trường" — field list + inspector.
//
// Queue #92 (field-inspector-stale-overwrite): saving a field in the inspector used to PUT the
// WHOLE state on screen (label, isRequired, options, default, description), so a save written
// from a stale screen put back whatever someone had just changed elsewhere — the 2026-09-29
// acceptance run turned "nhạy cảm" on and wrote isRequired=true back over a change made through
// the API. The inspector now sends only what was changed since it loaded the field, and
// "Bắt buộc" is edited in the inspector only (no one-click toggle on the row, which the same
// run had hit by mistake with no confirm and no undo).
import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { FieldMeta } from "@/lib/api";

vi.mock("@/lib/auth/AuthProvider", () => ({
  useAuth: () => ({ user: { id: "u1", isAdmin: true } }),
}));

// The token/project gate itself is covered in useProjectQuery.test.tsx; here the hook is a plain query.
vi.mock("./useProjectQuery", async () => {
  const { useQuery } = await import("@tanstack/react-query");
  return {
    useProjectQuery: (slug: string, key: readonly unknown[], fetcher: () => Promise<unknown>) =>
      useQuery({ queryKey: ["project", slug, ...key], queryFn: fetcher }),
  };
});

const listFields = vi.fn();
const updateField = vi.fn();
const reorderFields = vi.fn();
const deleteField = vi.fn();
const listRecordsPage = vi.fn();
const listRecords = vi.fn();
const addField = vi.fn();
const validateFormula = vi.fn();
const listEntities = vi.fn();
const appsCatalog = vi.fn();
const appNav = vi.fn();
const getPublishState = vi.fn();
const getLayout = vi.fn();
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  ensureAuth: vi.fn().mockResolvedValue(undefined),
  listEntities: (...a: unknown[]) => listEntities(...a),
  appsCatalog: (...a: unknown[]) => appsCatalog(...a),
  appNav: (...a: unknown[]) => appNav(...a),
  listFields: (...a: unknown[]) => listFields(...a),
  updateField: (...a: unknown[]) => updateField(...a),
  reorderFields: (...a: unknown[]) => reorderFields(...a),
  deleteField: (...a: unknown[]) => deleteField(...a),
  listRecordsPage: (...a: unknown[]) => listRecordsPage(...a),
  listRecords: (...a: unknown[]) => listRecords(...a),
  addField: (...a: unknown[]) => addField(...a),
  validateFormula: (...a: unknown[]) => validateFormula(...a),
  getPublishState: (...a: unknown[]) => getPublishState(...a),
  getLayout: (...a: unknown[]) => getLayout(...a),
}));

const undoDraftDelete = vi.fn();
vi.mock("@/lib/draftUndo", () => ({ undoDraftDelete: (...a: unknown[]) => undoDraftDelete(...a) }));
const toastInfo = vi.fn();
vi.mock("@/components/ui/Toaster", () => ({ toast: { info: (m: string) => toastInfo(m), success: vi.fn(), error: vi.fn(), warning: vi.fn() } }));

import { EntityFieldEditor } from "./EntityFieldEditor";
import { ApiError } from "@/lib/api";

const phone: FieldMeta = {
  id: "f-phone", name: "phone", label: "Điện thoại", fieldType: "text", isRequired: false,
  options: { maxLength: 20 }, description: "",
};
const email: FieldMeta = { id: "f-email", name: "email", label: "Email", fieldType: "email", isRequired: true };

function renderEditor() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <EntityFieldEditor entityName="crm_contacts" />
    </QueryClientProvider>,
  );
  return qc;
}

// The inspector opens on demand (SLDS docked panel), from the field's label in the grid.
async function openField(label: string) {
  fireEvent.click(await screen.findByRole("button", { name: label }));
  return screen.findByDisplayValue(label);
}
const saveButton = () => screen.getByRole("button", { name: /^Lưu( field)?$/ });

beforeEach(() => {
  vi.clearAllMocks();
  listEntities.mockResolvedValue([{ id: "e1", name: "crm_contacts", label: "Liên hệ", tableName: "obj_crm_contacts" }]);
  appsCatalog.mockResolvedValue([]);
  appNav.mockResolvedValue({ items: [] });
  getPublishState.mockRejectedValue(new Error("not delivered"));
  listFields.mockResolvedValue([phone, email]);
  // custom edit form: only email is placed
  getLayout.mockResolvedValue({ entityName: "crm_contacts", views: { edit: { sections: [{ title: "A", columns: 2, fields: ["email"] }] }, detail: {}, list: {} } });
  updateField.mockImplementation(async (_id: string, p: Partial<FieldMeta>) => ({ ...phone, ...p }));
  reorderFields.mockResolvedValue({ entityName: "crm_contacts", updated: 2 });
  deleteField.mockResolvedValue(undefined);
  listRecords.mockResolvedValue([{ id: "rec-1", data: { phone: "0901" } }]);
  listRecordsPage.mockResolvedValue({ records: [], pagination: { page: 1, limit: 1, total: 0, totalPages: 0 } });
  // The created field comes back on the next list fetch, as it does from the server.
  addField.mockImplementation(async (_entity: string, f: FieldMeta) => {
    const created = { ...f, id: "f-new" };
    listFields.mockResolvedValue([phone, email, created]);
    return created;
  });
});

describe("EntityFieldEditor — inspector saves only what changed (#92)", () => {
  it("renaming a field sends the label alone — not isRequired, options, default or description", async () => {
    renderEditor();
    const labelInput = await openField("Điện thoại");
    fireEvent.change(labelInput, { target: { value: "Số điện thoại" } });
    fireEvent.click(saveButton());
    await waitFor(() => expect(updateField).toHaveBeenCalledTimes(1));
    expect(updateField).toHaveBeenCalledWith("f-phone", { label: "Số điện thoại" });
  });

  it("a stale screen does not write isRequired back when the list refreshed underneath it", async () => {
    const qc = renderEditor();
    const labelInput = await openField("Điện thoại");
    // Someone makes `phone` required elsewhere; this screen's list refetches (focus, another
    // save…) while the inspector still shows the state it loaded.
    listFields.mockResolvedValue([{ ...phone, isRequired: true }, email]);
    await qc.refetchQueries({ queryKey: ["metadata", "fields", "crm_contacts"] });
    fireEvent.change(labelInput, { target: { value: "Số điện thoại" } });
    fireEvent.click(saveButton());
    await waitFor(() => expect(updateField).toHaveBeenCalledTimes(1));
    expect(updateField.mock.calls[0][1]).not.toHaveProperty("isRequired");
  });

  it("'Bắt buộc' is a switch in the inspector and saving it sends isRequired alone", async () => {
    renderEditor();
    await openField("Điện thoại");
    fireEvent.click(screen.getByRole("switch", { name: /Bắt buộc/ }));
    fireEvent.click(saveButton());
    await waitFor(() => expect(updateField).toHaveBeenCalledWith("f-phone", { isRequired: true }));
  });

  it("turning 'Trường nhạy cảm' on sends the options with the flag — and nothing else", async () => {
    renderEditor();
    await openField("Điện thoại");
    fireEvent.click(screen.getByRole("switch", { name: /Trường nhạy cảm/ }));
    fireEvent.click(saveButton());
    await waitFor(() => expect(updateField).toHaveBeenCalledTimes(1));
    expect(updateField).toHaveBeenCalledWith("f-phone", { options: { maxLength: 20, sensitive: true } });
  });

  // A long label must end in "…", not be cut mid-word: the button is inline-block, so the
  // cell's slds-truncate cannot clip its text — the button needs its own.
  it("the label link in the grid truncates with an ellipsis", async () => {
    renderEditor();
    const grid = await screen.findByRole("grid", { name: "Danh sách trường" });
    expect(await within(grid).findByRole("button", { name: "Điện thoại" })).toHaveClass("slds-truncate");
  });

  it("the field rows carry no one-click required toggle; required is a tick, not a badge", async () => {
    renderEditor();
    const grid = await screen.findByRole("grid", { name: "Danh sách trường" });
    await within(grid).findByRole("button", { name: "Email" });
    expect(screen.queryByRole("button", { name: /^Bắt buộc: (bật|tắt)$/ })).not.toBeInTheDocument();
    expect(within(grid).queryByRole("switch")).not.toBeInTheDocument();
    const row = within(grid).getByRole("button", { name: "Email" }).closest("tr")!;
    expect(within(row).getByText("Bắt buộc", { selector: ".slds-assistive-text" })).toBeInTheDocument();
    const phoneRow = within(grid).getByRole("button", { name: "Điện thoại" }).closest("tr")!;
    expect(within(phoneRow).queryByText("Bắt buộc", { selector: ".slds-assistive-text" })).not.toBeInTheDocument();
  });

  it("'Trên form': tick for a placed field, 'Đặt lên form' link to the screen editor for an unplaced one", async () => {
    renderEditor();
    const grid = await screen.findByRole("grid", { name: "Danh sách trường" });
    const placed = (await within(grid).findByRole("button", { name: "Email" })).closest("tr")!;
    await waitFor(() => expect(within(placed).getByText("Trên form", { selector: ".slds-assistive-text" })).toBeInTheDocument());
    expect(within(placed).queryByRole("link", { name: "Đặt lên form" })).not.toBeInTheDocument();
    const unplaced = within(grid).getByRole("button", { name: "Điện thoại" }).closest("tr")!;
    const link = await within(unplaced).findByRole("link", { name: "Đặt lên form" });
    expect(link.getAttribute("href")).toMatch(/dung\/man-hinh\/crm_contacts\?.*truong=phone/);
  });

  it("attributes are plain text under the type, no Badge", async () => {
    listFields.mockResolvedValue([{ ...phone, isUnique: true, options: { indexed: true, sensitive: true } }, email]);
    renderEditor();
    const grid = await screen.findByRole("grid", { name: "Danh sách trường" });
    const row = (await within(grid).findByRole("button", { name: "Điện thoại" })).closest("tr")!;
    expect(row.querySelector(".slds-badge")).toBeNull();
    expect(within(row).getByText(/Duy nhất · Lọc nhanh · Nhạy cảm/)).toBeInTheDocument();
  });

  it("nothing to save until something changed", async () => {
    renderEditor();
    await openField("Điện thoại");
    expect(saveButton()).toBeDisabled();
  });
});

describe("EntityFieldEditor — list actions", () => {
  it("'Chuyển xuống' persists the new order", async () => {
    renderEditor();
    const grid = await screen.findByRole("grid", { name: "Danh sách trường" });
    await within(grid).findByRole("button", { name: "Điện thoại" });
    fireEvent.pointerDown(screen.getByRole("button", { name: "Thêm thao tác cho Điện thoại" }), { button: 0, ctrlKey: false });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Chuyển xuống" }));
    await waitFor(() => expect(reorderFields).toHaveBeenCalledWith("crm_contacts", ["f-email", "f-phone"]));
  });

  it("a failed reorder rolls back and says so, with a close button visible on the red notice", async () => {
    reorderFields.mockRejectedValue(new Error("Sắp xếp bị từ chối"));
    renderEditor();
    const grid = await screen.findByRole("grid", { name: "Danh sách trường" });
    await within(grid).findByRole("button", { name: "Điện thoại" });
    fireEvent.pointerDown(screen.getByRole("button", { name: "Thêm thao tác cho Điện thoại" }), { button: 0, ctrlKey: false });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Chuyển xuống" }));
    const notice = (await screen.findByText("Sắp xếp bị từ chối")).closest(".slds-scoped-notification") as HTMLElement;
    expect(notice).toHaveClass("slds-theme_error");
    const close = within(notice).getByRole("button", { name: "Đóng" });
    expect(close).toHaveClass("slds-button_icon-inverse");
    fireEvent.click(close);
    expect(screen.queryByText("Sắp xếp bị từ chối")).not.toBeInTheDocument();
  });

  it("deleting a field from an empty table confirms once and calls the API", async () => {
    renderEditor();
    await openField("Điện thoại");
    fireEvent.click(screen.getByRole("button", { name: "Xoá trường" }));
    const prompt = await screen.findByText(/Xoá trường “/).then((n) => n.closest("[tabindex]") as HTMLElement);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    await within(prompt).findByText("Bảng chưa có bản ghi nào");
    fireEvent.click(within(prompt).getByRole("button", { name: "Xoá trường" }));
    await waitFor(() => expect(deleteField).toHaveBeenCalledWith("f-phone"));
  });

  it("deleting a field from a table with records asks for the field code", async () => {
    listRecordsPage.mockResolvedValue({ records: [], pagination: { page: 1, limit: 1, total: 12, totalPages: 12 } });
    renderEditor();
    await openField("Điện thoại");
    fireEvent.click(screen.getByRole("button", { name: "Xoá trường" }));
    const prompt = await screen.findByText(/Xoá trường “/).then((n) => n.closest("[tabindex]") as HTMLElement);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    const confirm = within(prompt).getByRole("button", { name: "Xoá trường" });
    await within(prompt).findByLabelText("Gõ phone để xác nhận");
    expect(confirm).toBeDisabled();
    fireEvent.change(within(prompt).getByLabelText("Gõ phone để xác nhận"), { target: { value: "phone" } });
    expect(confirm).toBeEnabled();
  });
});

// "Thêm trường mới" dialog (modals/base + visual-picker/vertical): the type is a radio in one of
// four groups, the config a type cannot be created without shows up front, and the footer button
// submits the form it is attached to with `form=`.
async function openAddField() {
  const grid = await screen.findByRole("grid", { name: "Danh sách trường" });
  await within(grid).findByRole("button", { name: "Điện thoại" });
  fireEvent.click(screen.getByRole("button", { name: "Thêm trường" }));
  const dialog = await screen.findByRole("region", { name: "Thêm trường mới" });
  expect(screen.queryByRole("dialog")).toBeNull();
  fireEvent.change(within(dialog).getByPlaceholderText("Ví dụ: Ngày ký hợp đồng"), { target: { value: "Mức độ" } });
  return dialog;
}
const addButton = (dialog: HTMLElement) => within(dialog).getByRole("button", { name: "Thêm trường" });

describe("EntityFieldEditor — add field strip (in place)", () => {
  // A new field on a delivered workspace is a pending change: the header's "Xuất bản" count recounts on this event.
  it("announces drafts-changed after creating a field", async () => {
    const heard = vi.fn();
    window.addEventListener("nexus:drafts-changed", heard);
    renderEditor();
    const dialog = await openAddField();
    fireEvent.click(addButton(dialog));
    await waitFor(() => expect(addField).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(heard).toHaveBeenCalledTimes(1));
    expect((heard.mock.calls[0][0] as CustomEvent<string>).detail).toBe("field-create");
    window.removeEventListener("nexus:drafts-changed", heard);
  });

  it("'Chọn một' shows its values up front and creates a picklist with them, then opens it", async () => {
    renderEditor();
    const dialog = await openAddField();
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Chọn một/ }));
    // Required config sits outside the collapsed "Cấu hình nâng cao".
    fireEvent.click(within(dialog).getByRole("button", { name: "Thêm giá trị" }));
    fireEvent.change(within(dialog).getByPlaceholderText("Mã…"), { target: { value: "hot" } });
    fireEvent.click(addButton(dialog));
    await waitFor(() => expect(addField).toHaveBeenCalledTimes(1));
    expect(addField).toHaveBeenCalledWith("crm_contacts", expect.objectContaining({
      name: "muc_do", label: "Mức độ", fieldType: "picklist",
      options: expect.objectContaining({ values: ["hot"] }),
    }));
    const chain = (await screen.findByText("Đã tạo “Mức độ”. Tiếp theo:")).closest(".slds-scoped-notification") as HTMLElement;
    // slds-theme_success is dark green: the close glyph must be the inverse variant to be seen.
    expect(within(chain).getByRole("button", { name: "Đóng" })).toHaveClass("slds-button_icon-inverse");
    expect(await screen.findByDisplayValue("Mức độ")).toBeInTheDocument(); // inspector on the new field
    expect(screen.queryByRole("region", { name: "Thêm trường mới" })).not.toBeInTheDocument();
  });

  // queue #274: the label typed beside each value used to be dropped on create.
  it("create sends the Vietnamese label typed beside a picklist value (valueLabels)", async () => {
    renderEditor();
    const dialog = await openAddField();
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Chọn một/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Thêm giá trị" }));
    fireEvent.change(within(dialog).getByPlaceholderText("Mã…"), { target: { value: "kham" } });
    fireEvent.change(within(dialog).getByPlaceholderText("Nhãn hiện ra (tuỳ chọn)…"), { target: { value: "Khám" } });
    fireEvent.click(addButton(dialog));
    await waitFor(() => expect(addField).toHaveBeenCalledTimes(1));
    expect(addField).toHaveBeenCalledWith("crm_contacts", expect.objectContaining({
      options: expect.objectContaining({ values: ["kham"], valueLabels: { kham: "Khám" } }),
    }));
  });

  // queue #242: BE allows a unique auto_number since RM-28; the toggle used to be hidden.
  it("'Duy nhất' can be switched on for a số tự sinh field and is sent as isUnique", async () => {
    renderEditor();
    const dialog = await openAddField();
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Số tự sinh/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: /Cấu hình nâng cao/ }));
    fireEvent.click(within(dialog).getByRole("switch", { name: /^Duy nhất/ }));
    fireEvent.click(addButton(dialog));
    await waitFor(() => expect(addField).toHaveBeenCalledTimes(1));
    expect(addField).toHaveBeenCalledWith("crm_contacts", expect.objectContaining({ fieldType: "auto_number", isUnique: true }));
  });

  // queue #320: a relation field gets a record picker for its default (was hidden).
  it("a quan hệ field offers the 'Giá trị mặc định' record picker", async () => {
    renderEditor();
    const dialog = await openAddField();
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Quan hệ(?! đa hình)/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: /Cấu hình nâng cao/ }));
    expect(within(dialog).getByRole("combobox", { name: /Giá trị mặc định/ })).toBeInTheDocument();
  });

  // Review of #320: picking a record must reach the create payload, and changing the target
  // table must drop a default that points at the old table.
  async function pickRelationDefault(dialog: HTMLElement) {
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Quan hệ(?! đa hình)/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: /Cấu hình nâng cao/ }));
    fireEvent.click(within(dialog).getByRole("combobox", { name: /Bảng đích/ }));
    fireEvent.click(await screen.findByRole("option", { name: /Liên hệ/ }));
    const def = within(dialog).getByRole("combobox", { name: /Giá trị mặc định/ });
    fireEvent.focus(def); // the picker loads records on focus
    fireEvent.click(def);
    fireEvent.click(await screen.findByRole("option", { name: /0901|a@b\.vn|rec-1/ }));
  }

  it("a relation default picked from the target's records is chained as PUT defaultValue", async () => {
    renderEditor();
    const dialog = await openAddField();
    await pickRelationDefault(dialog);
    fireEvent.click(addButton(dialog));
    await waitFor(() => expect(addField).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(updateField).toHaveBeenCalledWith("f-new", { defaultValue: "rec-1" }));
  });

  it("changing the relation target clears the default picked for the old target", async () => {
    listEntities.mockResolvedValue([
      { id: "e1", name: "crm_contacts", label: "Liên hệ", tableName: "obj_crm_contacts" },
      { id: "e2", name: "crm_deals", label: "Cơ hội", tableName: "obj_crm_deals" },
    ]);
    renderEditor();
    const dialog = await openAddField();
    await pickRelationDefault(dialog);
    fireEvent.click(within(dialog).getByRole("combobox", { name: /Bảng đích/ }));
    fireEvent.click(await screen.findByRole("option", { name: /Cơ hội/ }));
    fireEvent.click(addButton(dialog));
    await waitFor(() => expect(addField).toHaveBeenCalledTimes(1));
    expect(updateField).not.toHaveBeenCalled(); // no stale default chained after create
  });

  it("the add button stays locked while values repeat or a polymorphic relation has no target", async () => {
    renderEditor();
    const dialog = await openAddField();
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Chọn một/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Thêm giá trị" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Thêm giá trị" }));
    within(dialog).getAllByPlaceholderText("Mã…").forEach((el) => fireEvent.change(el, { target: { value: "hot" } }));
    expect(addButton(dialog)).toBeDisabled();
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Quan hệ đa hình/ }));
    expect(addButton(dialog)).toBeDisabled();
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Văn bản Một dòng/ }));
    expect(addButton(dialog)).toBeEnabled();
  });

  it("a failed create keeps the dialog open and shows the error inside it", async () => {
    addField.mockRejectedValue(new Error("Mã trường đã tồn tại"));
    renderEditor();
    const dialog = await openAddField();
    fireEvent.click(addButton(dialog));
    expect(await within(dialog).findByText("Mã trường đã tồn tại")).toBeInTheDocument();
  });

  // Enter in any text box of a form presses its submit button (implicit submission) — the footer
  // button is wired with form=, so these two boxes must swallow Enter or they create the field
  // half-configured: the type cannot be changed after creation.
  it("Enter in the type filter picks the first type still shown and does not submit", async () => {
    renderEditor();
    const dialog = await openAddField();
    const filter = within(dialog).getByPlaceholderText("Tìm kiểu dữ liệu…");
    fireEvent.change(filter, { target: { value: "tiền" } });
    expect(fireEvent.keyDown(filter, { key: "Enter" })).toBe(false); // default prevented
    expect(within(dialog).getByRole("radio", { name: /^Tiền tệ/ })).toBeChecked();
    expect(addField).not.toHaveBeenCalled();
  });

  it("Enter in a value's label adds the next value and does not submit", async () => {
    renderEditor();
    const dialog = await openAddField();
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Chọn một/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Thêm giá trị" }));
    fireEvent.change(within(dialog).getByPlaceholderText("Mã…"), { target: { value: "hot" } });
    const label = within(dialog).getByPlaceholderText("Nhãn hiện ra (tuỳ chọn)…");
    fireEvent.change(label, { target: { value: "Nóng" } });
    expect(fireEvent.keyDown(label, { key: "Enter" })).toBe(false);
    expect(within(dialog).getAllByPlaceholderText("Mã…")).toHaveLength(2);
    expect(addField).not.toHaveBeenCalled();
  });
});

// specs/053: a text-result formula saved through the API or the AI Builder must stay editable
// in the Studio. The live check used to drop the "text" hint, the server judged the formula as a
// number (422 FIELD_FORMULA_TYPE_MISMATCH) and Save stayed locked even for a label change.
describe("EntityFieldEditor — text formula field (specs/053)", () => {
  const grade: FieldMeta = {
    id: "f-grade", name: "grade", label: "Xếp loại", fieldType: "formula", isRequired: false,
    options: { formula: 'IF(score >= 8, "Giỏi", "TB")', resultType: "text" },
  };

  it("checks the formula as text and lets a relabel save", async () => {
    listFields.mockResolvedValue([phone, grade]);
    updateField.mockImplementation(async (_id: string, p: Partial<FieldMeta>) => ({ ...grade, ...p }));
    // What POST /admin/formula/validate answers: a text formula without its hint is a number mismatch.
    validateFormula.mockImplementation(async (_e: string, _f: string, resultType?: string) => {
      if (resultType !== "text") throw new ApiError(422, "FIELD_FORMULA_TYPE_MISMATCH", "kết quả phải là số");
      return { valid: true, fields: ["score"], resultType: "text" };
    });
    renderEditor();
    const labelInput = await openField("Xếp loại");
    await waitFor(() => expect(validateFormula).toHaveBeenCalled());
    expect(validateFormula).toHaveBeenLastCalledWith("crm_contacts", 'IF(score >= 8, "Giỏi", "TB")', "text");
    fireEvent.change(labelInput, { target: { value: "Học lực" } });
    await waitFor(() => expect(saveButton()).toBeEnabled());
    fireEvent.click(saveButton());
    await waitFor(() => expect(updateField).toHaveBeenCalledWith("f-grade", { label: "Học lực" }));
  });
});

// #262: the 09-30 journeys (clinic invoice total, repair parts total) had to curl
// POST /admin/fields because "Trường cần tính" listed only number/currency/rating — the line's
// "Thành tiền" is a formula. The list must offer a number or money formula (the engine sums both)
// and still leave out a text formula (the engine answers 422).
describe("EntityFieldEditor — rollup over a formula (#262)", () => {
  const total: FieldMeta = {
    id: "f-total", name: "total", label: "Tổng tiền", fieldType: "rollup", isRequired: false,
    options: { childEntity: "lines", metric: "sum", aggField: "", viaRelation: "contact" },
  };
  const lineFields: FieldMeta[] = [
    { name: "contact", label: "Liên hệ", fieldType: "relation", options: { relatedEntity: "crm_contacts" } },
    { name: "qty", label: "Số lượng", fieldType: "number" },
    { name: "line_total", label: "Thành tiền", fieldType: "formula", options: { formula: "qty * 2", resultType: "currency", currencyCode: "VND" } },
    { name: "grade", label: "Xếp loại", fieldType: "formula", options: { formula: 'IF(qty > 1, "A", "B")', resultType: "text" } },
  ];

  it("offers the child's number and money formulas, not a text formula", async () => {
    listFields.mockImplementation(async (entity: string) => (entity === "lines" ? lineFields : [phone, total]));
    renderEditor();
    await openField("Tổng tiền");
    const agg = await screen.findByRole("combobox", { name: /Trường cần tính/ });
    await waitFor(() => expect(agg).toBeEnabled());
    fireEvent.click(agg);
    expect(await screen.findByRole("option", { name: /Thành tiền/ })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: /Số lượng/ })).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: /Xếp loại/ })).toBeNull();
  });
});

// Queue #301: two tables both called "Khách hàng" must be told apart in the "Bảng đích" picker.
describe("EntityFieldEditor — target table picker tells same-named tables apart (#301)", () => {
  it("shows code and app name on the clashing labels only", async () => {
    listEntities.mockResolvedValue([
      { id: "e1", name: "crm_contacts", label: "Liên hệ", tableName: "obj_crm_contacts" },
      { id: "e2", name: "crm_customers", label: "Khách hàng", tableName: "obj_crm_customers" },
      { id: "e3", name: "dental_customers", label: "Khách hàng", tableName: "obj_dental_customers" },
    ]);
    appsCatalog.mockResolvedValue([
      { id: "a1", name: "crm", label: "CRM", icon: null, color: null, isDefault: true },
      { id: "a2", name: "nhakhoa", label: "Nha khoa", icon: null, color: null, isDefault: false },
    ]);
    appNav.mockImplementation(async (n: string) => ({
      items: [{ type: "entity", refId: "x", label: "", href: n === "crm" ? "/crm_customers" : "/dental_customers" }],
    }));
    renderEditor();
    const dialog = await openAddField();
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Quan hệ Trỏ/ }));
    fireEvent.click(await within(dialog).findByRole("combobox", { name: /Bảng đích/ }));
    await waitFor(() => expect(screen.getByText("crm_customers · CRM")).toBeInTheDocument());
    expect(screen.getByText("dental_customers · Nha khoa")).toBeInTheDocument();
    expect(document.querySelectorAll(".slds-listbox__option-meta")).toHaveLength(2); // "Liên hệ" stays bare
  });
});

// specs/063-draft-publish S7-8 (T124): what the builder of a delivered workspace needs to read off a field.
describe("EntityFieldEditor — new fields and draft edits on a delivered workspace (S7-8)", () => {
  it("marks a field born after the last publish and a required flag that is not live yet", async () => {
    listFields.mockResolvedValue([phone, { ...email, id: "f-prio", name: "priority", label: "Ưu tiên", fieldType: "text", draftState: "new", pendingRequired: true }]);
    renderEditor();
    const grid = await screen.findByRole("grid", { name: "Danh sách trường" });
    await within(grid).findByRole("button", { name: "Ưu tiên" });
    expect(within(grid).getByText(/Mới — khách thấy khi Xuất bản/)).toBeInTheDocument();
    expect(within(grid).getByText(/Bắt buộc sẽ bật khi Xuất bản/)).toBeInTheDocument();
    // a live field carries neither
    expect(within(grid).getAllByText(/Mới — khách thấy khi Xuất bản/)).toHaveLength(1);
  });

  // specs/063 S7-9 (T132): a delete on a delivered workspace leaves the field in the list, marked, with Hoàn tác.
  it("marks a field 'Sẽ xoá khi xuất bản' and Hoàn tác discards that pending change, then re-reads the list", async () => {
    listFields.mockResolvedValue([phone, { ...email, draftState: "deleted" }]);
    renderEditor();
    const grid = await screen.findByRole("grid", { name: "Danh sách trường" });
    await within(grid).findByRole("button", { name: "Email" });
    expect(within(grid).getByText("Sẽ xoá khi xuất bản")).toBeInTheDocument();
    expect(within(grid).getAllByText("Sẽ xoá khi xuất bản")).toHaveLength(1); // the live field carries no mark
    undoDraftDelete.mockImplementation(async () => { listFields.mockResolvedValue([phone, email]); return true; });
    fireEvent.click(within(grid).getByRole("button", { name: "Hoàn tác việc xoá trường Email" }));
    await waitFor(() => expect(undoDraftDelete).toHaveBeenCalledWith("field", "f-email"));
    await waitFor(() => expect(within(grid).queryByText("Sẽ xoá khi xuất bản")).toBeNull());
  });

  it("a delete that is only a draft tells the maker the customer keeps the field until Xuất bản", async () => {
    renderEditor();
    await openField("Điện thoại");
    fireEvent.click(screen.getByRole("button", { name: "Xoá trường" }));
    const prompt = await screen.findByText(/Xoá trường “/).then((n) => n.closest("[tabindex]") as HTMLElement);
    await within(prompt).findByText("Bảng chưa có bản ghi nào");
    // the server answers 204 but the field is still there, marked
    listFields.mockResolvedValue([{ ...phone, draftState: "deleted" }, email]);
    fireEvent.click(within(prompt).getByRole("button", { name: "Xoá trường" }));
    await waitFor(() => expect(toastInfo).toHaveBeenCalledWith("Trường “Điện thoại” sẽ bị xoá khi xuất bản. Khách vẫn dùng bình thường tới lúc đó."));
  });

  it("a failed delete says 'Xoá chưa xong' and Thử lại sends it again (T368)", async () => {
    renderEditor();
    await openField("Điện thoại");
    deleteField.mockRejectedValueOnce(new Error("boom"));
    fireEvent.click(screen.getByRole("button", { name: "Xoá trường" }));
    const prompt = await screen.findByText(/Xoá trường “/).then((n) => n.closest("[tabindex]") as HTMLElement);
    await within(prompt).findByText("Bảng chưa có bản ghi nào");
    fireEvent.click(within(prompt).getByRole("button", { name: "Xoá trường" }));
    expect(await screen.findByText(/Xoá chưa xong/)).toBeInTheDocument();
    deleteField.mockResolvedValueOnce(undefined);
    fireEvent.click(screen.getByRole("button", { name: "Thử lại" }));
    await waitFor(() => expect(deleteField).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryByText(/Xoá chưa xong/)).toBeNull());
  });

  it("says 'Áp ngay' beside the index switch only once the workspace is delivered", async () => {
    getPublishState.mockResolvedValue({ delivered: true });
    renderEditor();
    await openField("Điện thoại");
    await screen.findByRole("switch", { name: /Lọc nhanh/ });
    expect(await screen.findByText("Áp ngay")).toBeInTheDocument();
  });

  it("no 'Áp ngay' on a workspace that is not delivered", async () => {
    renderEditor();
    await openField("Điện thoại");
    await screen.findByRole("switch", { name: /Bắt buộc/ });
    expect(screen.queryByText("Áp ngay")).not.toBeInTheDocument();
  });

  it("shows the server's hint beside a refused edit", async () => {
    updateField.mockRejectedValue(new ApiError(422, "FIELD_TYPE_IMMUTABLE", "không thể đổi kiểu field", { hint: "Thêm một trường mới đúng kiểu." }));
    renderEditor();
    const labelInput = await openField("Điện thoại");
    fireEvent.change(labelInput, { target: { value: "X" } });
    fireEvent.click(saveButton());
    // visible text in an alert, not a tooltip on a truncated span
    expect(await screen.findByRole("alert")).toHaveTextContent(/Thêm một trường mới đúng kiểu\./);
  });

  it("states the way to change a type as static help on the type cell, plus the Publish note once delivered", async () => {
    getPublishState.mockResolvedValue({ delivered: true });
    renderEditor();
    await openField("Điện thoại");
    expect(await screen.findByText(/Muốn đổi kiểu\? Tạo trường mới đúng kiểu.*Khách chỉ thấy khi Xuất bản\./)).toBeInTheDocument();
  });
});

describe("EntityFieldEditor — add strip keyboard", () => {
  it("Esc closes the strip without a dialog; focus lands on the first input", async () => {
    renderEditor();
    const strip = await openAddField();
    expect(strip.querySelector("input")).toBeInstanceOf(HTMLInputElement);
    fireEvent.keyDown(strip, { key: "Escape" });
    expect(screen.queryByRole("region", { name: "Thêm trường mới" })).toBeNull();
  });
});
