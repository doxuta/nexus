// LayoutForm — field-lock-signal (DAILY-QUEUE #12, specs/001-auth-security A6 "chân C").
//
// A role may SEE a field but not EDIT it (sys_field_acl can_edit=0). Until this slice the form
// could not tell: it drew the input, the employee typed a new value, pressed Lưu, and only then met
// 403 FIELD_FORBIDDEN. GET /metadata/fields/:entity now marks such a field `readOnly: true` for the
// CALLER, and the form must (a) render it as TEXT — no <input> at all — and (b) never put it in the
// payload (the create guard refuses any locked key; the update guard refuses a changed one).
//
// Real LayoutForm + SectionGrid + CellRenderer + widgets; only the network seam and the app
// providers (auth, entity label, navigation blocker) are mocked.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { ApiError, type FieldMeta } from "@/lib/api";

const seam = vi.hoisted(() => ({
  getLayout: vi.fn(),
  listFields: vi.fn(),
  updateRecord: vi.fn(),
  createRecord: vi.fn(),
}));

vi.mock("@/lib/auth/AuthProvider", () => ({
  useAuth: () => ({ user: { id: "u1", isAdmin: false } }),
}));
vi.mock("@/lib/metadata/hooks", () => ({
  useEntityLabel: (entity: string) => entity,
}));
vi.mock("@/lib/navigation-blocker", () => ({
  useNavigationBlocker: () => ({ setBlocked: () => {}, confirmLeave: () => true }),
}));
vi.mock("@/components/help/HelpHint", () => ({ HelpHint: () => null }));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn().mockResolvedValue(undefined),
    getLayout: (...args: unknown[]) => seam.getLayout(...args),
    getRecord: vi.fn().mockResolvedValue({
      id: "opp-1",
      version: 1,
      data: { name: "Hợp đồng A", amount: { amount: 9000, currencyCode: "VND" } },
    }),
    listFields: (...args: unknown[]) => seam.listFields(...args),
    updateRecord: (...args: unknown[]) => seam.updateRecord(...args),
    createRecord: (...args: unknown[]) => seam.createRecord(...args),
  };
});

import { LayoutForm } from "./LayoutForm";

const NAME: FieldMeta = { id: "f1", name: "name", label: "Tên cơ hội", fieldType: "text", isRequired: true };
const AMOUNT: FieldMeta = { id: "f2", name: "amount", label: "Giá trị", fieldType: "currency" };

const layoutWith = (fields: string[]) => ({
  entityName: "crm_opportunities",
  systemFields: [],
  views: { edit: { isDefault: true, sections: [{ title: "Thông tin", columns: 2, fields }] } },
});

beforeEach(() => {
  seam.getLayout.mockReset().mockResolvedValue(layoutWith(["name", "amount"]));
  seam.listFields.mockReset();
  seam.updateRecord.mockReset().mockResolvedValue({ id: "opp-1", version: 2 });
  seam.createRecord.mockReset().mockResolvedValue({ id: "opp-new", version: 1 });
});

