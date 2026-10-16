// LayoutForm — conditional cells (specs/054 FR-7, queue #258 fe-layout-visible-when).
// "Mã số thuế" shows only for a business customer. The form judges on the create IMAGE (payload +
// defaults), a hidden field is neither validated nor sent (Q1: its stored value is kept), and
// showing it again restores what was typed. Real LayoutForm; only the network seam is mocked.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { ApiError, type FieldMeta } from "@/lib/api";

const seam = vi.hoisted(() => ({
  getLayout: vi.fn(),
  listFields: vi.fn(),
  getRecord: vi.fn(),
  updateRecord: vi.fn(),
  createRecord: vi.fn(),
}));

vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { id: "u1", isAdmin: false } }) }));
vi.mock("@/lib/metadata/hooks", () => ({ useEntityLabel: (entity: string) => entity }));
vi.mock("@/lib/navigation-blocker", () => ({
  useNavigationBlocker: () => ({ setBlocked: () => {}, confirmLeave: () => true }),
}));
vi.mock("@/components/help/HelpHint", () => ({ HelpHint: () => null }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn().mockResolvedValue(undefined),
    getLayout: (...a: unknown[]) => seam.getLayout(...a),
    listFields: (...a: unknown[]) => seam.listFields(...a),
    getRecord: (...a: unknown[]) => seam.getRecord(...a),
    updateRecord: (...a: unknown[]) => seam.updateRecord(...a),
    createRecord: (...a: unknown[]) => seam.createRecord(...a),
  };
});

import { LayoutForm } from "./LayoutForm";

const NAME = { id: "f1", name: "name", label: "Tên khách", fieldType: "text", isRequired: true } as FieldMeta;
const TYPE = (defaultValue?: string) =>
  ({
    id: "f2", name: "customer_type", label: "Loại khách", fieldType: "picklist", defaultValue,
    options: { values: ["individual", "business"] },
  }) as FieldMeta;
const TAX = { id: "f3", name: "tax_code", label: "Mã số thuế", fieldType: "text", isRequired: true } as FieldMeta;

const layout = {
  entityName: "crm_leads",
  systemFields: [],
  views: {
    edit: {
      isDefault: true,
      sections: [
        {
          title: "Khách hàng",
          columns: 1,
          fields: [
            "name",
            "customer_type",
            { field: "tax_code", visibleWhen: { field: "customer_type", op: "eq", value: "business" } },
          ],
        },
      ],
    },
  },
};

const pick = async (label: string, option: string) => {
  fireEvent.click(screen.getByRole("combobox", { name: label }));
  fireEvent.click(await screen.findByRole("option", { name: option }));
};

beforeEach(() => {
  seam.getLayout.mockReset().mockResolvedValue(layout);
  seam.listFields.mockReset();
  seam.getRecord.mockReset();
  seam.updateRecord.mockReset().mockResolvedValue({ id: "l1", version: 2 });
  seam.createRecord.mockReset().mockResolvedValue({ id: "l-new", version: 1 });
});

