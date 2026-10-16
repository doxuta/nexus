import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { FieldMeta } from "@/lib/api";
import { draftFromSections, type DraftFieldCell } from "@/lib/layoutDraft";
import { VisibleWhenRow, type VisibleWhenRowProps } from "./VisibleWhenRow";

const F = (name: string, fieldType: string, extra: Partial<FieldMeta> = {}): FieldMeta => ({ name, label: name.toUpperCase(), fieldType, ...extra });
const fields = [F("tax", "text"), F("kind", "picklist", { options: { values: ["person", "biz"] } }), F("notes", "textarea")];

function setup(over: Partial<VisibleWhenRowProps> = {}, cellPatch: Partial<DraftFieldCell> = {}, target: FieldMeta = fields[0]) {
  const draft = draftFromSections([{ title: "A", columns: 2, fields: ["tax", "kind", "notes"] }]);
  const cell = { ...(draft[0].cells[0] as DraftFieldCell), ...cellPatch };
  const props: VisibleWhenRowProps = { cell, field: target, fields, draft, onChange: vi.fn(), ...over };
  render(<VisibleWhenRow {...props} />);
  return props;
}

describe("VisibleWhenRow (specs/075 T062)", () => {
  it("offers only valid controllers (kind), not the field itself or a textarea", async () => {
    setup();
    await userEvent.click(screen.getByRole("combobox", { name: "Trường" }));
    expect(screen.getByRole("option", { name: "KIND" })).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: "NOTES" })).toBeNull();
    expect(screen.queryByRole("option", { name: "TAX" })).toBeNull();
  });

  it("required target: locked with the reason, no controls", () => {
    setup({}, {}, F("tax", "text", { isRequired: true }));
    expect(screen.getByText(/Trường bắt buộc phải luôn hiện/)).toBeInTheDocument();
    expect(screen.queryByRole("combobox")).toBeNull();
  });

  it("notIn written elsewhere shows read-only text and 'Xoá điều kiện' clears it", async () => {
    const p = setup({}, { visibleWhen: { field: "kind", op: "notIn", value: ["biz"] } });
    expect(screen.getByText(/do nơi khác đặt/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Xoá điều kiện" }));
    expect(p.onChange).toHaveBeenCalledWith(undefined);
  });

  it("shows the server 422 sentence under the section", () => {
    setup({ error: "Điều kiện chưa hợp lệ" });
    expect(screen.getByRole("alert")).toHaveTextContent("Điều kiện chưa hợp lệ");
  });

  it("existing condition is loaded and emits a typed VisibleWhen on operator change", async () => {
    const p = setup({}, { visibleWhen: { field: "kind", op: "eq", value: "biz" } });
    await userEvent.click(screen.getByRole("combobox", { name: "Toán tử" }));
    await userEvent.click(screen.getByRole("option", { name: "không rỗng" }));
    expect(p.onChange).toHaveBeenLastCalledWith({ field: "kind", op: "notEmpty" });
  });
});
