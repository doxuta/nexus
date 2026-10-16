import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ApiRecord, FieldMeta } from "@/lib/api";
import { draftFromSections } from "@/lib/layoutDraft";
import { FormCanvas, type FormCanvasProps } from "./FormCanvas";

const F = (name: string, label: string, isRequired = false): FieldMeta => ({ name, label, fieldType: "text", isRequired }) as FieldMeta;
const fields = [F("a", "Tên", true), F("b", "Email"), F("c", "Chức danh")];
const draft = () =>
  draftFromSections([
    { title: "Thông tin chung", columns: 2, fields: ["a", { field: "b", labelOverride: "Thư điện tử" }] },
    { title: "Khác", columns: 2, fields: ["c"] },
  ]);
const record = { id: "r1", version: 1, data: { a: "Nguyễn", b: "x@y.vn" } } as ApiRecord;

function setup(over: Partial<FormCanvasProps> = {}) {
  const props: FormCanvasProps = {
    tableLabel: "Liên hệ", fields, draft: draft(), record, appName: "Phòng khám", selected: null,
    onSelect: vi.fn(), onDeselect: vi.fn(), onAddSection: vi.fn(), ...over,
  };
  return { props, ...render(<FormCanvas {...props} />) };
}
const cells = () => screen.getAllByRole("button", { name: /^Trường / });

describe("FormCanvas (specs/075 T013)", () => {
  it("draws the real form inside the customer frame, one control per field cell", () => {
    const { container } = setup();
    expect(screen.getByTestId("customer-preview-frame")).toBeInTheDocument();
    expect(cells()).toHaveLength(3);
    expect(container.querySelector(".slds-docked-form-footer")).toBeNull();
    // the real widget is a picture: inside inert, so never in the Tab order
    for (const c of cells()) expect(c.querySelector("[inert]")).not.toBeNull();
    expect(screen.getByText("Thư điện tử")).toBeInTheDocument();
  });

  it("accessible name: label, type, required, section, position", () => {
    setup();
    expect(screen.getByRole("button", { name: "Trường Tên, Văn bản, bắt buộc, khung Thông tin chung, vị trí 1 trên 2" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Trường Thư điện tử, Văn bản, khung Thông tin chung, vị trí 2 trên 2" })).toBeInTheDocument();
  });

  it("is ONE Tab stop (roving tabindex)", () => {
    setup();
    expect(cells().filter((c) => c.getAttribute("tabindex") === "0")).toHaveLength(1);
  });

  it("click selects as pointer, Enter/Space select as keyboard, aria-pressed follows selection", async () => {
    const onSelect = vi.fn();
    const { rerender, props } = setup({ onSelect });
    await userEvent.click(cells()[1]);
    expect(onSelect).toHaveBeenLastCalledWith("b", true);
    cells()[2].focus();
    await userEvent.keyboard("{Enter}");
    expect(onSelect).toHaveBeenLastCalledWith("c", false);
    await userEvent.keyboard(" ");
    expect(onSelect).toHaveBeenLastCalledWith("c", false);
    rerender(<FormCanvas {...props} selected="b" />);
    expect(cells()[1]).toHaveAttribute("aria-pressed", "true");
    expect(cells()[0]).toHaveAttribute("aria-pressed", "false");
  });

  it("arrows, Home and End walk the cells across sections; Esc deselects", async () => {
    const onDeselect = vi.fn();
    setup({ onDeselect, selected: "a" });
    cells()[0].focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(cells()[1]).toHaveFocus();
    await userEvent.keyboard("{ArrowDown}");
    expect(cells()[2]).toHaveFocus();
    await userEvent.keyboard("{Home}");
    expect(cells()[0]).toHaveFocus();
    await userEvent.keyboard("{End}");
    expect(cells()[2]).toHaveFocus();
    await userEvent.keyboard("{ArrowLeft}");
    expect(cells()[1]).toHaveFocus();
    await userEvent.keyboard("{Escape}");
    expect(onDeselect).toHaveBeenCalled();
  });

  it("no record: honest note and caption, no invented sample data, no role name", () => {
    setup({ record: null });
    expect(screen.getByText("Bảng chưa có bản ghi nên các ô để trống")).toBeInTheDocument();
    expect(screen.getByText("Chưa có bản ghi, các ô để trống")).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/Lễ tân|Dữ liệu minh hoạ/);
  });

  it("with a record the caption says so", () => {
    setup();
    expect(screen.getByText("Bản ghi đầu tiên của bảng")).toBeInTheDocument();
  });

  it("an empty section says what to do; 'Thêm khung' is a text button outside the customer frame", async () => {
    const onAddSection = vi.fn();
    setup({ draft: draftFromSections([{ title: "Trống", columns: 2, fields: [] }]), onAddSection });
    expect(screen.getByText("Khung này chưa có trường. Bấm Đặt ở danh sách bên phải.")).toBeInTheDocument();
    const add = screen.getByRole("button", { name: "Thêm khung" });
    expect(screen.getByTestId("customer-preview-frame")).not.toContainElement(add);
    await userEvent.click(add);
    expect(onAddSection).toHaveBeenCalled();
  });

  it("the customer's own Huỷ/Lưu row is inert and the live region announces the selection", async () => {
    setup();
    expect(screen.getByRole("button", { name: "Lưu" }).parentElement).toHaveAttribute("inert");
    await userEvent.click(cells()[0]);
    expect(document.querySelector('[aria-live="polite"]')?.textContent).toBe("Đã chọn Tên");
  });
});