describe("LayoutForm — a field-ACL-locked field is text, not an input, and never travels", () => {
  it("locked amount: label + value as text, ONE input on the form (name), payload without amount", async () => {
    // What the server says to THIS caller: amount is visible but locked.
    seam.listFields.mockResolvedValue([NAME, { ...AMOUNT, readOnly: true }]);
    render(<LayoutForm entity="crm_opportunities" recordId="opp-1" />);

    await waitFor(() => expect(screen.getByText("Giá trị")).toBeInTheDocument(), { timeout: 3000 });

    // The discriminating observable: no <input> for the locked field. Only `name` draws one.
    const inputs = Array.from(document.querySelectorAll("input"));
    expect(inputs).toHaveLength(1);
    expect((inputs[0] as HTMLInputElement).value).toBe("Hợp đồng A");
    // The stored value is still SHOWN (formatted currency text), not blanked.
    expect(screen.getByText(/9[.,]000/)).toBeInTheDocument();

    // Saving sends only what the user may edit — the locked key is absent, not null, not resent.
    fireEvent.click(screen.getByRole("button", { name: /Lưu thay đổi/ }));
    await waitFor(() => expect(seam.updateRecord).toHaveBeenCalledTimes(1), { timeout: 3000 });
    const payload = seam.updateRecord.mock.calls[0][2] as Record<string, unknown>;
    expect(payload).not.toHaveProperty("amount");
    expect(payload.name).toBe("Hợp đồng A");
  });

  // Q2 (2026-09-05): the lock applies on CREATE too — the strict create guard refuses any locked
  // key. A prefill aimed at a locked field (e.g. a related list seeding a parent FK) must not be
  // shown as read-only text that promises a value the record will not get: it renders empty, and
  // the POST carries only what the user may write.
  it("create + prefill into the locked field: shown empty, never posted", async () => {
    seam.listFields.mockResolvedValue([NAME, { ...AMOUNT, readOnly: true }]);
    render(<LayoutForm entity="crm_opportunities" modal prefill={{ amount: "5000", name: "Đơn B" }} />);

    await waitFor(() => expect(screen.getByText("Giá trị")).toBeInTheDocument(), { timeout: 3000 });
    expect(document.querySelectorAll("input")).toHaveLength(1);
    expect(screen.queryByText(/5[.,]000/)).toBeNull(); // the prefill is NOT promised as a value
    expect(screen.getByText("—")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /Tạo crm_opportunities/ }));
    await waitFor(() => expect(seam.createRecord).toHaveBeenCalledTimes(1), { timeout: 3000 });
    const payload = seam.createRecord.mock.calls[0][1] as Record<string, unknown>;
    expect(payload).toEqual({ name: "Đơn B" });
  });

  // A 422 that blames the LOCKED field (a required field locked for the role with no default —
  // a config PutGrants now refuses, but rows saved before the guard can exist) has no input to
  // land on. It must say "locked for your role", not the pre-existing "add it to the layout"
  // banner — the field IS on the form, as text.
  it("edit: a 422 blaming the locked field explains the lock instead of 'add it to the layout'", async () => {
    seam.listFields.mockResolvedValue([NAME, { ...AMOUNT, readOnly: true }]);
    seam.updateRecord.mockRejectedValue(
      new ApiError(422, "RECORD_REQUIRED_MISSING", 'field "amount" là bắt buộc', { field: "amount" }),
    );
    render(<LayoutForm entity="crm_opportunities" recordId="opp-1" />);

    await waitFor(() => expect(screen.getByText("Giá trị")).toBeInTheDocument(), { timeout: 3000 });
    fireEvent.click(screen.getByRole("button", { name: /Lưu thay đổi/ }));
    await waitFor(
      () => expect(screen.getByText(/"Giá trị" đang bị khoá với vai trò của bạn/)).toBeInTheDocument(),
      { timeout: 3000 },
    );
    expect(screen.queryByText(/chưa có trên form này/)).toBeNull();
  });

  // queue #62: the create path now answers RECORD_REQUIRED_LOCKED with the admin's REAL way out,
  // which depends on the field type (currency/user cannot carry a default). The form used to wrap
  // any lock complaint in its own advice — "báo quản trị viên đặt giá trị mặc định" — i.e. the
  // exact dead end the server just stopped recommending. The server's sentence must stand alone.
  it("create: RECORD_REQUIRED_LOCKED shows the server's type-aware exit, not a 'set a default' wrapper", async () => {
    seam.listFields.mockResolvedValue([NAME, { ...AMOUNT, readOnly: true }]);
    const msg =
      'Trường "Giá trị" bắt buộc nhưng vai trò của bạn không được ghi trường này, nên chưa tạo được bản ghi nào. ' +
      'Báo quản trị viên — cách gỡ: bỏ "bắt buộc" của field, hoặc để field sửa được cho vai này (kiểu "currency" không đặt được giá trị mặc định)';
    seam.createRecord.mockRejectedValue(new ApiError(422, "RECORD_REQUIRED_LOCKED", msg, { field: "amount" }));
    render(<LayoutForm entity="crm_opportunities" modal prefill={{ name: "Đơn C" }} />);

    await waitFor(() => expect(screen.getByText("Giá trị")).toBeInTheDocument(), { timeout: 3000 });
    fireEvent.click(screen.getByRole("button", { name: /Tạo crm_opportunities/ }));
    await waitFor(() => expect(screen.getByText(msg)).toBeInTheDocument(), { timeout: 3000 });
    expect(screen.queryByText(/đặt giá trị mặc định hoặc mở khoá/)).toBeNull();
  });

  // Adversarial review of queue #62: the LOCKED branch used to sit INSIDE "the locked field is
  // rendered on this form". A custom edit layout that leaves the locked field out fell through to
  // "chưa có trên form này — báo quản trị viên thêm vào bố cục": the dead-end advice this slice
  // exists to remove (adding it only draws it as text; create still 422s). The server's
  // RECORD_REQUIRED_LOCKED sentence names the real exit wherever the field sits.
  it("create: RECORD_REQUIRED_LOCKED for a locked field the layout OMITS still shows the server's exit alone", async () => {
    seam.getLayout.mockResolvedValue(layoutWith(["name"]));
    seam.listFields.mockResolvedValue([NAME, { ...AMOUNT, readOnly: true }]);
    const msg =
      'Trường "Giá trị" bắt buộc nhưng vai trò của bạn không được ghi trường này, nên chưa tạo được bản ghi nào. ' +
      'Báo quản trị viên — cách gỡ: bỏ "bắt buộc" của field, hoặc để field sửa được cho vai này (kiểu "currency" không đặt được giá trị mặc định)';
    seam.createRecord.mockRejectedValue(new ApiError(422, "RECORD_REQUIRED_LOCKED", msg, { field: "amount" }));
    render(<LayoutForm entity="crm_opportunities" modal prefill={{ name: "Đơn D" }} />);

    await waitFor(() => expect(screen.getByText("Tên cơ hội")).toBeInTheDocument(), { timeout: 3000 });
    expect(screen.queryByText("Giá trị")).toBeNull(); // the layout really omits the locked field
    fireEvent.click(screen.getByRole("button", { name: /Tạo crm_opportunities/ }));
    await waitFor(() => expect(screen.getByText(msg)).toBeInTheDocument(), { timeout: 3000 });
    expect(screen.queryByText(/chưa có trên form này/)).toBeNull();
  });

  it("no lock (admin / unrestricted role): amount keeps its input", async () => {
    seam.listFields.mockResolvedValue([NAME, AMOUNT]);
    render(<LayoutForm entity="crm_opportunities" recordId="opp-1" />);

    await waitFor(() => expect(screen.getByText("Giá trị")).toBeInTheDocument(), { timeout: 3000 });
    expect(document.querySelectorAll("input")).toHaveLength(2);
  });
});

