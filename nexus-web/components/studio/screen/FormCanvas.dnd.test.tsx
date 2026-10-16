import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { DndContext } from "@dnd-kit/core";
import type { FieldMeta } from "@/lib/api";
import { draftFromSections } from "@/lib/layoutDraft";
import { FormCanvas, type FormCanvasProps } from "./FormCanvas";

const F = (name: string, label: string): FieldMeta => ({ name, label, fieldType: "text", isRequired: false }) as FieldMeta;
const fields = [F("a", "Tên"), F("b", "Email")];
const draft = () => draftFromSections([{ title: "Chung", columns: 2, fields: ["a", "b", { spacer: true }] }, { title: "Rỗng", columns: 2, fields: [] }]);

function setup(over: Partial<FormCanvasProps> = {}) {
  const props: FormCanvasProps = {
    tableLabel: "Liên hệ", fields, draft: draft(), record: null, appName: "App", selected: null,
    onSelect: vi.fn(), onDeselect: vi.fn(), onAddSection: vi.fn(), ...over,
  };
  const ui = (p: FormCanvasProps) => <DndContext><FormCanvas {...p} /></DndContext>;
  const r = render(ui(props));
  return { props, ...r, again: (o: Partial<FormCanvasProps>) => r.rerender(ui({ ...props, ...o })) };
}

describe("FormCanvas drag and drop (specs/075 T041)", () => {
  it("the drop-zone class exists ONLY while something is dragged", () => {
    const { container, again } = setup();
    expect(container.querySelectorAll(".slds-drop-zone")).toHaveLength(0);
    again({ dragging: true });
    expect(container.querySelectorAll(".slds-drop-zone").length).toBeGreaterThan(0);
  });

  it("each cell has its own handle named 'Kéo {trường}', 24px target", () => {
    setup();
    const h = screen.getByRole("button", { name: "Kéo Tên" });
    expect(h).toHaveStyle({ minWidth: "24px", minHeight: "24px" });
    expect(screen.getByRole("button", { name: "Kéo Ô trống" })).toBeInTheDocument();
  });

  it("only the Tab-stop cell's handle is tabbable; handle shows on hover/focus/touch through classes", () => {
    setup();
    const tabbable = screen.getAllByRole("button", { name: /^Kéo / }).filter((b) => b.getAttribute("tabindex") === "0");
    expect(tabbable).toHaveLength(1);
    const group = screen.getByRole("button", { name: "Kéo Tên" }).parentElement!;
    expect(group.className).toMatch(/opacity-0/);
    expect(group.className).toMatch(/group-hover:opacity-100/);
    expect(group.className).toMatch(/group-focus-within:opacity-100/);
    expect(group.className).toMatch(/hover:none/);
  });

  it("Enter/Space on a cell select it and never lift it (the sensor is on the handle only)", async () => {
    const onSelect = vi.fn();
    setup({ onSelect });
    const cell = screen.getByRole("button", { name: /^Trường Email/ });
    cell.focus();
    await userEvent.keyboard("{Enter}");
    expect(onSelect).toHaveBeenLastCalledWith("b", false);
    await userEvent.keyboard(" ");
    expect(onSelect).toHaveBeenCalledTimes(2);
    expect(document.body.textContent).not.toMatch(/Đã nhấc/);
  });

  it("a spacer has 'Xoá ô trống'; an empty section is a drop target", async () => {
    const onRemoveSpacer = vi.fn();
    const { props } = setup({ onRemoveSpacer });
    await userEvent.click(screen.getByRole("button", { name: "Xoá ô trống" }));
    expect(onRemoveSpacer).toHaveBeenCalledWith(props.draft[0].cells[2].id);
    expect(screen.getByText("Khung này chưa có trường. Bấm Đặt ở danh sách bên phải.")).toBeInTheDocument();
  });
});
