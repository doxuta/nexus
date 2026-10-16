import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { RelatedLists, type RelatedSection } from "./RelatedLists";

const base = (p: Partial<RelatedSection>): RelatedSection => ({ id: "a", title: "Mục A", state: "ready", items: [], empty: "Trống A", ...p });

describe("RelatedLists — each section has its own four states", () => {
  it("loading shows a spinner and no empty sentence", () => {
    const { container } = render(<RelatedLists label="Liên quan" sections={[base({ state: "loading" })]} />);
    expect(container.querySelector(".slds-spinner")).not.toBeNull();
    expect(screen.queryByText("Trống A")).toBeNull();
  });
  it("error marks only that section and offers a retry", async () => {
    const retry = vi.fn();
    render(<RelatedLists label="Liên quan" sections={[base({ state: "error", error: "Hỏng", onRetry: retry }), base({ id: "b", title: "Mục B", items: [<span key="x">Có</span>] })]} />);
    expect(screen.getByText("Hỏng")).toBeInTheDocument();
    expect(screen.getByText("Có")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Thử lại" }));
    expect(retry).toHaveBeenCalled();
  });
  it("empty shows the section's sentence; ready lists items with a count", () => {
    render(<RelatedLists label="Liên quan" sections={[base({}), base({ id: "b", title: "Mục B", count: 2, items: [<span key="1">một</span>, <span key="2">hai</span>] })]} />);
    expect(screen.getByText("Trống A")).toBeInTheDocument();
    const b = screen.getByRole("heading", { name: /Mục B/ }).closest("section")!;
    expect(within(b).getByText("(2)")).toBeInTheDocument();
    expect(within(b).getAllByRole("listitem")).toHaveLength(2);
  });
  it("is flat: dividers list, no card", () => {
    const { container } = render(<RelatedLists label="Liên quan" sections={[base({ items: [<i key="1">x</i>] })]} />);
    expect(container.querySelector(".slds-has-dividers_bottom")).not.toBeNull();
    expect(container.querySelector(".slds-card")).toBeNull();
  });
});
