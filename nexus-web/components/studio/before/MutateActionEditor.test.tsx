// specs/048 T156 — "Tự điền giá trị": target filter, the three value sources, the "Chỉ điền khi trống" toggle.
import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MutateActionEditor } from "./MutateActionEditor";

const FIELDS = [
  { name: "company", label: "Công ty", fieldType: "text" },
  { name: "email", label: "Email", fieldType: "text" },
  { name: "probability", label: "Xác suất", fieldType: "number" },
  { name: "owner", label: "Phụ trách", fieldType: "user" },
  { name: "account", label: "Khách hàng", fieldType: "relation", options: { relatedEntity: "accounts" } },
  { name: "code", label: "Mã", fieldType: "auto_number" },
  { name: "total", label: "Tổng", fieldType: "formula" },
];

describe("MutateActionEditor", () => {
  it("target picker drops link / person / computed / sequence fields", () => {
    render(<MutateActionEditor params={{}} fields={FIELDS} onChange={vi.fn()} />);
    const picker = screen.getByRole("combobox", { name: "Trường" });
    const names = within(picker).getAllByRole("option").map((o) => o.textContent);
    expect(names).toEqual(expect.arrayContaining(["Công ty", "Email", "Xác suất"]));
    for (const gone of ["Phụ trách", "Khách hàng", "Mã", "Tổng"]) expect(names).not.toContain(gone);
  });

  it("'Mẫu chữ' is offered for a text target only; a number target gets the typed box and copy-from", () => {
    const { rerender } = render(<MutateActionEditor params={{ field: "company" }} fields={FIELDS} onChange={vi.fn()} />);
    expect(screen.getByRole("radio", { name: "Mẫu chữ" })).toBeInTheDocument();
    rerender(<MutateActionEditor params={{ field: "probability" }} fields={FIELDS} onChange={vi.fn()} />);
    expect(screen.queryByRole("radio", { name: "Mẫu chữ" })).toBeNull();
    expect(screen.getByRole("radio", { name: "Giá trị cố định" })).toBeChecked();
  });

  it("template source: typing writes srcValue; the copy source lists only same-type fields", async () => {
    const onChange = vi.fn();
    const { rerender } = render(<MutateActionEditor params={{ field: "company", valueSource: "template", srcValue: "" }} fields={FIELDS} onChange={onChange} />);
    await userEvent.type(screen.getByRole("textbox", { name: "Mẫu chữ" }), "x");
    expect(onChange).toHaveBeenLastCalledWith({ field: "company", valueSource: "template", srcValue: "x" });
    rerender(<MutateActionEditor params={{ field: "company", valueSource: "field", srcValue: "" }} fields={FIELDS} onChange={onChange} />);
    const from = within(screen.getByRole("combobox", { name: "Chép từ trường" })).getAllByRole("option").map((o) => o.textContent);
    expect(from).toContain("Email");
    expect(from).not.toContain("Xác suất");
  });

  it("the toggle sets and clears onlyIfEmpty; a server refusal shows at the value", async () => {
    const onChange = vi.fn();
    const { rerender } = render(<MutateActionEditor params={{ field: "company" }} fields={FIELDS} onChange={onChange} />);
    await userEvent.click(screen.getByRole("switch", { name: /Chỉ điền khi trống/ }));
    expect(onChange).toHaveBeenLastCalledWith(expect.objectContaining({ onlyIfEmpty: "true" }));
    rerender(<MutateActionEditor params={{ field: "company", onlyIfEmpty: "true" }} fields={FIELDS} onChange={onChange} />);
    await userEvent.click(screen.getByRole("switch", { name: /Chỉ điền khi trống/ }));
    expect(onChange.mock.lastCall![0]).not.toHaveProperty("onlyIfEmpty");
    rerender(<MutateActionEditor params={{ field: "company" }} fields={FIELDS} error="giá trị không hợp lệ" errorField="company" onChange={onChange} />);
    expect(screen.getByText("giá trị không hợp lệ")).toBeInTheDocument();
  });
});
