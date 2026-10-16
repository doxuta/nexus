import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import type { FieldMeta, LayoutSection } from "@/lib/api";
import { SectionFields } from "./SectionGrid";

const fieldsByName = {
  a: { name: "a", label: "Alpha", fieldType: "text" },
  b: { name: "b", label: "Beta", fieldType: "text" },
} as unknown as Record<string, FieldMeta>;
const section: LayoutSection = { columns: 2, fields: ["a", { field: "b", colspan: 2 }, { spacer: true }] };
const props = { section, mode: "edit" as const, fieldsByName, values: {} };

describe("SectionFields renderCell (specs/075 T010)", () => {
  it("without renderCell the DOM is unchanged", () => {
    const { container } = render(<SectionFields {...props} />);
    expect(container.querySelectorAll(".slds-form__item")).toHaveLength(3);
    expect(container.querySelector("[data-wrap]")).toBeNull();
  });
  it("wraps every cell inside the same grid item (same class, colspan, index)", () => {
    const seen: number[] = [];
    const { container } = render(
      <SectionFields {...props} renderCell={(_cell, i, node) => { seen.push(i); return <div data-wrap={i}>{node}</div>; }} />,
    );
    expect(seen).toEqual([0, 1, 2]);
    const items = container.querySelectorAll(".slds-form__item");
    expect(items).toHaveLength(3);
    expect(items[1]).toHaveClass("md:col-span-2");
    expect(items[1].querySelector("[data-wrap='1']")).not.toBeNull();
    expect(screen.getByText("Alpha")).toBeInTheDocument();
  });
});
