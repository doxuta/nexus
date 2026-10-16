import { describe, expect, it, vi } from "vitest";
import { createRef } from "react";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { FieldMeta } from "@/lib/api";
import { draftFromSections, type DraftFieldCell } from "@/lib/layoutDraft";
import { FieldProperties, type FieldPropertiesProps } from "./FieldProperties";

const field = { name: "email", label: "Email", fieldType: "text", isRequired: false } as FieldMeta;
const sections = draftFromSections([
  { title: "Một", columns: 2, fields: ["email"] },
  { title: "Hai", columns: 2, fields: ["x"] },
]);
const cell = sections[0].cells[0] as DraftFieldCell;

function setup(over: Partial<FieldPropertiesProps> = {}) {
  const fn = () => vi.fn();
  const props: FieldPropertiesProps = {
    cell, field, unplaced: null, sections, sectionId: sections[0].id, canUp: true, canDown: true, removeBlocked: false, locked: false,
    tableHref: "/studio/du-an/northwind/dung/du-lieu/t", labelRef: createRef(), fields: [field], dependents: [], onWhen: fn(), requiredOn: false, onRequired: fn(),
    onLabel: fn(), onUp: fn(), onDown: fn(), onToSection: fn(), onRemove: fn(), onPlace: fn(), onDone: fn(), onOption: fn(), ...over,
  };
  render(<FieldProperties {...props} />);
  return props;
}

