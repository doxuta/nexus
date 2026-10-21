// LayoutDetail on SLDS (specs/038 T023) — the record page is the RecordHomePage template:
//   • page-headers/record-home: object label + record title in the h1, highlight fields in the
//     detail row (layout's first section, never a hard-coded field name);
//   • header actions follow the grants (Sửa/Nhân bản/Xoá hidden for a view-only user, In and
//     Đổi chủ always there — authorization for those two is server-side);
//   • Xoá asks in a modals/base dialog and only calls the API from the dialog's confirm button;
//     a refusal keeps the dialog open with the server's own sentence;
//   • the six record tabs stay (Tổng quan/Trao đổi/Quan hệ/Tệp/Lịch sử/Duyệt), and Tổng quan
//     renders each layout section as an expandable-section around a record-detail form.
// lib/api + useAuth + permissions mocked at the module seam (LayoutDetail.print.test.tsx pattern).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { LayoutDetail } from "./LayoutDetail";
import {
  ensureAuth, getLayout, listFields, getRecord, listEntities, deleteRecord, ApiError,
  listUserDirectory, listRecordShares, listRecordNotes, listRecordApprovals, listStartableProcesses,
  type EntityLayout, type FieldMeta, type ApiRecord, type EntityMeta,
} from "@/lib/api";

vi.mock("@/lib/auth/AuthProvider", () => ({
  useAuth: () => ({ user: { id: "u1", isAdmin: false } }),
}));

let granted = true;
vi.mock("@/lib/metadata/permissions", () => ({
  usePermissions: () => ({ can: () => granted, grants: [], isLoading: false, isError: false }),
}));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(),
    getLayout: vi.fn(),
    listFields: vi.fn(),
    getRecord: vi.fn(),
    listEntities: vi.fn(),
    deleteRecord: vi.fn(),
    listUserDirectory: vi.fn(),
    listRecordShares: vi.fn(),
    listRecordNotes: vi.fn(),
    listRecordApprovals: vi.fn(),
    listStartableProcesses: vi.fn(),
  };
});

const FIELDS: FieldMeta[] = [
  { name: "name", label: "Tiêu đề báo giá", fieldType: "text" },
  { name: "status", label: "Trạng thái", fieldType: "text" },
];
const LAYOUT: EntityLayout = {
  entityName: "crm_quotes",
  views: { edit: {}, list: {}, detail: { sections: [{ title: "Thông tin", fields: ["name", "status"] }] } },
};
const RECORD: ApiRecord = { id: "q1", version: 1, data: { name: "Báo giá quý 3", status: "Đã gửi" } };
// Object label deliberately NOT a substring of the record title, so the h1 check below can only pass
// when the object-label span is really rendered.
const ENTITIES: EntityMeta[] = [{ name: "crm_quotes", label: "Chào giá", tableName: "obj_crm_quotes" }];

function renderDetail(onDeleted = vi.fn()) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <QueryClientProvider client={qc}>
      <LayoutDetail entity="crm_quotes" recordId="q1" onEdit={() => {}} onClone={() => {}} onDeleted={onDeleted} />
    </QueryClientProvider>,
  );
  return { ...utils, onDeleted };
}

async function openOverflowAndPick(label: string) {
  screen.getByRole("button", { name: "Thêm thao tác" }).focus();
  await userEvent.keyboard("{Enter}");
  await userEvent.click(await screen.findByRole("menuitem", { name: label }));
}

beforeEach(() => {
  granted = true;
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(getLayout).mockReset().mockResolvedValue(LAYOUT);
  vi.mocked(listFields).mockReset().mockResolvedValue(FIELDS);
  vi.mocked(getRecord).mockReset().mockResolvedValue(RECORD);
  vi.mocked(listEntities).mockReset().mockResolvedValue(ENTITIES);
  vi.mocked(deleteRecord).mockReset();
  vi.mocked(listUserDirectory).mockReset().mockResolvedValue([]);
  vi.mocked(listRecordShares).mockReset().mockResolvedValue([]);
  vi.mocked(listStartableProcesses).mockReset().mockResolvedValue([]);
});

