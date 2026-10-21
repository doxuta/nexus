// LayoutForm on SLDS (specs/038 T024) — RecordFormPage: page header, one expandable section per
// layout section, docked footer (Huỷ · Lưu & tạo mới · Tạo/Lưu thay đổi); 422 lands on the field,
// 409 is a warning scoped notification with a reload, never a silent overwrite.
//
// Real LayoutForm + SectionGrid + CellRenderer + widgets + RecordFormPage; only the network seam and
// the app providers (auth, entity label, navigation blocker) are mocked.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { ApiError, type FieldMeta } from "@/lib/api";

const seam = vi.hoisted(() => ({
  getLayout: vi.fn(),
  listFields: vi.fn(),
  getRecord: vi.fn(),
  updateRecord: vi.fn(),
  createRecord: vi.fn(),
}));

vi.mock("@/lib/auth/AuthProvider", () => ({
  useAuth: () => ({ user: { id: "u1", isAdmin: false } }),
}));
vi.mock("@/lib/metadata/hooks", () => ({
  useEntityLabel: () => "Cơ hội",
}));
vi.mock("@/lib/navigation-blocker", () => ({
  useNavigationBlocker: () => ({ setBlocked: () => {}, confirmLeave: () => true }),
}));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn().mockResolvedValue(undefined),
    getLayout: (...args: unknown[]) => seam.getLayout(...args),
    getRecord: (...args: unknown[]) => seam.getRecord(...args),
    listFields: (...args: unknown[]) => seam.listFields(...args),
    updateRecord: (...args: unknown[]) => seam.updateRecord(...args),
    createRecord: (...args: unknown[]) => seam.createRecord(...args),
  };
});

import { LayoutForm } from "./LayoutForm";

const NAME: FieldMeta = { id: "f1", name: "name", label: "Tên cơ hội", fieldType: "text", isRequired: true };
const NOTE: FieldMeta = { id: "f2", name: "note", label: "Ghi chú", fieldType: "text" };

beforeEach(() => {
  seam.getLayout.mockReset().mockResolvedValue({
    entityName: "crm_opportunities",
    systemFields: [],
    views: {
      edit: {
        isDefault: true,
        sections: [
          { title: "Thông tin", columns: 2, fields: ["name"] },
          { title: "Khác", columns: 1, fields: ["note"] },
        ],
      },
    },
  });
  seam.listFields.mockReset().mockResolvedValue([NAME, NOTE]);
  seam.getRecord.mockReset().mockResolvedValue({ id: "opp-1", version: 3, data: { name: "Hợp đồng A", note: "" } });
  seam.updateRecord.mockReset().mockResolvedValue({ id: "opp-1", version: 4 });
  seam.createRecord.mockReset().mockResolvedValue({ id: "opp-new", version: 1 });
});

const footer = () => document.querySelector(".slds-docked-form-footer") as HTMLElement;