// queue #146: the employee used to learn about a dead-end lock only AFTER typing the whole record
// and pressing Tạo (422 RECORD_REQUIRED_LOCKED). GET /metadata/fields already tells the form
// everything it needs — `readOnly` + `isRequired` + `defaultValue` + `fieldType` — so a create form
// says it the moment it opens. The server still decides: the warning blocks nothing.
describe("LayoutForm — create warns up front about a required field locked for the caller (queue #146)", () => {
  const LOCKED_REQ_AMOUNT: FieldMeta = { ...AMOUNT, isRequired: true, readOnly: true };
  const warning = () => screen.queryByText(/Chưa tạo được bản ghi/);

  it("warns before typing, naming the field and an exit that works for its type", async () => {
    seam.listFields.mockResolvedValue([NAME, LOCKED_REQ_AMOUNT]);
    render(<LayoutForm entity="crm_opportunities" modal />);

    await waitFor(() => expect(warning()).toBeInTheDocument(), { timeout: 3000 });
    const note = warning()!.closest(".slds-scoped-notification")!;
    expect(note).toHaveClass("slds-theme_warning");
    expect(note).toHaveTextContent("“Giá trị”");
    // currency cannot carry a default: the advice must not send the admin into a second 422.
    expect(note).toHaveTextContent("bỏ “bắt buộc”");
    expect(note).not.toHaveTextContent("đặt giá trị mặc định");
    expect(seam.createRecord).not.toHaveBeenCalled();
  });

  it("warns even when the edit layout leaves the locked field out", async () => {
    seam.getLayout.mockResolvedValue(layoutWith(["name"]));
    seam.listFields.mockResolvedValue([NAME, LOCKED_REQ_AMOUNT]);
    render(<LayoutForm entity="crm_opportunities" modal />);

    await waitFor(() => expect(warning()).toBeInTheDocument(), { timeout: 3000 });
    expect(warning()!.closest(".slds-scoped-notification")).toHaveTextContent("“Giá trị”");
  });

  it("a locked required field the engine fills from its default is not a dead end", async () => {
    seam.listFields.mockResolvedValue([
      NAME,
      { id: "f3", name: "stage", label: "Giai đoạn", fieldType: "picklist", isRequired: true, defaultValue: "new", readOnly: true },
    ]);
    render(<LayoutForm entity="crm_opportunities" modal />);

    await waitFor(() => expect(screen.getByText("Tên cơ hội")).toBeInTheDocument(), { timeout: 3000 });
    expect(warning()).toBeNull();
  });

  it("edit mode: the record already exists — no create warning", async () => {
    seam.listFields.mockResolvedValue([NAME, LOCKED_REQ_AMOUNT]);
    render(<LayoutForm entity="crm_opportunities" recordId="opp-1" />);

    await waitFor(() => expect(screen.getByText("Giá trị")).toBeInTheDocument(), { timeout: 3000 });
    expect(warning()).toBeNull();
  });
});

// specs/063 S7-3: in a draft-preview tab BOTH save buttons are locked (the server answers 403 PREVIEW_READ_ONLY).
describe("LayoutForm — draft-preview tab", () => {
  it("create form: Lưu and Lưu & tạo mới are both disabled", async () => {
    seam.listFields.mockResolvedValue([NAME, AMOUNT]);
    window.sessionStorage.setItem("nexus_draft_preview", JSON.stringify({ returnTo: "/studio" }));
    try {
      render(<LayoutForm entity="crm_opportunities" />);
      const secondary = await screen.findByRole("button", { name: /Lưu & tạo mới/ }, { timeout: 3000 });
      expect(secondary).toBeDisabled();
      for (const b of screen.getAllByRole("button", { name: /^Lưu/ })) expect(b).toBeDisabled();
    } finally {
      window.sessionStorage.clear();
    }
  });
});
