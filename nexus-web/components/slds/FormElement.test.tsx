import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { FormElement } from "./FormElement";

describe("FormElement (blueprint form-element)", () => {
  it("renders label[for] + __control + required abbr", () => {
    const { container } = render(
      <FormElement label="Tên khách hàng" htmlFor="f1" required>
        <input id="f1" className="slds-input" />
      </FormElement>,
    );
    const root = container.firstElementChild!;
    expect(root).toHaveClass("slds-form-element");
    const label = root.querySelector("label.slds-form-element__label")!;
    expect(label).toHaveAttribute("for", "f1");
    const abbr = label.querySelector("abbr.slds-required")!;
    expect(abbr).toHaveAttribute("aria-hidden", "true");
    expect(abbr).toHaveAttribute("title", "bắt buộc");
    expect(root.querySelector(".slds-form-element__control > input")).not.toBeNull();
  });

  it("error adds slds-has-error and the help line carries helpId", () => {
    const { container } = render(
      <FormElement label="Email" htmlFor="f2" helpId="f2-help" error="Nhập giá trị." help="VD: a@b.vn">
        <input id="f2" />
      </FormElement>,
    );
    const root = container.firstElementChild!;
    expect(root).toHaveClass("slds-has-error");
    const help = root.querySelector(".slds-form-element__help")!;
    expect(help).toHaveAttribute("id", "f2-help");
    expect(help).toHaveTextContent("Nhập giá trị.");
    // help line sits after the control, not inside it
    expect(help.previousElementSibling).toHaveClass("slds-form-element__control");
  });

  it("stacked / horizontal / 1-col layouts map to SLDS modifiers", () => {
    const { container, rerender } = render(<FormElement label="A" layout="stacked" />);
    expect(container.firstElementChild).toHaveClass("slds-form-element_stacked");
    rerender(<FormElement label="A" layout="horizontal" oneColumn />);
    expect(container.firstElementChild).toHaveClass("slds-form-element_horizontal", "slds-form-element_1-col");
  });

  it("readOnly renders span label + __static; onEdit adds the hint pencil", async () => {
    const onEdit = vi.fn();
    const { container } = render(
      <FormElement label="Chủ sở hữu" readOnly layout="stacked" onEdit={onEdit}>
        Nguyễn Văn A
      </FormElement>,
    );
    const root = container.firstElementChild!;
    expect(root).toHaveClass("slds-form-element_readonly", "slds-form-element_edit", "slds-hint-parent");
    expect(root.querySelector("span.slds-form-element__label")).toHaveTextContent("Chủ sở hữu");
    expect(root.querySelector("label")).toBeNull();
    expect(root.querySelector(".slds-form-element__static")).toHaveTextContent("Nguyễn Văn A");
    const btn = screen.getByRole("button", { name: "Sửa: Chủ sở hữu" });
    expect(btn).toHaveAttribute("type", "button");
    await userEvent.click(btn);
    expect(onEdit).toHaveBeenCalledOnce();
  });

  it('readOnly="static" keeps the span label + __static but drops the readonly border', () => {
    const { container } = render(
      <FormElement label="Người tạo" readOnly="static">
        Trần Thu Hà
      </FormElement>,
    );
    const root = container.firstElementChild!;
    expect(root).not.toHaveClass("slds-form-element_readonly");
    expect(root.querySelector("span.slds-form-element__label")).not.toBeNull();
    expect(root.querySelector(".slds-form-element__static")).toHaveTextContent("Trần Thu Hà");
  });

  it("readOnly keeps the required mark (record-detail view); readOnly=\"static\" drops it", () => {
    const { container, rerender } = render(
      <FormElement label="Chủ sở hữu" required readOnly onEdit={() => {}} layout="stacked">
        Nguyễn Văn A
      </FormElement>,
    );
    const label = container.querySelector("span.slds-form-element__label")!;
    expect(label.querySelector("abbr.slds-required")).not.toBeNull();
    expect(label).toHaveTextContent("* Chủ sở hữu");
    rerender(
      <FormElement label="Chủ sở hữu" required readOnly="static">
        Nguyễn Văn A
      </FormElement>,
    );
    expect(container.querySelector("abbr.slds-required")).toBeNull();
  });

  it("editing adds slds-is-editing (record-detail edit mode); layout alone never does", () => {
    const { container, rerender } = render(<FormElement label="A" layout="horizontal" />);
    expect(container.firstElementChild).not.toHaveClass("slds-is-editing");
    rerender(<FormElement label="A" layout="horizontal" editing />);
    expect(container.firstElementChild).toHaveClass("slds-form-element_horizontal", "slds-is-editing");
    expect(container.firstElementChild).not.toHaveAttribute("editing");
  });

  it("fieldset groups render legend and slds-is-required", () => {
    const { container } = render(<FormElement as="fieldset" label="Ngày học" required />);
    const root = container.firstElementChild!;
    expect(root.tagName).toBe("FIELDSET");
    expect(root).toHaveClass("slds-is-required");
    expect(root.querySelector("legend")).toHaveClass("slds-form-element__legend", "slds-form-element__label");
  });

  it("fieldLevelHelp slot lands in slds-form-element__icon", () => {
    const { container } = render(
      <FormElement label="Mã" htmlFor="f3" fieldLevelHelp={<button type="button">?</button>}>
        <input id="f3" />
      </FormElement>,
    );
    expect(container.querySelector(".slds-form-element__icon > button")).not.toBeNull();
  });
});
