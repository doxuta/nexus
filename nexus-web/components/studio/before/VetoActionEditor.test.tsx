// specs/048 T149 — the "Chặn và báo lỗi" action: the message the user will read (1–500 characters,
// counted like the server, trimmed runes) and an optional field to pin it under. A server refusal of
// the message (WORKFLOW_VETO_MESSAGE_INVALID) lands on the message box.
import { describe, it, expect, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { FieldMeta } from "@/lib/api";
import { VetoActionEditor } from "./VetoActionEditor";

const FIELDS: FieldMeta[] = [
  { name: "stage", label: "Giai đoạn", fieldType: "picklist" },
  { name: "close_date", label: "Ngày chốt", fieldType: "date" },
];

const box = () => screen.getByRole("textbox", { name: /Thông điệp cho người dùng/ });
const formElement = (el: HTMLElement) => el.closest(".slds-form-element") as HTMLElement;

describe("VetoActionEditor", () => {
  it("shows the message with a character count and reports edits", () => {
    const onChange = vi.fn();
    render(<VetoActionEditor message="Chốt Thắng phải có Ngày chốt" field="" fields={FIELDS} onChange={onChange} />);
    expect(box()).toHaveValue("Chốt Thắng phải có Ngày chốt");
    expect(box()).toBeRequired();
    expect(formElement(box())).toHaveTextContent("28/500 ký tự");
    fireEvent.change(box(), { target: { value: "Cần Ngày chốt" } });
    expect(onChange).toHaveBeenCalledWith("message", "Cần Ngày chốt");
  });

  it("over 500 characters ⇒ an error on the box before the server is asked", () => {
    render(<VetoActionEditor message={"x".repeat(501)} field="" fields={FIELDS} onChange={() => {}} />);
    expect(formElement(box())).toHaveClass("slds-has-error");
    expect(formElement(box())).toHaveTextContent("Tối đa 500 ký tự (đang có 501).");
  });

  it("the server's message refusal lands on the box", () => {
    render(<VetoActionEditor message="" field="" fields={FIELDS} onChange={() => {}} messageError="thông điệp chặn phải có 1–500 ký tự (đang có 0)" />);
    expect(formElement(box())).toHaveClass("slds-has-error");
    expect(formElement(box())).toHaveTextContent("thông điệp chặn phải có 1–500 ký tự");
  });

  it("the field is optional: 'none' first, the entity's fields after, picking one reports it", async () => {
    const onChange = vi.fn();
    render(<VetoActionEditor message="m" field="" fields={FIELDS} onChange={onChange} />);
    const pick = screen.getByRole("combobox", { name: "Gắn lỗi vào trường" });
    expect(pick).toHaveTextContent("Không gắn — hiện ở đầu form");
    await userEvent.click(pick);
    expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual(["Không gắn — hiện ở đầu form", "Giai đoạn", "Ngày chốt"]);
    await userEvent.click(screen.getByRole("option", { name: "Ngày chốt" }));
    expect(onChange).toHaveBeenCalledWith("field", "close_date");
  });

  it("a saved field no longer on the entity stays visible instead of silently clearing", () => {
    render(<VetoActionEditor message="m" field="old_date" fields={FIELDS} onChange={() => {}} />);
    expect(screen.getByRole("combobox", { name: "Gắn lỗi vào trường" })).toHaveTextContent("old_date");
  });

  // Queue #323 (click-through 2026-09-30): a delete has no form — the refusal is read in the delete
  // dialog and the mass-delete strip, never under an input — and "vừa đổi thành" is not offered on a
  // delete rule. The anchor picker and its form advice told the maker otherwise.
  it("before_delete: no anchor picker and no form advice", () => {
    render(<VetoActionEditor trigger="before_delete" message="m" field="" fields={FIELDS} onChange={() => {}} />);
    expect(box()).toHaveValue("m");
    expect(screen.queryByRole("combobox", { name: "Gắn lỗi vào trường" })).toBeNull();
    expect(document.body.textContent).not.toMatch(/vừa đổi thành|đầu form/);
  });

  it("before_delete with a saved anchor: the picker stays so it can be cleared (and a refusal of it is seen)", () => {
    render(<VetoActionEditor trigger="before_delete" message="m" field="old_date" fields={FIELDS} onChange={() => {}} fieldError='field "old_date" không tồn tại' />);
    const pick = screen.getByRole("combobox", { name: "Gắn lỗi vào trường" });
    expect(pick).toHaveTextContent("old_date");
    expect(formElement(pick)).toHaveTextContent('field "old_date" không tồn tại');
  });

  it("a server refusal of the field (RECORD_UNKNOWN_FIELD) lands on the field picker", () => {
    render(<VetoActionEditor message="m" field="old_date" fields={FIELDS} onChange={() => {}} fieldError='field "old_date" không tồn tại' />);
    const pick = screen.getByRole("combobox", { name: "Gắn lỗi vào trường" });
    expect(formElement(pick)).toHaveClass("slds-has-error");
    expect(formElement(pick)).toHaveTextContent('field "old_date" không tồn tại');
  });
});
