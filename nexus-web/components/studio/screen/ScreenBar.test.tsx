import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { EntityMeta, FieldMeta } from "@/lib/api";
import { ScreenBar, type ScreenBarProps } from "./ScreenBar";

const entities = [
  { name: "contacts", label: "Liên hệ" },
  { name: "deals", label: "Cơ hội" },
] as EntityMeta[];
const miss = [{ name: "a", label: "Tên" }, { name: "b", label: "Họ" }] as FieldMeta[];

function setup(over: Partial<ScreenBarProps> = {}) {
  const props: ScreenBarProps = {
    entities, bang: "contacts", man: "form", missing: [], dirty: false, saving: false, locked: false, canReset: false,
    onPick: vi.fn(), onSave: vi.fn(), onDiscard: vi.fn(), onReset: vi.fn(), ...over,
  };
  render(<ScreenBar {...props} />);
  return props;
}

describe("ScreenBar (specs/075 T015)", () => {
  it("picker groups by table with the three screens each", () => {
    setup();
    const groups = screen.getAllByRole("group").filter((g) => g.tagName === "OPTGROUP");
    expect(groups.map((g) => g.getAttribute("label"))).toEqual(["Liên hệ", "Cơ hội"]);
    expect(screen.getByRole("option", { name: "Form Liên hệ" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "Trang hồ sơ Cơ hội" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "Danh sách Liên hệ" })).toBeInTheDocument();
  });

  it("choosing another screen only reports it (the editor decides); the select keeps its value", async () => {
    const p = setup();
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Chọn màn hình" }), "deals|form");
    expect(p.onPick).toHaveBeenCalledWith("deals", "form");
    expect(screen.getByRole("combobox", { name: "Chọn màn hình" })).toHaveValue("contacts|form");
  });

  it("status: 'Đã đủ' or the missing required fields by name", () => {
    setup({ missing: miss });
    expect(screen.getByText("Chưa đủ. Còn 2 trường bắt buộc chưa nằm trên form: Tên, Họ.")).toBeInTheDocument();
  });

  it("clean: 'Đã đủ', Lưu is off, no 'Chưa lưu', no 'Bỏ thay đổi', no menu", () => {
    setup();
    expect(screen.getByText("Đã đủ")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Lưu thay đổi" })).toBeDisabled();
    expect(screen.queryByText("Chưa lưu")).toBeNull();
    expect(screen.queryByRole("button", { name: "Bỏ thay đổi" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Thao tác của màn hình" })).toBeNull();
  });

  it("dirty: 'Chưa lưu', Bỏ thay đổi, one brand Lưu thay đổi that is on", async () => {
    const p = setup({ dirty: true });
    expect(screen.getByText("Chưa lưu")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Bỏ thay đổi" }));
    expect(p.onDiscard).toHaveBeenCalled();
    const save = screen.getByRole("button", { name: "Lưu thay đổi" });
    expect(save).toHaveClass("slds-button_brand");
    expect(save).toBeEnabled();
    await userEvent.click(save);
    expect(p.onSave).toHaveBeenCalled();
  });

  it("locked or saving turns Lưu off", () => {
    setup({ dirty: true, locked: true });
    expect(screen.getByRole("button", { name: "Lưu thay đổi" })).toBeDisabled();
  });

  it("the reset item exists only for a saved screen", async () => {
    const p = setup({ canReset: true });
    await userEvent.click(screen.getByRole("button", { name: "Thao tác của màn hình" }));
    await userEvent.click(screen.getByRole("menuitem", { name: "Đưa màn về màn tự sinh" }));
    expect(p.onReset).toHaveBeenCalled();
  });
});
