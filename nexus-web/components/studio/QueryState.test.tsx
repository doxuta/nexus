// specs/073 kh1a T144/T152 (FR-018): loading, error (sentence by code + retry) and empty, in the working surface.
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ApiError } from "@/lib/api";
import { QueryState } from "./QueryState";

const q = (over: Record<string, unknown> = {}) => ({ isPending: false, isError: false, error: null, data: ["x"], refetch: vi.fn(), ...over });

describe("QueryState", () => {
  it("loading: a spinner, no children", () => {
    render(<QueryState query={q({ isPending: true, data: undefined })}>content</QueryState>);
    expect(screen.getByRole("status")).toBeInTheDocument();
    expect(screen.queryByText("content")).toBeNull();
  });

  it("error: the sentence for the code, and Thử lại calls refetch", () => {
    const query = q({ isError: true, data: undefined, error: new ApiError(403, "FORBIDDEN", "raw") });
    render(<QueryState query={query}>content</QueryState>);
    expect(screen.getByText("Bạn không có quyền thực hiện thao tác này.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Thử lại" }));
    expect(query.refetch).toHaveBeenCalledTimes(1);
    expect(screen.queryByText("content")).toBeNull();
  });

  it("empty: shows the empty node instead of the children", () => {
    render(
      <QueryState query={q({ data: [] })} empty={<p>Chưa có gì</p>}>
        content
      </QueryState>,
    );
    expect(screen.getByText("Chưa có gì")).toBeInTheDocument();
    expect(screen.queryByText("content")).toBeNull();
  });

  it("data present: the children, and no empty node", () => {
    render(
      <QueryState query={q()} empty={<p>Chưa có gì</p>}>
        content
      </QueryState>,
    );
    expect(screen.getByText("content")).toBeInTheDocument();
    expect(screen.queryByText("Chưa có gì")).toBeNull();
  });

  it("an empty object or a missing `empty` prop never hides the children", () => {
    render(<QueryState query={q({ data: [] })}>content</QueryState>);
    expect(screen.getByText("content")).toBeInTheDocument();
  });
});
