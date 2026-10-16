// Component tests — /admin/field-acl on SLDS (specs/038 T048).
// Pins the editor contract on the new markup: every field × role cell is a radio-button-group
// (Sửa / Chỉ xem / Ẩn) named "<field> · <role>"; the stored grants pre-select the cells; Save PUTs
// EVERY changed cell explicitly (a cell set back to "Sửa" sends (true,true) so the backend clears
// it) and nothing else; switching entity with unsaved changes asks first.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { FieldACL } from "@/components/screens/admin/FieldACL";
import { logout } from "@/lib/api";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const ENTITIES = [
  { name: "crm_accounts", label: "Khách hàng" },
  { name: "crm_contacts", label: "Liên hệ" },
];
const ROLES = [
  { id: "r-sales", name: "sales", label: "Kinh doanh" },
  { id: "r-support", name: "support", label: "Hỗ trợ" },
];
const FIELDS = [
  { name: "name", label: "Tên", fieldType: "text", isRequired: true },
  { name: "phone", label: "Số điện thoại", fieldType: "phone" },
];

const GRANTS = [
  { roleId: "r-sales", entityName: "crm_accounts", fieldName: "phone", canView: true, canEdit: false },
];

// `deadEnd` = the server's `deadEndIfLocked` (queue #306): the matrix paints exactly these fields.
function routedFetch(puts: unknown[], putRes?: () => Response, fields: unknown[] = FIELDS, grants: unknown[] = GRANTS, deadEnd: string[] = []) {
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    if (u.endsWith("/admin/publish/state")) return jsonRes({ data: { delivered } });
    if (u.includes("/metadata/entities")) return jsonRes({ data: ENTITIES });
    if (u.includes("/admin/roles")) return jsonRes({ data: { roles: ROLES } });
    if (u.includes("/metadata/fields/")) return jsonRes({ data: fields });
    if (u.includes("/admin/field-acl") && method === "GET") {
      return jsonRes({ data: { defaultPolicy: "allow", grants, deadEndIfLocked: deadEnd } });
    }
    if (u.includes("/admin/field-acl") && method === "PUT") {
      puts.push(JSON.parse(String(init?.body)));
      return putRes ? putRes() : jsonRes({ data: { applied: 1, cleared: 0 } });
    }
    return jsonRes({ error: `unmocked ${method} ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
}

let delivered = false; // GET /admin/publish/state
const cell = (name: string) => screen.getByRole("radiogroup", { name });

describe("FieldACL (SLDS)", () => {
  let originalFetch: FetchFn;

  beforeEach(async () => {
    delivered = false;
    originalFetch = global.fetch as unknown as FetchFn;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout();
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("renders one radio-button-group per field × role, pre-selected from the stored grants", async () => {
    global.fetch = routedFetch([]) as unknown as typeof fetch;
    render(<FieldACL />);

    await waitFor(() => expect(cell("Số điện thoại · Kinh doanh")).toBeInTheDocument());
    expect(screen.getAllByRole("radiogroup")).toHaveLength(FIELDS.length * ROLES.length);
    expect(within(cell("Số điện thoại · Kinh doanh")).getByRole("radio", { name: "Chỉ xem" })).toBeChecked();
    expect(within(cell("Tên · Hỗ trợ")).getByRole("radio", { name: "Sửa" })).toBeChecked();
    // Legend in the table header: the roles are the columns.
    expect(screen.getByRole("columnheader", { name: "Kinh doanh" })).toBeInTheDocument();
    // Nothing changed yet → Save is disabled.
    expect(screen.getByRole("button", { name: /^Lưu$/ })).toBeDisabled();
  });

  // specs/073 kh3 T232: the Studio route has no ?entity=&field= in the URL (it uses ?bang=&truong=), so the page
  // hands the pair over as props. Without props the old one-shot URL read stays.
  it("props entity/field preselect the table and light the field row (no URL needed)", async () => {
    const fetchMock = routedFetch([]);
    global.fetch = fetchMock as unknown as typeof fetch;
    render(<FieldACL entity="crm_contacts" field="phone" />);

    await waitFor(() => expect(cell("Số điện thoại · Kinh doanh")).toBeInTheDocument());
    const aclGets = fetchMock.mock.calls.map((c) => String(c[0])).filter((u) => u.includes("/admin/field-acl"));
    expect(aclGets.some((u) => u.includes("entity=crm_contacts"))).toBe(true);
    expect(aclGets.some((u) => u.includes("entity=crm_accounts"))).toBe(false);
    // The deep-link target: the phone row is marked, a sibling row is not.
    expect(cell("Số điện thoại · Kinh doanh").closest("tr")).toHaveClass("slds-is-selected");
    expect(cell("Tên · Kinh doanh").closest("tr")).not.toHaveClass("slds-is-selected");
  });

  it("without props the URL is still read once", async () => {
    window.history.replaceState(null, "", "/?entity=crm_contacts&field=phone");
    const fetchMock = routedFetch([]);
    global.fetch = fetchMock as unknown as typeof fetch;
    render(<FieldACL />);
    await waitFor(() => expect(cell("Số điện thoại · Kinh doanh")).toBeInTheDocument());
    window.history.replaceState(null, "", "/");
    const aclGets = fetchMock.mock.calls.map((c) => String(c[0])).filter((u) => u.includes("/admin/field-acl"));
    expect(aclGets.some((u) => u.includes("entity=crm_contacts"))).toBe(true);
  });

  it("Save PUTs every changed cell explicitly — hidden (false,false) and a cleared exception (true,true)", async () => {
    const puts: unknown[] = [];
    global.fetch = routedFetch(puts) as unknown as typeof fetch;
    render(<FieldACL />);
    await waitFor(() => expect(cell("Tên · Hỗ trợ")).toBeInTheDocument());

    fireEvent.click(within(cell("Tên · Hỗ trợ")).getByRole("radio", { name: "Ẩn" }));
    fireEvent.click(within(cell("Số điện thoại · Kinh doanh")).getByRole("radio", { name: "Sửa" }));
    expect(screen.getByText("2 thay đổi chưa lưu")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /^Lưu$/ }));
    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0]).toEqual({ grants: [
      { roleId: "r-sales", entityName: "crm_accounts", fieldName: "phone", canView: true, canEdit: true },
      { roleId: "r-support", entityName: "crm_accounts", fieldName: "name", canView: false, canEdit: false },
    ] });
    await waitFor(() => expect(screen.getByText("Đã lưu")).toBeInTheDocument());
    expect(screen.getByRole("button", { name: /^Lưu$/ })).toBeDisabled();
  });

  it("Huỷ puts the draft back to the saved matrix", async () => {
    global.fetch = routedFetch([]) as unknown as typeof fetch;
    render(<FieldACL />);
    await waitFor(() => expect(cell("Tên · Kinh doanh")).toBeInTheDocument());

    fireEvent.click(within(cell("Tên · Kinh doanh")).getByRole("radio", { name: "Chỉ xem" }));
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(within(cell("Tên · Kinh doanh")).getByRole("radio", { name: "Sửa" })).toBeChecked();
    expect(screen.queryByText(/thay đổi chưa lưu/)).not.toBeInTheDocument();
  });

  it("a PUT rejected at details.index marks THAT cell in error, readable by assistive tech", async () => {
    global.fetch = routedFetch([], () =>
      jsonRes({ error: "Quyền mâu thuẫn", code: "FIELD_ACL_INVALID", details: { index: 0 } }, 422),
    ) as unknown as typeof fetch;
    render(<FieldACL />);
    await waitFor(() => expect(cell("Tên · Hỗ trợ")).toBeInTheDocument());

    fireEvent.click(within(cell("Tên · Hỗ trợ")).getByRole("radio", { name: "Ẩn" }));
    fireEvent.click(screen.getByRole("button", { name: /^Lưu$/ }));

    // radio-button-group blueprint "states: error": slds-has-error + the help line inside the control,
    // every radio aria-describedby it.
    await waitFor(() => expect(cell("Tên · Hỗ trợ")).toHaveClass("slds-has-error"));
    expect(within(cell("Tên · Hỗ trợ")).getByRole("radio", { name: "Ẩn" }))
      .toHaveAccessibleDescription("Máy chủ từ chối ô này");
    expect(cell("Tên · Kinh doanh")).not.toHaveClass("slds-has-error");
    expect(screen.getByText("Quyền mâu thuẫn")).toBeInTheDocument();
    // The dismiss sits on the error theme's dark red: inverse glyph (white), as in the alert blueprint.
    const dismiss = screen.getByRole("button", { name: "Đóng thông báo lỗi" });
    expect(dismiss).toHaveClass("slds-button_icon-inverse");
    fireEvent.click(dismiss);
    expect(screen.queryByText("Quyền mâu thuẫn")).not.toBeInTheDocument();
  });

  it("switching entity with unsaved changes asks before throwing them away", async () => {
    global.fetch = routedFetch([]) as unknown as typeof fetch;
    render(<FieldACL />);
    await waitFor(() => expect(cell("Tên · Kinh doanh")).toBeInTheDocument());
    fireEvent.click(within(cell("Tên · Kinh doanh")).getByRole("radio", { name: "Ẩn" }));

    await userEvent.click(screen.getByRole("combobox", { name: /Đối tượng/ }));
    await userEvent.click(screen.getByRole("option", { name: "Liên hệ" }));
    expect(await screen.findByText(/Bỏ thay đổi chưa lưu\?/)).toBeInTheDocument();
    expect(screen.getByText(/1 thay đổi chưa lưu trên “Khách hàng”/)).toBeInTheDocument();
  });

  // specs/074 T203 (f2-access-field-acl): the URL is the source of truth inside Studio — the page hands the pair over,
  // the screen reports every change, a new pair from outside switches the table, and the discard question is
  // asked in the page (FR-065), not in a dialog.
  describe("controlled by the Studio URL (T203)", () => {
    it("picking another table reports (entity, no field) to onChange", async () => {
      global.fetch = routedFetch([]) as unknown as typeof fetch;
      const onChange = vi.fn();
      render(<FieldACL entity="crm_accounts" onChange={onChange} />);
      await waitFor(() => expect(cell("Tên · Kinh doanh")).toBeInTheDocument());
      await userEvent.click(screen.getByRole("combobox", { name: /Đối tượng/ }));
      await userEvent.click(screen.getByRole("option", { name: "Liên hệ" }));
      expect(onChange).toHaveBeenCalledWith("crm_contacts", undefined);
    });

    it("a new entity prop from the URL (Back button) loads that table", async () => {
      const fetchMock = routedFetch([]);
      global.fetch = fetchMock as unknown as typeof fetch;
      const { rerender } = render(<FieldACL entity="crm_accounts" onChange={vi.fn()} />);
      await waitFor(() => expect(cell("Tên · Kinh doanh")).toBeInTheDocument());
      rerender(<FieldACL entity="crm_contacts" onChange={vi.fn()} />);
      await waitFor(() =>
        expect(fetchMock.mock.calls.some((c) => String(c[0]).includes("/admin/field-acl?entity=crm_contacts"))).toBe(true),
      );
    });

    it("unsaved changes: the question is asked in the page (no dialog) and onChange waits for the answer", async () => {
      global.fetch = routedFetch([]) as unknown as typeof fetch;
      const onChange = vi.fn();
      render(<FieldACL entity="crm_accounts" onChange={onChange} />);
      await waitFor(() => expect(cell("Tên · Kinh doanh")).toBeInTheDocument());
      fireEvent.click(within(cell("Tên · Kinh doanh")).getByRole("radio", { name: "Ẩn" }));
      await userEvent.click(screen.getByRole("combobox", { name: /Đối tượng/ }));
      await userEvent.click(screen.getByRole("option", { name: "Liên hệ" }));
      expect(await screen.findByText(/1 thay đổi chưa lưu trên “Khách hàng”/)).toBeInTheDocument();
      expect(screen.queryByRole("dialog")).toBeNull();
      expect(onChange).not.toHaveBeenCalled();
      await userEvent.click(screen.getByRole("button", { name: "Bỏ & chuyển" }));
      expect(onChange).toHaveBeenCalledWith("crm_contacts", undefined);
    });

    it("Back with unsaved edits asks in the page; cancelling reports the shown table back to the URL", async () => {
      global.fetch = routedFetch([]) as unknown as typeof fetch;
      const onChange = vi.fn();
      const { rerender } = render(<FieldACL entity="crm_accounts" onChange={onChange} />);
      await waitFor(() => expect(cell("Tên · Kinh doanh")).toBeInTheDocument());
      fireEvent.click(within(cell("Tên · Kinh doanh")).getByRole("radio", { name: "Ẩn" }));
      rerender(<FieldACL entity="crm_contacts" onChange={onChange} />);
      expect(await screen.findByText(/1 thay đổi chưa lưu trên “Khách hàng”/)).toBeInTheDocument();
      expect(screen.getByRole("combobox", { name: /Đối tượng/ })).toHaveTextContent("Khách hàng");
      await userEvent.click(screen.getAllByRole("button", { name: "Huỷ" }).at(-1)!);
      expect(onChange).toHaveBeenCalledWith("crm_accounts", undefined);
    });
  });

  // queue #146: a lock (Chỉ xem OR Ẩn) on a required field the engine cannot fill leaves that role
  // unable to create anything. Rows saved before the PutGrants guard (or a field made required
  // after the lock) still exist; the admin must SEE them here, with the way out, before an
  // employee meets RECORD_REQUIRED_LOCKED.
  describe("dead-end locks (queue #146)", () => {
    const DEAD_FIELDS = [
      { name: "name", label: "Tên", fieldType: "text", isRequired: true },
      { name: "amount", label: "Giá trị", fieldType: "currency", isRequired: true },
      { name: "stage", label: "Giai đoạn", fieldType: "picklist", isRequired: true, defaultValue: "new" },
    ];
    const DEAD_GRANTS = [
      // pre-guard row: currency cannot carry a default → a dead end for Kinh doanh
      { roleId: "r-sales", entityName: "crm_accounts", fieldName: "amount", canView: true, canEdit: false },
      // hidden, but the engine fills the default → NOT a dead end
      { roleId: "r-support", entityName: "crm_accounts", fieldName: "stage", canView: false, canEdit: false },
    ];

    it("paints a saved lock on a required field the engine cannot fill, lists it, and names the way out", async () => {
      global.fetch = routedFetch([], undefined, DEAD_FIELDS, DEAD_GRANTS, ["name", "amount"]) as unknown as typeof fetch;
      render(<FieldACL />);
      await waitFor(() => expect(cell("Giá trị · Kinh doanh")).toBeInTheDocument());

      // The cell: radio-button-group error state, the reason readable by assistive tech.
      expect(cell("Giá trị · Kinh doanh")).toHaveClass("slds-has-error");
      expect(within(cell("Giá trị · Kinh doanh")).getByRole("radio", { name: "Chỉ xem" }))
        .toHaveAccessibleDescription("Ngõ cụt khi tạo bản ghi");
      // A hidden required field WITH a default the engine writes is fine — no alarm.
      expect(cell("Giai đoạn · Hỗ trợ")).not.toHaveClass("slds-has-error");
      expect(cell("Tên · Kinh doanh")).not.toHaveClass("slds-has-error");

      // The list: which field, which role, and the exit that works for a currency field
      // (no "đặt giá trị mặc định" — the builder refuses a default for this type).
      const note = screen.getByText(/1 ô khoá đang là ngõ cụt/).closest(".slds-scoped-notification")!;
      expect(note).toHaveClass("slds-theme_warning");
      expect(note).toHaveTextContent("“Giá trị” — Kinh doanh");
      expect(note).toHaveTextContent("kiểu Tiền tệ không đặt được giá trị mặc định");
      expect(note).not.toHaveTextContent("đặt giá trị mặc định");
    });

    it("a new lock on a required text field is flagged before Save, with the default as one way out", async () => {
      global.fetch = routedFetch([], undefined, DEAD_FIELDS, DEAD_GRANTS, ["name", "amount"]) as unknown as typeof fetch;
      render(<FieldACL />);
      await waitFor(() => expect(cell("Tên · Hỗ trợ")).toBeInTheDocument());

      fireEvent.click(within(cell("Tên · Hỗ trợ")).getByRole("radio", { name: "Ẩn" }));
      expect(cell("Tên · Hỗ trợ")).toHaveClass("slds-has-error");
      const note = screen.getByText(/2 ô khoá đang là ngõ cụt/).closest(".slds-scoped-notification")!;
      expect(note).toHaveTextContent("“Tên” — Hỗ trợ");
      expect(note).toHaveTextContent("đặt giá trị mặc định");

      // Back to Sửa: the alarm goes with it.
      fireEvent.click(within(cell("Tên · Hỗ trợ")).getByRole("radio", { name: "Sửa" }));
      expect(cell("Tên · Hỗ trợ")).not.toHaveClass("slds-has-error");
      expect(screen.getByText(/1 ô khoá đang là ngõ cụt/)).toBeInTheDocument();
    });

    // queue #306: the verdict is the SERVER's. A picklist whose stored default is no longer in the
    // list has a default string (the old FE type-list mirror said "fillable") but Create 422s on it.
    it("paints a dead end the server flags even though the field carries a default string", async () => {
      const fields = [{ name: "stage", label: "Giai đoạn", fieldType: "picklist", isRequired: true, defaultValue: "gone" }];
      const grants = [{ roleId: "r-sales", entityName: "crm_accounts", fieldName: "stage", canView: true, canEdit: false }];
      global.fetch = routedFetch([], undefined, fields, grants, ["stage"]) as unknown as typeof fetch;
      render(<FieldACL />);
      await waitFor(() => expect(cell("Giai đoạn · Kinh doanh")).toBeInTheDocument());
      expect(cell("Giai đoạn · Kinh doanh")).toHaveClass("slds-has-error");
      expect(screen.getByText(/1 ô khoá đang là ngõ cụt/)).toBeInTheDocument();
    });

    it("no dead end, no notice", async () => {
      global.fetch = routedFetch([]) as unknown as typeof fetch;
      render(<FieldACL />);
      await waitFor(() => expect(cell("Tên · Kinh doanh")).toBeInTheDocument());
      expect(screen.queryByText(/ô khoá đang là ngõ cụt/)).toBeNull();
    });
  });

  describe("Studio, delivered project: 'Áp ngay' (lát f2-direct-apply-access, T379)", () => {
    async function edit(puts: unknown[]) {
      global.fetch = routedFetch(puts) as unknown as typeof fetch;
      render(<FieldACL inlineApply />);
      await waitFor(() => expect(cell("Tên · Hỗ trợ")).toBeInTheDocument());
      fireEvent.click(within(cell("Tên · Hỗ trợ")).getByRole("radio", { name: "Ẩn" }));
    }

    it("asks in the page with the label before writing; Huỷ writes nothing, Áp ngay writes", async () => {
      delivered = true;
      const puts: unknown[] = [];
      await edit(puts);
      expect(await screen.findByText("Áp ngay cho người dùng thật")).toBeInTheDocument();
      fireEvent.click(screen.getByRole("button", { name: /^Lưu$/ }));
      expect(await screen.findByText(/Áp ngay cho người dùng thật: Lưu quyền theo trường/)).toBeInTheDocument();
      expect(screen.queryByRole("dialog")).toBeNull();
      expect(puts).toHaveLength(0);
      fireEvent.click(screen.getAllByRole("button", { name: "Huỷ" }).at(-1)!); // the confirm's own, the last one in the page
      expect(puts).toHaveLength(0);
      fireEvent.click(screen.getByRole("button", { name: /^Lưu$/ }));
      fireEvent.click(await screen.findByRole("button", { name: "Áp ngay" }));
      await waitFor(() => expect(puts).toHaveLength(1));
    });

    it("not delivered: no label, saves at once", async () => {
      const puts: unknown[] = [];
      await edit(puts);
      fireEvent.click(screen.getByRole("button", { name: /^Lưu$/ }));
      await waitFor(() => expect(puts).toHaveLength(1));
      expect(screen.queryByText("Áp ngay cho người dùng thật")).toBeNull();
    });
  });
});