describe("LayoutForm — RecordFormPage (SLDS)", () => {
  it("create: page header, one expandable section per layout section, docked footer with the three actions", async () => {
    render(<LayoutForm entity="crm_opportunities" onCancel={() => {}} onSaved={() => {}} />);
    await screen.findByRole("textbox", { name: /Tên cơ hội/ });

    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Tạo Cơ hội");
    for (const title of ["Thông tin", "Khác"]) {
      const toggle = screen.getByRole("button", { name: title });
      expect(toggle).toHaveClass("slds-section__title-action");
      expect(toggle).toHaveAttribute("aria-expanded", "true");
    }
    // record-detail edit-stacked: slds-form[role=list] > row > item[role=listitem] > stacked element
    const item = screen.getByRole("textbox", { name: /Tên cơ hội/ }).closest(".slds-form__item");
    expect(item).toHaveAttribute("role", "listitem");
    expect(item?.closest(".slds-form")).toHaveAttribute("role", "list");

    const f = within(footer());
    expect(f.getByRole("button", { name: "Huỷ" })).toHaveClass("slds-button_neutral");
    expect(f.getByRole("button", { name: "Lưu & tạo mới" })).toHaveClass("slds-button_neutral");
    expect(f.getByRole("button", { name: "Tạo Cơ hội" })).toHaveClass("slds-button_brand");
  });

  it("zod: an empty required field fails at the field, nothing is posted", async () => {
    render(<LayoutForm entity="crm_opportunities" onCancel={() => {}} />);
    const input = await screen.findByRole("textbox", { name: /Tên cơ hội/ });
    fireEvent.click(within(footer()).getByRole("button", { name: "Tạo Cơ hội" }));
    await waitFor(() => expect(input).toHaveAttribute("aria-invalid", "true"));
    expect(input.closest(".slds-form-element")).toHaveClass("slds-has-error");
    expect(seam.createRecord).not.toHaveBeenCalled();
  });

  // Blueprint docked-form-footer: "When errors are found within a form, the user will be notified".
  // The fields use setValue (no registered ref), so react-hook-form cannot focus them itself: without
  // this a failed Lưu on a long form changes nothing on screen and a screen reader hears nothing.
  it("invalid submit: the form-level notification names the fields to fix and focus moves to the first one", async () => {
    render(<LayoutForm entity="crm_opportunities" onCancel={() => {}} />);
    const input = await screen.findByRole("textbox", { name: /Tên cơ hội/ });
    const save = within(footer()).getByRole("button", { name: "Tạo Cơ hội" });
    save.focus();
    fireEvent.click(save);
    await waitFor(() => expect(input).toHaveFocus());
    const region = document.querySelector("form > [role=status]")!;
    expect(region).toHaveClass("slds-scoped-notification", "slds-theme_error");
    expect(region).toHaveTextContent("Có 1 trường cần sửa: Tên cơ hội");

    // Fixed: the notification goes with the error.
    fireEvent.change(input, { target: { value: "Đơn A" } });
    await waitFor(() => expect(region).toBeEmptyDOMElement());
  });

  it("422 with a rendered field lands ON that field (its section held open)", async () => {
    seam.updateRecord.mockRejectedValue(
      new ApiError(422, "RECORD_TOO_LONG", "Ghi chú quá dài", { field: "note" }),
    );
    render(<LayoutForm entity="crm_opportunities" recordId="opp-1" />);
    const note = await screen.findByRole("textbox", { name: /Ghi chú/ });
    fireEvent.click(screen.getByRole("button", { name: "Khác" })); // collapse the section first
    fireEvent.click(within(footer()).getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(screen.getByText("Ghi chú quá dài")).toHaveClass("slds-form-element__help"));
    expect(note).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByRole("button", { name: "Khác" })).toHaveAttribute("aria-expanded", "true");
    await waitFor(() => expect(note).toHaveFocus());
  });

  it("409: warning notification with a reload — reload refetches and drops the local edit", async () => {
    seam.updateRecord.mockRejectedValue(
      new ApiError(409, "RECORD_VERSION_CONFLICT", "conflict", { currentVersion: 5 }),
    );
    render(<LayoutForm entity="crm_opportunities" recordId="opp-1" />);
    const name = await screen.findByRole("textbox", { name: /Tên cơ hội/ });
    fireEvent.change(name, { target: { value: "Bản của tôi" } });
    fireEvent.click(within(footer()).getByRole("button", { name: "Lưu thay đổi" }));

    const note = (await screen.findByText(/Bản ghi đã được người khác sửa/)).closest(".slds-scoped-notification")!;
    expect(note).toHaveClass("slds-theme_warning");
    expect(note).toHaveTextContent("5");
    seam.getRecord.mockResolvedValue({ id: "opp-1", version: 5, data: { name: "Bản của người khác", note: "" } });
    fireEvent.click(within(note as HTMLElement).getByRole("button", { name: "Tải lại bản mới nhất" }));
    await waitFor(() => expect(name).toHaveValue("Bản của người khác"));
    expect(seam.getRecord).toHaveBeenCalledTimes(2);
    expect(screen.queryByText(/Bản ghi đã được người khác sửa/)).toBeNull();
  });

  // specs/051 · DAILY-QUEUE #223/#35: a record seen only through a share is read-only. Reaching its edit
  // form (a pasted /edit link) says so up front; a save the server refuses shows the refusal's sentence.
  it("share-only record: the edit form says it is read-only on open", async () => {
    seam.getRecord.mockResolvedValue({ id: "opp-1", version: 3, data: { name: "Hợp đồng A", note: "" }, sharedWithMe: true });
    render(<LayoutForm entity="crm_opportunities" recordId="opp-1" />);
    const note = (await screen.findByText("Hồ sơ này được chia sẻ để xem — bạn không sửa hay xoá được."))
      .closest(".slds-scoped-notification");
    expect(note).toHaveClass("slds-scoped-notification_light"); // RecordFormPage's info tone
  });

  // DAILY-QUEUE #334: edit=own on a colleague's record (canEdit:false) — a pasted /edit link says so up front.
  it("record outside my edit scope (canEdit:false): the edit form says so on open", async () => {
    seam.getRecord.mockResolvedValue({ id: "opp-1", version: 3, data: { name: "Hợp đồng A", note: "" }, canEdit: false });
    render(<LayoutForm entity="crm_opportunities" recordId="opp-1" />);
    const note = (await screen.findByText("Hồ sơ này thuộc người khác — bạn xem được nhưng không sửa được."))
      .closest(".slds-scoped-notification");
    expect(note).toHaveClass("slds-scoped-notification_light");
    // #18: the server refuses that save (403), so Save is off from the start — the notice above says why.
    const save = within(footer()).getByRole("button", { name: "Lưu thay đổi" });
    expect(save).toBeDisabled();
    expect(within(footer()).getByRole("button", { name: "Huỷ" })).toBeEnabled();
    fireEvent.click(save);
    expect(seam.updateRecord).not.toHaveBeenCalled();
  });

  it("403 FORBIDDEN on save (edit scope moved since the form opened): the server's sentence in the form-level error", async () => {
    seam.updateRecord.mockRejectedValue(new ApiError(403, "FORBIDDEN", "Bạn không có quyền thực hiện thao tác này"));
    render(<LayoutForm entity="crm_opportunities" recordId="opp-1" />);
    await screen.findByRole("textbox", { name: /Tên cơ hội/ });
    const save = within(footer()).getByRole("button", { name: "Lưu thay đổi" });
    expect(save).toBeEnabled(); // no canEdit flag: nothing known yet
    fireEvent.click(save);
    const note = (await screen.findByText("Bạn không có quyền thực hiện thao tác này")).closest(".slds-scoped-notification");
    expect(note).toHaveClass("slds-theme_error");
  });

  it("403 RECORD_SHARED_READ_ONLY on save: the server's sentence in the form-level error", async () => {
    seam.updateRecord.mockRejectedValue(
      new ApiError(403, "RECORD_SHARED_READ_ONLY", "Hồ sơ này được chia sẻ để xem, không sửa được"),
    );
    render(<LayoutForm entity="crm_opportunities" recordId="opp-1" />);
    await screen.findByRole("textbox", { name: /Tên cơ hội/ });
    fireEvent.click(within(footer()).getByRole("button", { name: "Lưu thay đổi" }));
    const note = (await screen.findByText("Hồ sơ này được chia sẻ để xem, không sửa được")).closest(".slds-scoped-notification");
    expect(note).toHaveClass("slds-theme_error");
  });

  it("Lưu & tạo mới: creates, stays on a blank form, says so", async () => {
    const onSaved = vi.fn();
    render(<LayoutForm entity="crm_opportunities" onCancel={() => {}} onSaved={onSaved} />);
    const name = await screen.findByRole("textbox", { name: /Tên cơ hội/ });
    fireEvent.change(name, { target: { value: "Đơn 1" } });
    fireEvent.click(within(footer()).getByRole("button", { name: "Lưu & tạo mới" }));
    await waitFor(() => expect(seam.createRecord).toHaveBeenCalledTimes(1));
    expect(await screen.findByText("Đã tạo Cơ hội. Nhập bản ghi tiếp theo.")).toBeInTheDocument();
    expect(name).toHaveValue("");
    expect(onSaved).not.toHaveBeenCalled();
  });

  // Quick-create (contracts/shell.md: RecordFormPage in Modal form): the template's modal variant,
  // Huỷ / Tạo in slds-modal__footer (pinned under the scrolling content), never a docked footer
  // inside a dialog. One dialog from the spinner to the form.
  it("modal: one SLDS modal from the spinner to the form; Huỷ / Tạo in its footer, no docked footer", async () => {
    let resolveLayout: (v: unknown) => void = () => {};
    const layout = await seam.getLayout();
    seam.getLayout.mockReturnValue(new Promise((r) => (resolveLayout = r)));
    const onCancel = vi.fn();
    render(<LayoutForm entity="crm_opportunities" modal onCancel={onCancel} onSaved={() => {}} />);

    const dialog = screen.getByRole("dialog", { name: "Tạo Cơ hội" });
    expect(dialog).toHaveClass("slds-modal", "slds-modal_medium");
    expect(within(dialog).getByText("Đang tải biểu mẫu…")).toBeInTheDocument();

    resolveLayout(layout);
    const input = await within(dialog).findByRole("textbox", { name: /Tên cơ hội/ });
    expect(screen.getByRole("dialog", { name: "Tạo Cơ hội" })).toBe(dialog);
    expect(input.closest(".slds-modal__content")).not.toBeNull();
    expect(document.querySelector(".slds-docked-form-footer")).toBeNull();
    const modalFooter = dialog.querySelector(".slds-modal__footer") as HTMLElement;
    expect(within(modalFooter).getAllByRole("button").map((b) => b.textContent)).toEqual(["Huỷ", "Tạo Cơ hội"]);

    fireEvent.change(input, { target: { value: "Đơn nhanh" } });
    fireEvent.click(within(modalFooter).getByRole("button", { name: "Tạo Cơ hội" }));
    await waitFor(() => expect(seam.createRecord).toHaveBeenCalledWith("crm_opportunities", { name: "Đơn nhanh" }));

    fireEvent.click(within(modalFooter).getByRole("button", { name: "Huỷ" }));
    expect(onCancel).toHaveBeenCalledOnce();
  });

  // Esc belongs to the innermost popup (ARIA combobox / datepicker): closing an open list must not
  // also close the quick-create and throw away what was typed.
  it("modal: Esc on an open picklist closes the list only; the next Esc cancels", async () => {
    const STAGE: FieldMeta = {
      id: "f3", name: "stage", label: "Giai đoạn", fieldType: "picklist", options: { values: ["new", "won"] },
    } as FieldMeta;
    seam.listFields.mockResolvedValue([NAME, NOTE, STAGE]);
    seam.getLayout.mockResolvedValue({
      entityName: "crm_opportunities",
      systemFields: [],
      views: { edit: { isDefault: true, sections: [{ title: "Thông tin", columns: 1, fields: ["name", "stage"] }] } },
    });
    const onCancel = vi.fn();
    render(<LayoutForm entity="crm_opportunities" modal onCancel={onCancel} />);
    const stage = await screen.findByRole("combobox", { name: "Giai đoạn" });
    fireEvent.click(stage);
    expect(await screen.findByRole("listbox")).toBeInTheDocument();

    fireEvent.keyDown(stage, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("listbox")).toBeNull());
    expect(onCancel).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog", { name: "Tạo Cơ hội" })).toBeInTheDocument();

    fireEvent.keyDown(stage, { key: "Escape" });
    expect(onCancel).toHaveBeenCalledOnce();
  });
});

