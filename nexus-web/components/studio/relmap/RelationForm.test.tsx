// specs/075 T170: the in-place "Liên kết mới" form — both create paths, every field of the old dialog, business-only errors.
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ addField: vi.fn(), createRelationship: vi.fn() }));
vi.mock("@/lib/api", async (importOriginal) => ({ ...(await importOriginal<typeof import("@/lib/api")>()), ...api }));
const emit = vi.hoisted(() => vi.fn());
vi.mock("@/lib/draftEvents", () => ({ emitDraftsChanged: (s: string) => emit(s) }));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn(), warning: vi.fn(), info: vi.fn() } }));

import { ApiError } from "@/lib/api";
import { RelationForm } from "./RelationForm";

const entities = [
  { name: "crm_accounts", label: "Khách hàng", fields: [{ name: "name", fieldType: "text" }] },
  { name: "crm_events", label: "Lịch hẹn", fields: [{ name: "account", fieldType: "relation" }] },
  { name: "crm_notes", label: "Ghi chú", fields: [] },
];
const onCreated = vi.fn();
const onCancel = vi.fn();
const mount = (p: { parent?: string; child?: string } = {}) =>
  render(<RelationForm entities={entities} initialParent={p.parent} initialChild={p.child} onCreated={onCreated} onCancel={onCancel} />);

beforeEach(() => vi.clearAllMocks());

describe("RelationForm", () => {
  it("1-N: suggests the field name, needs a label, and posts a relation field on the detail table", async () => {
    api.addField.mockResolvedValue({ name: "accounts" });
    mount({ parent: "crm_accounts", child: "crm_notes" });
    expect(screen.getByLabelText("Tên trường liên kết")).toHaveValue("account");
    const label = screen.getByLabelText(/Nhãn trường liên kết/);
    fireEvent.change(label, { target: { value: "" } });
    expect(screen.getByRole("button", { name: "Tạo liên kết" })).toBeDisabled();
    fireEvent.change(label, { target: { value: "Khách" } });
    fireEvent.change(screen.getByLabelText("Tên nhìn từ bảng chính (tuỳ chọn)"), { target: { value: "ghi_chu" } });
    expect(screen.getByText("Không sửa được sau khi tạo.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Tạo liên kết" }));
    await waitFor(() => expect(api.addField).toHaveBeenCalledWith("crm_notes", {
      name: "account", label: "Khách", fieldType: "relation",
      options: { relatedEntity: "crm_accounts", cardinality: "1-N", inverseField: "ghi_chu" },
    }));
    await waitFor(() => expect(onCreated).toHaveBeenCalledWith({ parent: "crm_accounts", child: "crm_notes", relType: "1-N", fieldName: "account", relName: undefined }));
    expect(emit).toHaveBeenCalledWith("relation-create");
  });

  it("wrong characters and a taken name are told under the box, in business words", () => {
    mount({ parent: "crm_accounts", child: "crm_events" });
    expect(screen.getByLabelText("Tên trường liên kết")).toHaveValue("account_2"); // "account" already exists: suggestion skips it
    fireEvent.change(screen.getByLabelText("Tên trường liên kết"), { target: { value: "account" } });
    expect(screen.getByText("Bảng “Lịch hẹn” đã có trường tên này.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Tạo liên kết" })).toBeDisabled();
    fireEvent.change(screen.getByLabelText("Tên trường liên kết"), { target: { value: "Sai Tên" } });
    const msg = screen.getByText("Chỉ dùng chữ thường không dấu, số và gạch dưới, bắt đầu bằng chữ.");
    expect(msg.textContent).not.toMatch(/snake_case/);
  });

  it("N-N asks only for a link name and posts /admin/relationships", async () => {
    api.createRelationship.mockResolvedValue({ id: "r9", name: "khach_ghi_chu" });
    mount({ parent: "crm_accounts", child: "crm_notes" });
    fireEvent.click(screen.getByLabelText("Nhiều–nhiều"));
    expect(screen.queryByLabelText("Tên trường liên kết")).toBeNull();
    expect(screen.queryByLabelText(/Nhãn trường liên kết/)).toBeNull();
    fireEvent.change(screen.getByLabelText("Tên liên kết"), { target: { value: "khach_ghi_chu" } });
    fireEvent.click(screen.getByRole("button", { name: "Tạo liên kết" }));
    await waitFor(() => expect(api.createRelationship).toHaveBeenCalledWith({ name: "khach_ghi_chu", entityA: "crm_accounts", entityB: "crm_notes" }));
    expect(api.addField).not.toHaveBeenCalled();
    await waitFor(() => expect(emit).toHaveBeenCalledWith("relation-create")); // a new relation is a pending change: the header recounts
  });

  it("swap exchanges the two tables", () => {
    mount({ parent: "crm_accounts", child: "crm_notes" });
    fireEvent.click(screen.getByRole("button", { name: "Đảo chiều hai bảng" }));
    expect(screen.getByLabelText("Tên trường liên kết")).toHaveValue("note");
  });

  it.each(["FIELD_RELATION_INVERSE_TAKEN", "FIELD_RELATION_OPTIONS_IMMUTABLE", "SOME_UNKNOWN_CODE"])("422 %s: no engine words, no raw message", async (code) => {
    api.addField.mockRejectedValue(new ApiError(422, code, "raw inverse unique N-N đối tượng"));
    const { container } = mount({ parent: "crm_accounts", child: "crm_notes" });
    fireEvent.click(screen.getByRole("button", { name: "Tạo liên kết" }));
    await waitFor(() => expect(container.querySelector(".slds-has-error, .slds-scoped-notification")).not.toBeNull());
    const text = container.textContent ?? "";
    expect(text).not.toMatch(/inverse|đối tượng|N-N|unique|raw|snake_case/i);
    expect(onCreated).not.toHaveBeenCalled();
    expect(emit).not.toHaveBeenCalled();
  });

  it("Huỷ closes it and there is no dialog", () => {
    mount();
    expect(screen.queryByRole("dialog")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(onCancel).toHaveBeenCalled();
  });
});
