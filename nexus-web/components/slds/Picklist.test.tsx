// Picklist — blueprint `picklist` (SLDS 2.264.1): a select-only combobox in a small container.
import { describe, it, expect } from "vitest";
import { useState } from "react";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Picklist } from "@/components/slds/Picklist";

const STAGES = [
  { value: "new", label: "Mới", group: "Đang mở" },
  { value: "work", label: "Đang xử lý", group: "Đang mở" },
  { value: "won", label: "Thắng", group: "Đã đóng" },
  { value: "lost", label: "Thua", group: "Đã đóng" },
];

describe("Picklist", () => {
  it("is the select-only combobox inside slds-combobox_container.slds-size_small", () => {
    render(<Picklist label="Giai đoạn" options={STAGES} variant="autocomplete" />);
    const el = screen.getByRole("combobox");
    expect(el.tagName).toBe("DIV");
    expect(el).toHaveClass("slds-input_faux", "slds-combobox__input");
    expect(el.closest(".slds-combobox_container")).toHaveClass("slds-size_small");
  });

  it("group heading labels (blueprint 'Group heading label'), Enter chooses", async () => {
    render(<Picklist label="Giai đoạn" options={STAGES} />);
    screen.getByRole("combobox").focus();
    await userEvent.keyboard("{ArrowDown}");
    expect(screen.getAllByRole("group").map((g) => g.getAttribute("aria-label"))).toEqual(["Đang mở", "Đã đóng"]);
    await userEvent.keyboard("{ArrowDown}{ArrowDown}{Enter}");
    expect(screen.getByRole("combobox")).toHaveTextContent("Thắng");
  });

  it("multi: 'n mục' in the field, pills in the selection group below (blueprint 'Closed - Options Selected')", async () => {
    function Multi() {
      const [v, setV] = useState<string[]>(["new", "won"]);
      return <Picklist label="Giai đoạn" options={STAGES} multiple value={v} onChange={setV} />;
    }
    render(<Multi />);
    expect(screen.getByRole("combobox")).toHaveTextContent("Đã chọn 2 mục");
    const pills = screen.getByRole("list", { name: "Các mục đã chọn:" });
    expect(pills.parentElement).toHaveClass("slds-listbox_selection-group");
    expect(within(pills).getAllByRole("listitem")).toHaveLength(2);
  });
});
