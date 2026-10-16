// specs/073 kh1a T144/T152 (FR-017): the line that opens each build step: name (focus target), sentence, enough / not yet.
import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { StepHeader } from "./StepHeader";

describe("StepHeader", () => {
  it("enough: names the step in an h2 that can take focus and says Đã đủ", () => {
    render(<StepHeader step={{ key: "data", complete: true }} />);
    const h2 = screen.getByRole("heading", { level: 2, name: "Dữ liệu" });
    expect(h2).toHaveAttribute("tabindex", "-1");
    expect(h2).toHaveClass("slds-assistive-text");
    expect(screen.getByText("Đã đủ")).toBeInTheDocument();
    expect(screen.queryByText("Chưa đủ")).toBeNull();
  });

  it("not yet: says Chưa đủ and what is needed", () => {
    render(<StepHeader step={{ key: "data", complete: false }} />);
    expect(screen.getByText("Chưa đủ")).toBeInTheDocument();
    expect(screen.getByText(/Có ít nhất một bảng\./)).toBeInTheDocument();
  });

  it("the status sentence leads, with the step's facts; the static sentence is only the tooltip", () => {
    const { container } = render(<StepHeader step={{ key: "data", complete: true, sublabel: "3 bảng" }} />);
    const line = container.querySelector("p")!;
    expect(line.textContent).toBe("Đã đủ. 3 bảng.");
    expect(line).toHaveAttribute("title", "Các bảng và trường của dự án.");
    expect(line.className).not.toMatch(/body_small/);
  });

  it("not yet: facts then the criterion on the same line", () => {
    const { container } = render(<StepHeader step={{ key: "data", complete: false, sublabel: "chưa có bảng" }} />);
    expect(container.querySelector("p")!.textContent).toBe("Chưa đủ. chưa có bảng. Có ít nhất một bảng.");
  });

  it("unknown (complete null): prints neither status word, shows the static sentence", () => {
    render(<StepHeader step={{ key: "data", complete: null }} />);
    expect(screen.queryByText("Đã đủ")).toBeNull();
    expect(screen.queryByText("Chưa đủ")).toBeNull();
    expect(screen.getByRole("heading", { level: 2, name: "Dữ liệu" })).toBeInTheDocument();
    expect(screen.getByText("Các bảng và trường của dự án.")).toBeInTheDocument();
  });

  it("status is coloured text, never a Badge or Pill", () => {
    const { container } = render(<StepHeader step={{ key: "data", complete: true }} />);
    expect(container.querySelector(".slds-badge,.slds-pill")).toBeNull();
    expect(container.querySelector(".nx-status")).not.toBeNull();
  });

  it("actions go to the right of the line", () => {
    render(<StepHeader step={{ key: "data", complete: true }} actions={<button>Bảng mới</button>} />);
    expect(screen.getByRole("button", { name: "Bảng mới" })).toBeInTheDocument();
  });

  it("every one of the eight steps has a name", () => {
    for (const key of ["data", "screens", "automation", "access", "reports", "branding", "trial", "deliver"] as const) {
      const { unmount } = render(<StepHeader step={{ key, complete: null }} />);
      expect(screen.getByRole("heading", { level: 2 }).textContent).not.toMatch(/^studio\./);
      unmount();
    }
  });
});
