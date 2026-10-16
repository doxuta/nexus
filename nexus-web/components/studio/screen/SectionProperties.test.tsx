import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { draftFromSections } from "@/lib/layoutDraft";
import { SectionProperties, SectionsPanel, type SectionPropertiesProps } from "./SectionProperties";

const sections = draftFromSections([
  { title: "Thông tin chung", columns: 2, fields: ["a", "b"] },
  { title: "Khác", columns: 3, fields: [] },
]);

function setup(over: Partial<SectionPropertiesProps> = {}) {
  const fn = () => vi.fn();
  const props: SectionPropertiesProps = {
    section: sections[0], canUp: false, canDown: true, removeBlocked: false,
    onTitle: fn(), onColumns: fn(), onUp: fn(), onDown: fn(), onAddSpacer: fn(), onRemove: fn(), ...over,
  };
  render(<SectionProperties {...props} />);
  return props;
}

describe("SectionProperties (specs/075 T043)", () => {
  it("title is edited in place", async () => {
    const p = setup();
    await userEvent.type(screen.getByRole("textbox", { name: "Tiêu đề khung" }), "!");
    expect(p.onTitle).toHaveBeenLastCalledWith("Thông tin chung!");
  });

  it("columns offer 1 to 4 and report the choice", async () => {
    const p = setup();
    const sel = screen.getByRole("combobox", { name: "Số cột" });
    expect(Array.from((sel as HTMLSelectElement).options).map((o) => o.value)).toEqual(["1", "2", "3", "4"]);
    await userEvent.selectOptions(sel, "3");
    expect(p.onColumns).toHaveBeenCalledWith(3);
  });

  it("Lên/Xuống respect the edges; 'Thêm ô trống' and 'Xoá khung' fire", async () => {
    const p = setup();
    expect(screen.getByRole("button", { name: "Khung lên" })).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Khung xuống" }));
    expect(p.onDown).toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Thêm ô trống" }));
    expect(p.onAddSpacer).toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Xoá khung" }));
    expect(p.onRemove).toHaveBeenCalled();
  });

  it("a refused removal says why", () => {
    setup({ removeBlocked: true });
    expect(screen.getByRole("alert")).toHaveTextContent("Khung này giữ trường bắt buộc");
  });
});

describe("SectionsPanel", () => {
  it("lists the sections with column and cell counts; picking one reports its id", async () => {
    const onPick = vi.fn();
    render(<SectionsPanel sections={sections} selectedId={null} onPick={onPick} />);
    expect(screen.getByRole("button", { name: /Thông tin chung/ })).toHaveTextContent("2 cột · 2 ô");
    await userEvent.click(screen.getByRole("button", { name: /Khác/ }));
    expect(onPick).toHaveBeenCalledWith(sections[1].id);
  });
});