describe("LayoutDetail — SLDS record home", () => {
  it("renders the record-home header: object label + record title, highlight fields from the first section", async () => {
    const { container } = renderDetail();
    const h1 = await screen.findByRole("heading", { level: 1, name: /Báo giá quý 3/ });
    expect(h1.closest(".slds-page-header")).toHaveClass("slds-page-header_record-home");
    expect(await within(h1).findByText("Chào giá")).toBeInTheDocument();

    const details = container.querySelector(".slds-page-header__detail-row") as HTMLElement;
    expect(details).not.toBeNull();
    expect(within(details).getByText("Trạng thái")).toBeInTheDocument();
    expect(within(details).getByText("Đã gửi")).toBeInTheDocument();
  });

  it("keeps the six record tabs", async () => {
    renderDetail();
    await screen.findByRole("heading", { level: 1, name: /Báo giá quý 3/ });
    const names = screen.getAllByRole("tab").map((t) => t.textContent);
    expect(names).toEqual(["Tổng quan", "Trao đổi", "Quan hệ", "Tệp", "Lịch sử", "Duyệt"]);
  });

  it("Tổng quan renders each layout section as an expandable section around a record-detail form", async () => {
    const { container } = renderDetail();
    const toggle = await screen.findByRole("button", { name: "Thông tin" });
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    const form = container.querySelector(".slds-section .slds-form[role=list]") as HTMLElement;
    expect(form).not.toBeNull();
    expect(within(form).getByText("Tiêu đề báo giá")).toBeInTheDocument();
    expect(within(form).getByText("Báo giá quý 3")).toBeInTheDocument();
  });

  it("a view-only user gets no Sửa / Nhân bản / Xoá, but still In and Đổi chủ", async () => {
    granted = false;
    renderDetail();
    expect(await screen.findByTitle("In chứng từ này")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Đổi chủ" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Sửa" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Nhân bản" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Thêm thao tác" })).toBeNull(); // Xoá lives there
    // …and not as a visible button either (3 actions would fit without the overflow menu).
    expect(screen.queryByRole("button", { name: "Xoá" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Xoá" })).toBeNull();
  });

  // modals/base: Esc "moves focus to whatever triggered the modal to open". The overflow item that
  // opened it is gone by then, so focus must land on the "Thêm thao tác" button, not <body>.
  it("Xoá opened from the overflow menu: Esc returns focus to the menu button", async () => {
    renderDetail();
    await screen.findByRole("heading", { level: 1, name: /Báo giá quý 3/ });
    const more = screen.getByRole("button", { name: "Thêm thao tác" });

    await openOverflowAndPick("Xoá");
    await screen.findByRole("dialog", { name: "Xoá bản ghi?" });
    await userEvent.keyboard("{Escape}");

    expect(screen.queryByRole("dialog")).toBeNull();
    await waitFor(() => expect(more).toHaveFocus());
    expect(deleteRecord).not.toHaveBeenCalled();
  });

  it("Xoá asks in a modal and deletes only from the modal's confirm button", async () => {
    vi.mocked(deleteRecord).mockResolvedValue(undefined);
    const { onDeleted } = renderDetail();
    await screen.findByRole("heading", { level: 1, name: /Báo giá quý 3/ });

    await openOverflowAndPick("Xoá");
    const dialog = await screen.findByRole("dialog", { name: "Xoá bản ghi?" });
    expect(deleteRecord).not.toHaveBeenCalled();

    await userEvent.click(within(dialog).getByRole("button", { name: "Xoá" }));
    await waitFor(() => expect(deleteRecord).toHaveBeenCalledWith("crm_quotes", "q1"));
    await waitFor(() => expect(onDeleted).toHaveBeenCalled());
  });

  it("a refused delete keeps the modal open with the server's message", async () => {
    vi.mocked(deleteRecord).mockRejectedValue(new ApiError(403, "FORBIDDEN", "Bạn không có quyền xoá bản ghi này"));
    const { onDeleted } = renderDetail();
    await screen.findByRole("heading", { level: 1, name: /Báo giá quý 3/ });

    await openOverflowAndPick("Xoá");
    const dialog = await screen.findByRole("dialog", { name: "Xoá bản ghi?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Xoá" }));

    expect(await within(dialog).findByText("Bạn không có quyền xoá bản ghi này")).toBeInTheDocument();
    expect(onDeleted).not.toHaveBeenCalled();
  });

  // specs/048 T138: a before-delete rule refuses the delete — the dialog stays open with the
  // rule's own sentence (no error code), and the record is not reported deleted.
  it("a before-delete rule's refusal keeps the dialog open with the rule's sentence", async () => {
    vi.mocked(deleteRecord).mockRejectedValue(
      new ApiError(422, "RECORD_HOOK_VETO", "Không xoá báo giá đã gửi khách", {
        violations: [{ message: "Không xoá báo giá đã gửi khách" }],
      }),
    );
    const { onDeleted } = renderDetail();
    await screen.findByRole("heading", { level: 1, name: /Báo giá quý 3/ });

    await openOverflowAndPick("Xoá");
    const dialog = await screen.findByRole("dialog", { name: "Xoá bản ghi?" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Xoá" }));

    expect(await within(dialog).findByText("Không xoá báo giá đã gửi khách")).toBeInTheDocument();
    expect(dialog.textContent).not.toMatch(/RECORD_HOOK_VETO/);
    expect(onDeleted).not.toHaveBeenCalled();
  });

  // specs/051 · DAILY-QUEUE #35: "Chia sẻ" sits with the other record actions (overflow — Sửa, Nhân
  // bản, In, Đổi chủ stay visible) and opens the share dialog, which reads the live share list.
  it("an editor finds Chia sẻ in the actions; it opens the share dialog", async () => {
    renderDetail();
    await screen.findByRole("heading", { level: 1, name: /Báo giá quý 3/ });
    await openOverflowAndPick("Chia sẻ");
    expect(await screen.findByRole("dialog", { name: "Chia sẻ hồ sơ" })).toBeInTheDocument();
    await waitFor(() => expect(listRecordShares).toHaveBeenCalledWith("crm_quotes", "q1"));
  });

  it("a view-only user gets no Chia sẻ", async () => {
    granted = false;
    renderDetail();
    await screen.findByTitle("In chứng từ này");
    expect(screen.queryByRole("button", { name: "Chia sẻ" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Thêm thao tác" })).toBeNull();
  });

  // DAILY-QUEUE #223: a record seen ONLY through a share is read-only. Even with an entity-level edit
  // grant, the page says so on open and offers no Sửa / Xoá / Đổi chủ / Chia sẻ (each would be refused
  // with 403 RECORD_SHARED_READ_ONLY); read-only actions stay. The server marks it canEdit:false (#43:
  // the page decides from canEdit, sharedWithMe only picks the sentence).
  it("a record shared with me outside my edit scope opens read-only: notice, no write actions", async () => {
    vi.mocked(getRecord).mockResolvedValue({ ...RECORD, sharedWithMe: true, canEdit: false });
    renderDetail();
    expect(await screen.findByText("Hồ sơ này được chia sẻ để xem — bạn không sửa hay xoá được.")).toBeInTheDocument();
    expect(screen.getByTitle("In chứng từ này")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Nhân bản" })).toBeInTheDocument();
    for (const name of ["Sửa", "Xoá", "Đổi chủ", "Chia sẻ", "Thêm thao tác"]) {
      expect(screen.queryByRole("button", { name })).toBeNull();
    }
  });

  // DAILY-QUEUE #334: edit=own on a colleague's record — the entity-level grant says yes (mayEdit) but the
  // server says canEdit:false for THIS record, so no Sửa / Chia sẻ (the save would be refused 403, #18) and a reason is shown.
  it("a record outside my edit scope (canEdit:false) opens without Sửa / Chia sẻ / Đổi chủ and says why", async () => {
    vi.mocked(getRecord).mockResolvedValue({ ...RECORD, canEdit: false });
    renderDetail();
    expect(await screen.findByText("Hồ sơ này thuộc người khác — bạn xem được nhưng không sửa được.")).toBeInTheDocument();
    expect(screen.getByTitle("In chứng từ này")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Nhân bản" })).toBeInTheDocument();
    for (const name of ["Sửa", "Chia sẻ", "Đổi chủ"]) {
      expect(screen.queryByRole("button", { name })).toBeNull();
    }
  });

  it("a record with no canEdit key (own record / edit=all / admin) keeps Sửa and shows no notice", async () => {
    renderDetail();
    await screen.findByTitle("In chứng từ này");
    expect(screen.getByRole("button", { name: "Sửa" })).toBeInTheDocument();
    expect(screen.queryByText("Hồ sơ này thuộc người khác — bạn xem được nhưng không sửa được.")).toBeNull();
  });

  // DAILY-QUEUE #251: the Trao đổi / Tệp / Duyệt tabs get the same flag, so their write controls are
  // gone too (the server would refuse each of them).
  it("a shared-with-me record: the Trao đổi, Tệp and Duyệt tabs offer no write control", async () => {
    vi.mocked(getRecord).mockResolvedValue({ ...RECORD, sharedWithMe: true, canEdit: false });
    vi.mocked(listFields).mockResolvedValue([...FIELDS, { name: "docs", label: "Tài liệu", fieldType: "file" }]);
    vi.mocked(listRecordNotes).mockResolvedValue({ notes: [], pagination: { page: 1, limit: 20, total: 0, totalPages: 1 } });
    vi.mocked(listRecordApprovals).mockResolvedValue([]);
    const { container } = renderDetail();
    await screen.findByText("Hồ sơ này được chia sẻ để xem — bạn không sửa hay xoá được.");

    await userEvent.click(screen.getByRole("tab", { name: "Trao đổi" }));
    await screen.findByText("Chưa có trao đổi nào");
    expect(screen.queryByRole("textbox")).toBeNull();

    await userEvent.click(screen.getByRole("tab", { name: "Tệp" }));
    await screen.findByText(/Tài liệu/);
    expect(container.querySelector('input[type="file"]')).toBeNull();

    await userEvent.click(screen.getByRole("tab", { name: "Duyệt" }));
    await screen.findByText("Chưa gửi duyệt");
    expect(screen.queryByRole("button", { name: "Gửi duyệt" })).toBeNull();
  });

  // #43: role view=own / edit=team, a TEAM record that is also shared with me. GET says sharedWithMe
  // (my view scope reaches it only through the share) but no canEdit:false (my edit scope reaches it;
  // PUT answers 200). A share grants a read and takes nothing away, so this is an editable record: Sửa,
  // no "shared for viewing" notice, upload and "Gửi duyệt" offered. Only Trao đổi stays read-only — a
  // note's floor is the VIEW scope without the share (specs/060 #349), and the server refuses it.
  it("a shared record my edit scope reaches (sharedWithMe, no canEdit:false) stays editable", async () => {
    vi.mocked(getRecord).mockResolvedValue({ ...RECORD, sharedWithMe: true });
    vi.mocked(listFields).mockResolvedValue([...FIELDS, { name: "docs", label: "Tài liệu", fieldType: "file" }]);
    vi.mocked(listRecordNotes).mockResolvedValue({ notes: [], pagination: { page: 1, limit: 20, total: 0, totalPages: 1 } });
    vi.mocked(listRecordApprovals).mockResolvedValue([]);
    const { container } = renderDetail();
    expect(await screen.findByRole("button", { name: "Sửa" })).toBeInTheDocument();
    expect(screen.queryByText("Hồ sơ này được chia sẻ để xem — bạn không sửa hay xoá được.")).toBeNull();
    expect(screen.queryByText("Hồ sơ này thuộc người khác — bạn xem được nhưng không sửa được.")).toBeNull();

    await userEvent.click(screen.getByRole("tab", { name: "Tệp" }));
    await screen.findByText(/Tài liệu/);
    expect(container.querySelector('input[type="file"]')).not.toBeNull();

    await userEvent.click(screen.getByRole("tab", { name: "Duyệt" }));
    expect(await screen.findByRole("button", { name: "Gửi duyệt" })).toBeInTheDocument();

    await userEvent.click(screen.getByRole("tab", { name: "Trao đổi" }));
    await screen.findByText("Chưa có trao đổi nào");
    expect(screen.queryByRole("textbox")).toBeNull();
  });

  // #43 verifier: view=own, edit=team, delete=own, a team record shared with me. canEdit is absent (the
  // edit scope reaches it) but the DELETE runs under delete=own and answers 403 RECORD_SHARED_READ_ONLY —
  // so Xoá stays off for a sharedWithMe record whatever canEdit says, while Sửa and Chia sẻ stay.
  it("a shared record my edit scope reaches still offers no Xoá (the delete scope is not canEdit)", async () => {
    vi.mocked(getRecord).mockResolvedValue({ ...RECORD, sharedWithMe: true });
    renderDetail();
    expect(await screen.findByRole("button", { name: "Sửa" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Xoá" })).toBeNull();
    screen.getByRole("button", { name: "Thêm thao tác" }).focus();
    await userEvent.keyboard("{Enter}");
    expect(await screen.findByRole("menuitem", { name: "Chia sẻ" })).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: "Xoá" })).toBeNull();
  });
});

// specs/054 · DAILY-QUEUE #24: the record page honours the detail view's visibleWhen with the SAME
// evaluator as the form (lib/visibleWhen). Q1 keeps a hidden field's stored value, so the record
// below still carries tax_code — the page must not show it (Tổng quan, header row), and a section
// whose every field is hidden does not leave an empty titled box behind.
describe("LayoutDetail — conditional cells (visibleWhen)", () => {
  const LEAD_FIELDS: FieldMeta[] = [
    { name: "full_name", label: "Họ tên", fieldType: "text" },
    { name: "customer_type", label: "Loại khách", fieldType: "picklist", options: { values: ["individual", "business"] } },
    { name: "tax_code", label: "Mã số thuế", fieldType: "text" },
    { name: "company_size", label: "Quy mô", fieldType: "text" },
  ];
  const BUSINESS = { field: "customer_type", op: "eq", value: "business" } as const;
  const LEAD_LAYOUT: EntityLayout = {
    entityName: "crm_leads",
    views: {
      edit: {},
      list: {},
      detail: {
        sections: [
          { title: "Thông tin", columns: 2, fields: ["full_name", "customer_type", { field: "tax_code", visibleWhen: BUSINESS }] },
          { title: "Doanh nghiệp", fields: [{ field: "company_size", visibleWhen: BUSINESS }] },
        ],
      },
    },
  };
  const lead = (customerType: string): ApiRecord => ({
    id: "q1",
    version: 3,
    data: { full_name: "Nguyễn An", customer_type: customerType, tax_code: "0101234567", company_size: "50 người" },
  });

  beforeEach(() => {
    vi.mocked(getLayout).mockResolvedValue(LEAD_LAYOUT);
    vi.mocked(listFields).mockResolvedValue(LEAD_FIELDS);
  });

  it("condition not met: the hidden field shows nowhere — not in Tổng quan, not in the header row", async () => {
    vi.mocked(getRecord).mockResolvedValue(lead("individual"));
    const { container } = renderDetail();
    await screen.findByRole("button", { name: "Thông tin" });
    const form = container.querySelector(".slds-section .slds-form[role=list]") as HTMLElement;
    expect(within(form).getByText("Loại khách")).toBeInTheDocument();
    expect(screen.queryByText("Mã số thuế")).toBeNull();
    expect(screen.queryByText("0101234567")).toBeNull();
    expect(screen.queryByRole("button", { name: "Doanh nghiệp" })).toBeNull();
    expect(screen.queryByText("50 người")).toBeNull();
    const header = container.querySelector(".slds-page-header__detail-row") as HTMLElement;
    expect(within(header).getByText("Loại khách")).toBeInTheDocument();
  });

  it("condition met: the same cells show, in Tổng quan and in the header row", async () => {
    vi.mocked(getRecord).mockResolvedValue(lead("business"));
    const { container } = renderDetail();
    await screen.findByRole("button", { name: "Doanh nghiệp" });
    const header = container.querySelector(".slds-page-header__detail-row") as HTMLElement;
    expect(within(header).getByText("0101234567")).toBeInTheDocument();
    const form = container.querySelector(".slds-section .slds-form[role=list]") as HTMLElement;
    expect(within(form).getByText("Mã số thuế")).toBeInTheDocument();
    expect(screen.getByText("50 người")).toBeInTheDocument();
  });
});
