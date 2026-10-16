// specs/048 T157 — DuelingPicklist picks and orders the operations; the error lands on the right control.
import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { NormalizeActionEditor } from "./NormalizeActionEditor";

const FIELDS = [
  { name: "email", label: "Email", fieldType: "email" },
  { name: "name", label: "Họ tên", fieldType: "text" },
  { name: "stage", label: "Giai đoạn", fieldType: "picklist" },
];

describe("NormalizeActionEditor", () => {
  it("offers text/email targets only and shows the stored ops in order", () => {
    render(<NormalizeActionEditor params={{ field: "email", ops: JSON.stringify(["trim", "lower"]) }} fields={FIELDS} onChange={vi.fn()} />);
    const target = screen.getByRole("combobox", { name: "Trường cần chuẩn hoá" });
    expect(within(target).getByRole("option", { name: "Email" })).toBeInTheDocument();
    expect(within(target).getByRole("option", { name: "Họ tên" })).toBeInTheDocument();
    expect(within(target).queryByRole("option", { name: "Giai đoạn" })).toBeNull();
    const chosen = screen.getAllByRole("listbox")[1].querySelectorAll('[role="option"]');
    expect([...chosen].map((o) => o.textContent)).toEqual(["Bỏ khoảng trắng hai đầu", "Chữ thường"]);
  });

  it("moving an operation across emits the ops JSON array", async () => {
    const onChange = vi.fn();
    render(<NormalizeActionEditor params={{ field: "email", ops: "[]" }} fields={FIELDS} onChange={onChange} />);
    await userEvent.click(screen.getByRole("option", { name: "Chữ thường" }));
    await userEvent.keyboard("{Meta>}{ArrowRight}{/Meta}");
    expect(JSON.parse(onChange.mock.calls.at(-1)![0].ops)).toEqual(["lower"]);
  });

  it("an ops refusal sits at the picklist, another at the field", () => {
    const { rerender } = render(<NormalizeActionEditor params={{ field: "email", ops: "[]" }} fields={FIELDS} error="chuẩn hoá cần 1–4 phép" onChange={vi.fn()} />);
    expect(screen.getByRole("group", { name: /Phép chuẩn hoá/ })).toHaveTextContent("cần 1–4 phép");
    rerender(<NormalizeActionEditor params={{ field: "stage", ops: "[]" }} fields={FIELDS} error="chuẩn hoá chỉ áp cho field chữ" onChange={vi.fn()} />);
    expect(screen.getByRole("combobox", { name: "Trường cần chuẩn hoá" }).closest(".slds-form-element")).toHaveTextContent("chỉ áp cho field chữ");
  });
});
