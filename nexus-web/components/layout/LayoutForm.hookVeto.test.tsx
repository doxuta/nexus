// specs/048 T132 — a save refused by before-save rules (422 RECORD_HOOK_VETO) on the record form.
// Each rule's sentence sits under its own input; a rule anchored to a field the layout does not
// draw shows its sentence in the form-level notification — never the old "required field missing
// from this form" wrapper; a 503 says "busy, retry" and keeps what the user typed.
//
// Real LayoutForm + SectionGrid + widgets; only the network seam and the app providers are mocked.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { ApiError, type FieldMeta } from "@/lib/api";

const seam = vi.hoisted(() => ({
  getLayout: vi.fn(),
  listFields: vi.fn(),
  updateRecord: vi.fn(),
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
      version: 3,
      data: { name: "Hợp đồng A", probability: 40 },
    }),
    listFields: (...args: unknown[]) => seam.listFields(...args),
    updateRecord: (...args: unknown[]) => seam.updateRecord(...args),
  };
});

import { LayoutForm } from "./LayoutForm";

const NAME: FieldMeta = { id: "f1", name: "name", label: "Tên cơ hội", fieldType: "text", isRequired: true };
const CLOSE: FieldMeta = { id: "f2", name: "close_date", label: "Ngày chốt", fieldType: "date" };
const PROB: FieldMeta = { id: "f3", name: "probability", label: "Xác suất", fieldType: "number" };
const REASON: FieldMeta = { id: "f4", name: "loss_reason", label: "Lý do thua", fieldType: "text" };

beforeEach(() => {
  seam.getLayout.mockReset().mockResolvedValue({
    entityName: "crm_opportunities",
    systemFields: [],
    views: { edit: { isDefault: true, sections: [{ title: "Thông tin", columns: 2, fields: ["name", "close_date", "probability"] }] } },
  });
  seam.listFields.mockReset().mockResolvedValue([NAME, CLOSE, PROB, REASON]);
  seam.updateRecord.mockReset();
});

const veto = (violations: Array<{ field?: string; message: string }>) =>
  new ApiError(422, "RECORD_HOOK_VETO", violations[0].message, {
    ...(violations[0].field ? { field: violations[0].field } : {}),
    violations,
  });

async function openAndSave() {
  render(<LayoutForm entity="crm_opportunities" recordId="opp-1" />);
  await waitFor(() => expect(screen.getByDisplayValue("Hợp đồng A")).toBeInTheDocument(), { timeout: 3000 });
  fireEvent.click(screen.getByRole("button", { name: /Lưu thay đổi/ }));
  await waitFor(() => expect(seam.updateRecord).toHaveBeenCalledTimes(1), { timeout: 3000 });
}

// The help/error lines of the form element whose label is `label` — none unless it is in error
// (a date input also carries its format hint as a help line).
const errorUnder = (label: string) => {
  const el = screen.getByText(label, { selector: "label, legend, span" }).closest(".slds-form-element");
  if (!el?.classList.contains("slds-has-error")) return [];
  return Array.from(el.querySelectorAll(".slds-form-element__help"), (h) => h.textContent ?? "");
};