// specs/052-dependent-picklist (FE-dependent-picklist): the child lists only what the chosen parent
// allows, is locked while the parent is empty, and a parent change blanks a child it no longer
// allows (an edit sends it as null). The server still judges the pair: its 422 lands on the child.
describe("LayoutForm — dependent picklist (Tỉnh → Quận/Huyện)", () => {
  const PROVINCE = {
    id: "fp", name: "province", label: "Tỉnh/TP", fieldType: "picklist", options: { values: ["Hà Nội", "Đà Nẵng"] },
  } as FieldMeta;
  const DISTRICT = {
    id: "fd", name: "district", label: "Quận/Huyện", fieldType: "picklist",
    options: {
      values: ["Ba Đình", "Hoàn Kiếm", "Hải Châu"],
      controllingField: "province",
      dependentValues: { "Hà Nội": ["Ba Đình", "Hoàn Kiếm"], "Đà Nẵng": ["Hải Châu"] },
    },
  } as FieldMeta;

  beforeEach(() => {
    seam.listFields.mockResolvedValue([NAME, PROVINCE, DISTRICT]);
    // Child in ANOTHER section than its parent: the blanking crosses sections.
    seam.getLayout.mockResolvedValue({
      entityName: "crm_contacts",
      systemFields: [],
      views: {
        edit: {
          isDefault: true,
          sections: [
            { title: "Thông tin", columns: 1, fields: ["name", "province"] },
            { title: "Địa chỉ", columns: 1, fields: ["district"] },
          ],
        },
      },
    });
  });

  const pick = async (label: string, option: string) => {
    fireEvent.click(screen.getByRole("combobox", { name: label }));
    fireEvent.click(await screen.findByRole("option", { name: option }));
  };
  const optionsOf = async (label: string) => {
    fireEvent.click(screen.getByRole("combobox", { name: label }));
    const list = await screen.findByRole("listbox");
    const names = within(list).queryAllByRole("option").map((o) => o.textContent);
    fireEvent.keyDown(screen.getByRole("combobox", { name: label }), { key: "Escape" });
    return names;
  };

  it("create: the child is locked until the parent is chosen, then lists only that parent's children", async () => {
    render(<LayoutForm entity="crm_contacts" onCancel={() => {}} />);
    const district = await screen.findByRole("combobox", { name: "Quận/Huyện" });
    expect(district).toHaveAttribute("aria-disabled", "true");
    expect(district).toHaveTextContent("Chọn Tỉnh/TP trước");

    await pick("Tỉnh/TP", "Hà Nội");
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Quận/Huyện" })).not.toHaveAttribute("aria-disabled"));
    expect(await optionsOf("Quận/Huyện")).toEqual(["Ba Đình", "Hoàn Kiếm"]);
  });

  it("edit: switching the parent blanks a child the new parent does not allow, and the save sends it as null", async () => {
    seam.getRecord.mockResolvedValue({ id: "c-1", version: 2, data: { name: "An", province: "Hà Nội", district: "Ba Đình" } });
    render(<LayoutForm entity="crm_contacts" recordId="c-1" />);
    const district = await screen.findByRole("combobox", { name: "Quận/Huyện" });
    expect(district).toHaveTextContent("Ba Đình");

    await pick("Tỉnh/TP", "Đà Nẵng");
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Quận/Huyện" })).not.toHaveTextContent("Ba Đình"));
    expect(await optionsOf("Quận/Huyện")).toEqual(["Hải Châu"]);

    fireEvent.click(within(footer()).getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(seam.updateRecord).toHaveBeenCalledTimes(1));
    expect(seam.updateRecord.mock.calls[0][2]).toMatchObject({ province: "Đà Nẵng", district: null });
  });

  it("edit: a record holding a pair from before the config opens untouched and saves it back as it is", async () => {
    seam.getRecord.mockResolvedValue({ id: "c-2", version: 1, data: { name: "Bình", province: "Hà Nội", district: "Hải Châu" } });
    render(<LayoutForm entity="crm_contacts" recordId="c-2" />);
    expect(await screen.findByRole("combobox", { name: "Quận/Huyện" })).toHaveTextContent("Hải Châu");
    fireEvent.change(screen.getByRole("textbox", { name: /Tên cơ hội/ }), { target: { value: "Bình 2" } });
    fireEvent.click(within(footer()).getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(seam.updateRecord).toHaveBeenCalledTimes(1));
    expect(seam.updateRecord.mock.calls[0][2]).toMatchObject({ name: "Bình 2", province: "Hà Nội", district: "Hải Châu" });
  });

  it("422 RECORD_DEPENDENT_PICKLIST_INVALID lands under the child field", async () => {
    const message = "\"Quận/Huyện\" = \"Hải Châu\" không thuộc \"Tỉnh/TP\" = \"Hà Nội\"";
    seam.createRecord.mockRejectedValue(new ApiError(422, "RECORD_DEPENDENT_PICKLIST_INVALID", message, { field: "district" }));
    render(<LayoutForm entity="crm_contacts" onCancel={() => {}} />);
    fireEvent.change(await screen.findByRole("textbox", { name: /Tên cơ hội/ }), { target: { value: "An" } });
    await pick("Tỉnh/TP", "Hà Nội");
    await pick("Quận/Huyện", "Ba Đình");
    fireEvent.click(within(footer()).getByRole("button", { name: "Tạo Cơ hội" }));
    expect(await screen.findByText(message)).toHaveClass("slds-form-element__help");
    expect(screen.getByRole("combobox", { name: "Quận/Huyện" })).toHaveAttribute("aria-invalid", "true");
  });
});
