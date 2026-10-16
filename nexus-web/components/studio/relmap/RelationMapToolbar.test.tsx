// specs/075 T143: one gentle row of four labelled icon buttons; no minimap, no snap switch.
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { RelationMapToolbar } from "./RelationMapToolbar";

describe("RelationMapToolbar", () => {
  it("has Vietnamese names for every icon button and fires its own handler", () => {
    const h = { onZoomIn: vi.fn(), onZoomOut: vi.fn(), onFit: vi.fn(), onRelayout: vi.fn() };
    render(<RelationMapToolbar {...h} />);
    fireEvent.click(screen.getByRole("button", { name: "Phóng to" }));
    fireEvent.click(screen.getByRole("button", { name: "Thu nhỏ" }));
    fireEvent.click(screen.getByRole("button", { name: "Vừa khung" }));
    fireEvent.click(screen.getByRole("button", { name: "Xếp lại" }));
    expect(h.onZoomIn).toHaveBeenCalledTimes(1);
    expect(h.onZoomOut).toHaveBeenCalledTimes(1);
    expect(h.onFit).toHaveBeenCalledTimes(1);
    expect(h.onRelayout).toHaveBeenCalledTimes(1);
    expect(screen.getAllByRole("button")).toHaveLength(4);
    expect(screen.queryByText(/minimap|snap|lưới/i)).toBeNull();
  });
});