describe("FieldProperties (specs/075 T014)", () => {
  it("nothing selected: one hint line", () => {
    setup({ cell: null, field: null });
    expect(screen.getByText("Bấm một ô trên form để sửa nhãn và vị trí của nó.")).toBeInTheDocument();
  });

  it("label is edited per screen with the 'only this screen' note and a link to the table page", async () => {
    const p = setup();
    expect(screen.getByText("Chỉ đổi ở màn này.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Đổi nhãn gốc ở trang bảng" })).toHaveAttribute("href", p.tableHref);
    await userEvent.type(screen.getByRole("textbox", { name: "Nhãn khách thấy" }), "A");
    expect(p.onLabel).toHaveBeenCalledWith("A");
  });

  it("Lên / Xuống / Sang khung / Bỏ khỏi form / Xong call back; Lên is off at the very top", async () => {
    const p = setup();
    await userEvent.click(screen.getByRole("button", { name: "Lên" }));
    await userEvent.click(screen.getByRole("button", { name: "Xuống" }));
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Sang khung" }), sections[1].id);
    await userEvent.click(screen.getByRole("button", { name: "Bỏ khỏi form" }));
    await userEvent.click(screen.getByRole("button", { name: "Xong, bỏ chọn" }));
    expect(p.onUp).toHaveBeenCalled();
    expect(p.onDown).toHaveBeenCalled();
    expect(p.onToSection).toHaveBeenCalledWith(sections[1].id);
    expect(p.onRemove).toHaveBeenCalled();
    expect(p.onDone).toHaveBeenCalled();
  });

  it("canUp/canDown false disables the buttons", () => {
    setup({ canUp: false, canDown: false });
    expect(screen.getByRole("button", { name: "Lên" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Xuống" })).toBeDisabled();
  });

  it("a refused removal (required field) says why", () => {
    setup({ removeBlocked: true });
    expect(screen.getByRole("alert")).toHaveTextContent("Trường bắt buộc phải nằm trên form nên không bỏ được.");
  });

  it("a field not on the form shows the prominent 'Đặt lên form' instead of cell controls", async () => {
    const p = setup({ cell: null, field: null, unplaced: field });
    expect(screen.queryByRole("button", { name: "Bỏ khỏi form" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Đặt lên form" }));
    expect(p.onPlace).toHaveBeenCalled();
  });

  it("removing a controller asks first, naming the dependents; 'Giữ lại' keeps it", async () => {
    const p = setup({ dependents: ["tax", "vat"] });
    await userEvent.click(screen.getByRole("button", { name: "Bỏ khỏi form" }));
    expect(p.onRemove).not.toHaveBeenCalled();
    expect(screen.getByRole("alert")).toHaveTextContent("2 trường: tax, vat");
    await userEvent.click(screen.getByRole("button", { name: "Giữ lại" }));
    expect(screen.queryByRole("alert")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Bỏ khỏi form" }));
    await userEvent.click(screen.getByRole("button", { name: "Bỏ và xoá điều kiện" }));
    expect(p.onRemove).toHaveBeenCalledTimes(1);
  });

  it("'Tuỳ chọn riêng của màn này': help text is capped at 200 and counted", async () => {
    const p = setup();
    expect(screen.getByText("Tuỳ chọn riêng của màn này")).toBeInTheDocument();
    const box = screen.getByRole("textbox", { name: "Chữ hướng dẫn" });
    expect(box).toHaveAttribute("maxlength", "200");
    await userEvent.type(box, "x");
    expect(p.onOption).toHaveBeenLastCalledWith({ helpText: "x" });
    expect(screen.getByText("0/200 ký tự")).toBeInTheDocument();
  });

  it("width is 1..N of the section's columns or the full row (exclusive)", async () => {
    const p = setup();
    const sel = screen.getByRole("combobox", { name: "Độ rộng" });
    expect(Array.from((sel as HTMLSelectElement).options).map((o) => o.value)).toEqual(["auto", "1", "2", "full"]);
    await userEvent.selectOptions(sel, "2");
    expect(p.onOption).toHaveBeenLastCalledWith({ colspan: 2, fullWidth: undefined });
    await userEvent.selectOptions(sel, "full");
    expect(p.onOption).toHaveBeenLastCalledWith({ fullWidth: true, colspan: undefined });
    await userEvent.selectOptions(sel, "auto");
    expect(p.onOption).toHaveBeenLastCalledWith({ fullWidth: undefined, colspan: undefined });
  });

  it("read-only and required for this form only are separate checkboxes", async () => {
    const p = setup();
    await userEvent.click(screen.getByRole("checkbox", { name: "Chỉ đọc trên form này" }));
    expect(p.onOption).toHaveBeenLastCalledWith({ readonly: true });
    await userEvent.click(screen.getByRole("checkbox", { name: "Bắt buộc riêng form này" }));
    expect(p.onOption).toHaveBeenLastCalledWith({ required: true });
  });

  it("Bắt buộc nhập is a field switch that says it applies to every screen of the table", async () => {
    const p = setup();
    const sw = screen.getByRole("switch", { name: /Bắt buộc nhập/ });
    expect(screen.getByText(/Áp cho mọi màn của bảng/)).toBeInTheDocument();
    await userEvent.click(sw);
    expect(p.onRequired).toHaveBeenCalledWith(true);
  });

  it("an already-required field shows the switch on and can be turned off", async () => {
    const p = setup({ requiredOn: true });
    const sw = screen.getByRole("switch", { name: /Bắt buộc nhập/ });
    expect(sw).toBeChecked();
    await userEvent.click(sw);
    expect(p.onRequired).toHaveBeenCalledWith(false);
  });

  it("a field with a show-when condition locks the switch with the reason", () => {
    const withCond = draftFromSections([{ columns: 2, fields: ["kind", { field: "email", visibleWhen: { field: "kind", op: "empty" } }] }]);
    const c = withCond[0].cells[1] as DraftFieldCell;
    setup({ cell: c, sections: withCond, sectionId: withCond[0].id, fields: [{ name: "kind", fieldType: "text" } as FieldMeta, field] });
    expect(screen.getByRole("switch", { name: /Bắt buộc nhập/ })).toBeDisabled();
    expect(screen.getByText(/Trường đang có điều kiện hiện/)).toBeInTheDocument();
  });

  it("the four blocks follow the demo order: label, required, show-when, move", () => {
    setup();
    const text = document.body.textContent ?? "";
    const at = (s: string) => text.indexOf(s);
    expect(at("Nhãn khách thấy")).toBeLessThan(at("Bắt buộc nhập"));
    expect(at("Bắt buộc nhập")).toBeLessThan(at("Chỉ hiện khi"));
    expect(at("Chỉ hiện khi")).toBeLessThan(at("Lên"));
  });

  it("a field off the form still offers Bắt buộc nhập (it joins the form on Lưu)", async () => {
    const p = setup({ cell: null, field: null, unplaced: field });
    await userEvent.click(screen.getByRole("switch", { name: /Bắt buộc nhập/ }));
    expect(p.onRequired).toHaveBeenCalledWith(true);
  });
});