describe("LayoutForm — a save refused by before-save rules", () => {
  it("two rules broken at once ⇒ two inputs in error, each with its own rule's sentence", async () => {
    seam.updateRecord.mockRejectedValue(
      veto([
        { field: "close_date", message: "Chốt Thắng phải có Ngày chốt" },
        { field: "probability", message: "Xác suất phải từ 10% khi Đàm phán" },
      ]),
    );
    await openAndSave();

    await waitFor(() => expect(errorUnder("Ngày chốt")).toContain("Chốt Thắng phải có Ngày chốt"), { timeout: 3000 });
    expect(errorUnder("Xác suất")).toContain("Xác suất phải từ 10% khi Đàm phán");
    expect(screen.queryByText(/chưa có trên form này/)).toBeNull();
    expect(screen.queryByText(/RECORD_HOOK_VETO/)).toBeNull();
  });

  // Found by the browser acceptance (2026-09-30): with a real network the form re-renders WHILE the
  // request is in flight (react-hook-form publishes a fresh, empty `errors` object as the submit
  // starts). setError then mutates that same object in place, so the error list memoised on it
  // never recomputed: nothing turned red, no notice. The mocks above reject in the same tick and hid
  // it; this one answers after a delay, like the network.
  it("a refusal that arrives after the form re-rendered still lands under its inputs", async () => {
    const err = veto([
      { field: "close_date", message: "Chốt Thắng phải có Ngày chốt" },
      { field: "probability", message: "Xác suất phải từ 10% khi Đàm phán" },
    ]);
    seam.updateRecord.mockImplementation(() => new Promise((_, reject) => setTimeout(() => reject(err), 80)));
    render(<LayoutForm entity="crm_opportunities" recordId="opp-1" />);
    await waitFor(() => expect(screen.getByDisplayValue("Hợp đồng A")).toBeInTheDocument(), { timeout: 3000 });
    fireEvent.change(screen.getByDisplayValue("Hợp đồng A"), { target: { value: "Hợp đồng A — chốt" } });
    fireEvent.change(screen.getByRole("textbox", { name: /Xác suất/ }), { target: { value: "5" } });
    fireEvent.click(screen.getByRole("button", { name: /Lưu thay đổi/ }));
    await waitFor(() => expect(seam.updateRecord).toHaveBeenCalledTimes(1), { timeout: 3000 });

    await waitFor(() => expect(errorUnder("Ngày chốt")).toContain("Chốt Thắng phải có Ngày chốt"), { timeout: 3000 });
    expect(errorUnder("Xác suất")).toContain("Xác suất phải từ 10% khi Đàm phán");
  });

  it("a rule anchored to a field the layout does not draw ⇒ its sentence in the notification, not 'missing from this form'", async () => {
    seam.updateRecord.mockRejectedValue(
      veto([
        { field: "loss_reason", message: "Đóng Thua phải ghi Lý do thua" },
        { message: "Không sửa được cơ hội của quý đã khoá sổ" },
      ]),
    );
    await openAndSave();

    await waitFor(() => expect(screen.getByText(/Đóng Thua phải ghi Lý do thua/)).toBeInTheDocument(), { timeout: 3000 });
    // Both sentences, one per line, inside the notification's paragraph (no list nested in a <p>).
    const line = screen.getByText(/Đóng Thua phải ghi Lý do thua/).closest("p")!;
    expect(line.textContent).toContain("Không sửa được cơ hội của quý đã khoá sổ");
    expect(line.querySelector("ul, li")).toBeNull();
    expect(screen.queryByText(/chưa có trên form này/)).toBeNull();
    expect(screen.queryByText(/Trường bắt buộc/)).toBeNull();
  });

  // Review finding (2026-09-30): the layout's own read-only toggle (LayoutBuilder cell.readonly) draws
  // the field as text with no input — setError() on it showed nothing, and the notification fell back
  // to "Có 1 trường cần sửa: Ngày chốt" without the rule's sentence (a field the user cannot edit).
  it("a rule anchored to a field the layout draws read-only ⇒ its sentence in the notification", async () => {
    seam.getLayout.mockResolvedValue({
      entityName: "crm_opportunities",
      systemFields: [],
      views: {
        edit: {
          isDefault: true,
          sections: [{ title: "Thông tin", columns: 2, fields: ["name", { field: "close_date", readonly: true }, "probability"] }],
        },
      },
    });
    seam.updateRecord.mockRejectedValue(veto([{ field: "close_date", message: "Chốt Thắng phải có Ngày chốt" }]));
    await openAndSave();

    await waitFor(() => expect(screen.getByText(/Chốt Thắng phải có Ngày chốt/)).toBeInTheDocument(), { timeout: 3000 });
    expect(screen.queryByText(/trường cần sửa/)).toBeNull();
  });

  it("one violation under its input + one with no field ⇒ both shown, neither dropped", async () => {
    seam.updateRecord.mockRejectedValue(
      veto([
        { field: "close_date", message: "Chốt Thắng phải có Ngày chốt" },
        { message: "Không sửa được cơ hội của quý đã khoá sổ" },
      ]),
    );
    await openAndSave();

    await waitFor(() => expect(errorUnder("Ngày chốt")).toContain("Chốt Thắng phải có Ngày chốt"), { timeout: 3000 });
    expect(screen.getByText("Không sửa được cơ hội của quý đã khoá sổ")).toBeInTheDocument();
  });

  it("503 ⇒ 'busy, retry', and what the user typed is still on the form", async () => {
    seam.updateRecord.mockRejectedValue(new ApiError(503, "RECORD_HOOKS_UNAVAILABLE", "Hệ thống bận, thử lại sau ít giây"));
    render(<LayoutForm entity="crm_opportunities" recordId="opp-1" />);
    const name = await screen.findByDisplayValue("Hợp đồng A", {}, { timeout: 3000 });
    fireEvent.change(name, { target: { value: "Hợp đồng A — bản sửa" } });
    fireEvent.click(screen.getByRole("button", { name: /Lưu thay đổi/ }));

    await waitFor(() => expect(screen.getByText(/Hệ thống bận, thử lại sau ít giây/)).toBeInTheDocument(), { timeout: 3000 });
    expect(screen.getByDisplayValue("Hợp đồng A — bản sửa")).toBeInTheDocument();
  });
});
