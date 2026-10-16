// specs/075 T141/T142: node = text only; edge kind = arrowheads + dash + words, never colour.
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

vi.mock("@xyflow/react", () => ({
  Handle: () => null,
  Position: { Left: "left", Right: "right" },
  BaseEdge: (p: Record<string, unknown>) => <path data-testid="edge" data-end={String(p.markerEnd ?? "")} data-start={String(p.markerStart ?? "")} style={p.style as React.CSSProperties} />,
  EdgeLabelRenderer: ({ children }: { children: React.ReactNode }) => <>{children}</>,
  getSmoothStepPath: () => ["M0 0", 0, 0],
}));

import { RelationNode, type RelationNodeData } from "./RelationNode";
import { RelationEdge } from "./RelationEdge";

const fields = ["a", "b", "c", "d", "e"].map((n) => ({ name: n, label: `Trường ${n}` }));
const base = (over: Partial<RelationNodeData> = {}): RelationNodeData => ({
  name: "crm_accounts", label: "Khách hàng", fields, selected: false, match: false, onSelect: vi.fn(), onMove: vi.fn(), ...over,
});
const node = (data: RelationNodeData) => render(<RelationNode {...({ data } as unknown as React.ComponentProps<typeof RelationNode>)} />);

describe("RelationNode", () => {
  it("is a text block: bold name, field count, at most 3 extra fields, no initials tile", () => {
    const { container } = node(base());
    expect(screen.getByText("Khách hàng")).toBeInTheDocument();
    expect(screen.getByText("5 trường")).toBeInTheDocument();
    expect(screen.getAllByText(/^Trường /)).toHaveLength(3);
    expect(screen.getByText("và 2 trường nữa")).toBeInTheDocument();
    expect(container.querySelector("[class*=slds-icon]")).toBeNull();
    expect(container.innerHTML).not.toMatch(/KH|EntityBadge|slds-avatar/);
  });
  it("selected = aria-pressed + 2px brand border (not colour alone)", () => {
    node(base({ selected: true }));
    const b = screen.getByRole("button");
    expect(b).toHaveAttribute("aria-pressed", "true");
    expect(b.style.borderWidth).toBe("2px");
    expect(b.style.borderColor).toContain("brand-base-50");
  });
  it("click selects; arrow keys move 12px", () => {
    const d = base();
    node(d);
    const b = screen.getByRole("button");
    fireEvent.click(b);
    expect(d.onSelect).toHaveBeenCalledWith("crm_accounts");
    fireEvent.keyDown(b, { key: "ArrowRight" });
    fireEvent.keyDown(b, { key: "ArrowUp" });
    expect(d.onMove).toHaveBeenNthCalledWith(1, "crm_accounts", 12, 0);
    expect(d.onMove).toHaveBeenNthCalledWith(2, "crm_accounts", 0, -12);
  });
});

const edge = (relType: "1-N" | "N-N" | "1-1", selected = false) =>
  render(<svg><RelationEdge {...({ id: "e", sourceX: 0, sourceY: 0, targetX: 9, targetY: 9, sourcePosition: "right", targetPosition: "left", data: { relType, selected, parallelIndex: 0 } } as unknown as React.ComponentProps<typeof RelationEdge>)} /></svg>);

describe("RelationEdge", () => {
  it.each([
    ["1-N", "Một–nhiều", undefined, true, false],
    ["N-N", "Nhiều–nhiều", "6 4", true, true],
    ["1-1", "Một–một", undefined, false, false],
  ] as const)("%s = %s: dash %s, end arrow %s, start arrow %s", (type, word, dash, end, start) => {
    edge(type);
    const p = screen.getByTestId("edge");
    expect(screen.getByText(word)).toBeInTheDocument();
    expect(p.style.strokeDasharray).toBe(dash ?? "");
    expect(!!p.dataset.end).toBe(end);
    expect(!!p.dataset.start).toBe(start);
    expect(p.style.stroke).toContain("neutral-base-30");
  });
  it("selected edge: brand stroke and thicker line", () => {
    edge("1-N", true);
    const p = screen.getByTestId("edge");
    expect(p.style.stroke).toContain("brand-base-50");
    expect(p.style.strokeWidth).toBe("3");
  });
});