describe("LayoutForm — conditional cells on create", () => {
  it("default 'business' (never touched): the image shows Mã số thuế; blank ⇒ error under THAT field, no POST", async () => {
    seam.listFields.mockResolvedValue([NAME, TYPE("business"), TAX]);
    render(<LayoutForm entity="crm_leads" />);
    await screen.findByLabelText(/Mã số thuế/);
    fireEvent.change(screen.getByLabelText(/Tên khách/), { target: { value: "An" } });
    fireEvent.click(screen.getByRole("button", { name: /Tạo crm_leads/ }));
    await waitFor(() => expect(document.querySelector(".slds-has-error")).not.toBeNull());
    expect(seam.createRecord).not.toHaveBeenCalled();
  });

  it("individual: Mã số thuế disappears and the save goes through WITHOUT it; business again brings back what was typed", async () => {
    seam.listFields.mockResolvedValue([NAME, TYPE("business"), TAX]);
    render(<LayoutForm entity="crm_leads" />);
    const tax = await screen.findByLabelText(/Mã số thuế/);
    fireEvent.change(tax, { target: { value: "0101" } });
    fireEvent.change(screen.getByLabelText(/Tên khách/), { target: { value: "An" } });

    await pick("Loại khách", "individual");
    await waitFor(() => expect(screen.queryByLabelText(/Mã số thuế/)).toBeNull());
    await pick("Loại khách", "business");
    expect(((await screen.findByLabelText(/Mã số thuế/)) as HTMLInputElement).value).toBe("0101");

    await pick("Loại khách", "individual");
    await waitFor(() => expect(screen.queryByLabelText(/Mã số thuế/)).toBeNull());
    fireEvent.click(screen.getByRole("button", { name: /Tạo crm_leads/ }));
    await waitFor(() => expect(seam.createRecord).toHaveBeenCalledTimes(1));
    expect(seam.createRecord.mock.calls[0][1]).toEqual({ name: "An", customer_type: "individual" });
  });

  it("a 422 that names the hidden field goes to the banner, not to an input that is not on screen", async () => {
    seam.listFields.mockResolvedValue([NAME, TYPE("individual"), TAX]);
    seam.createRecord.mockRejectedValue(
      new ApiError(422, "RECORD_REQUIRED_MISSING", "field tax_code là bắt buộc", { field: "tax_code" }),
    );
    render(<LayoutForm entity="crm_leads" />);
    await screen.findByLabelText(/Tên khách/);
    expect(screen.queryByLabelText(/Mã số thuế/)).toBeNull();
    fireEvent.change(screen.getByLabelText(/Tên khách/), { target: { value: "An" } });
    fireEvent.click(screen.getByRole("button", { name: /Tạo crm_leads/ }));
    await waitFor(() => expect(seam.createRecord).toHaveBeenCalledTimes(1));
    expect(await screen.findByText(/tax_code/)).toBeInTheDocument();
    expect(document.querySelector(".slds-has-error")).toBeNull();
  });
});

describe("LayoutForm — conditional cells on edit (Q1: a hidden field keeps its stored value)", () => {
  it("record is individual with a stored tax_code: no input, and the PUT does not null it", async () => {
    seam.listFields.mockResolvedValue([NAME, TYPE(), TAX]);
    seam.getRecord.mockResolvedValue({
      id: "l1", version: 1, data: { name: "An", customer_type: "individual", tax_code: "0101" },
    });
    render(<LayoutForm entity="crm_leads" recordId="l1" />);
    await screen.findByLabelText(/Tên khách/);
    expect(screen.queryByLabelText(/Mã số thuế/)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /Lưu thay đổi/ }));
    await waitFor(() => expect(seam.updateRecord).toHaveBeenCalledTimes(1));
    const payload = seam.updateRecord.mock.calls[0][2] as Record<string, unknown>;
    // Not even `tax_code: null`: buildPayload(update) nulls empties, so it must never see the hidden field.
    expect(payload).toEqual({ name: "An", customer_type: "individual" });
  });

  it("read-only controller still shows the dependent cell and its edit reaches the PUT", async () => {
    // zod strips the read-only controller from the submitted values; visibility must be judged on
    // the full form values (same image as render), else tax_code vanishes from the PUT.
    const RO_TYPE = { ...TYPE(), readOnly: true } as FieldMeta;
    const TAX_OPT = { ...TAX, isRequired: false } as FieldMeta;
    seam.listFields.mockResolvedValue([NAME, RO_TYPE, TAX_OPT]);
    seam.getRecord.mockResolvedValue({
      id: "l1", version: 1, data: { name: "An", customer_type: "business", tax_code: "0101" },
    });
    render(<LayoutForm entity="crm_leads" recordId="l1" />);
    const tax = await screen.findByLabelText(/Mã số thuế/);
    fireEvent.change(tax, { target: { value: "9999" } });
    fireEvent.click(screen.getByRole("button", { name: /Lưu thay đổi/ }));
    await waitFor(() => expect(seam.updateRecord).toHaveBeenCalledTimes(1));
    expect(seam.updateRecord.mock.calls[0][2]).toMatchObject({ tax_code: "9999" });
  });
});
