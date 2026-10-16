// DropZone — blueprint `drop-zone` (SLDS 2.264.1): screen-builder region + component container.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { DropZone, DropZoneItem } from "@/components/slds/DropZone";

describe("DropZone", () => {
  it("rest: bare slds-drop-zone around its components, no label or slot", () => {
    const { container } = render(
      <DropZone label="Nội dung" slot={0}>
        <p>A</p>
      </DropZone>,
    );
    const zone = container.firstElementChild!;
    expect(zone).toHaveClass("slds-drop-zone");
    expect(zone).not.toHaveClass("slds-drop-zone_drag");
    expect(zone.querySelector(".slds-drop-zone__label, .slds-drop-zone_drag__slot")).toBeNull();
  });

  it("drag over: slds-drop-zone_drag, label tab with builder icon, insertion slot at the given index", () => {
    const { container } = render(
      <DropZone dragOver label="Nội dung" slot={1}>
        <p>A</p>
        <p>B</p>
      </DropZone>,
    );
    const zone = container.firstElementChild!;
    expect(zone).toHaveClass("slds-drop-zone", "slds-drop-zone_drag");
    const tab = zone.firstElementChild!;
    expect(tab).toHaveClass("slds-drop-zone__label");
    expect(tab.querySelector(".slds-icon_container.slds-icon-utility-builder.slds-current-color")).not.toBeNull();
    expect(tab.querySelector("h2")).toHaveTextContent("Nội dung");
    expect(Array.from(zone.children).map((c) => c.className || c.textContent)).toEqual([
      "slds-drop-zone__label",
      "A",
      "slds-drop-zone_drag__slot",
      "B",
    ]);
  });

  it("item: container[tabindex=-1] with label button 'Sửa: <tên>', Move/Remove brand x-small buttons", async () => {
    const onEdit = vi.fn();
    const onRemove = vi.fn();
    const { container } = render(
      <DropZoneItem label="Thông tin chung" hovered onEdit={onEdit} onRemove={onRemove} moveHandle={{ "aria-roledescription": "sortable" }}>
        <p>Thân</p>
      </DropZoneItem>,
    );
    const item = container.firstElementChild!;
    expect(item).toHaveClass("slds-drop-zone__container", "slds-is-hovered");
    expect(item).toHaveAttribute("tabindex", "-1");
    expect(item.querySelector(".slds-drop-zone__label")).toHaveClass("slds-drop-zone__label_container");
    const edit = screen.getByRole("button", { name: "Sửa: Thông tin chung" });
    expect(edit).toHaveClass("slds-drop-zone__label_button", "slds-button_reset");
    await userEvent.click(edit);
    expect(onEdit).toHaveBeenCalled();
    const group = item.querySelector(".slds-drop-zone__actions > .slds-button-group[role=group]")!;
    const move = screen.getByRole("button", { name: "Di chuyển" });
    expect(group).toContainElement(move);
    expect(move).toHaveClass("slds-button_icon-brand", "slds-button_icon-x-small");
    expect(move).toHaveAttribute("aria-roledescription", "sortable");
    await userEvent.click(screen.getByRole("button", { name: "Gỡ khỏi trang" }));
    expect(onRemove).toHaveBeenCalled();
  });

  it("item without actions renders no action group", () => {
    const { container } = render(<DropZoneItem label="Liên hệ" />);
    expect(container.querySelector(".slds-drop-zone__actions")).toBeNull();
  });
});
