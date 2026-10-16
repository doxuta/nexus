// specs/079 f7-nang-cap-ap (FR-140): the docked apply bar carries only the count sentence and the buttons.
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { UpgradeApplyBar } from "./UpgradeApplyBar";

const bar = (over: Partial<Parameters<typeof UpgradeApplyBar>[0]> = {}) => {
  const onApply = vi.fn();
  const onReset = vi.fn();
  render(<UpgradeApplyBar writes={2} adoptable={1} applying={false} onApply={onApply} onReset={onReset} {...over} />);
  return { onApply, onReset };
};

describe("UpgradeApplyBar", () => {
  it("says how many items it writes and how many it only records, and applies on click", () => {
    const { onApply } = bar();
    expect(screen.getByText("Sẽ ghi 2 mục.")).toBeInTheDocument();
    expect(screen.getByText("Ghi nhận thêm 1 mục đã khớp sẵn.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Áp nâng cấp (2 mục)" }));
    expect(onApply).toHaveBeenCalledTimes(1);
  });

  it("is enabled when there is nothing to write but something to record", () => {
    bar({ writes: 0, adoptable: 3 });
    expect(screen.getByRole("button", { name: "Áp nâng cấp (0 mục)" })).toBeEnabled();
  });

  it("is disabled when there is nothing to write and nothing to adopt", () => {
    bar({ writes: 0, adoptable: 0 });
    expect(screen.getByRole("button", { name: "Áp nâng cấp (0 mục)" })).toBeDisabled();
  });

  it("is disabled while applying, so a double click sends one request", () => {
    bar({ applying: true });
    expect(screen.getByRole("button", { name: "Áp nâng cấp (2 mục)" })).toBeDisabled();
  });

  it("'Đặt lại lựa chọn' hands control back to the page", () => {
    const { onReset } = bar();
    fireEvent.click(screen.getByRole("button", { name: "Đặt lại lựa chọn" }));
    expect(onReset).toHaveBeenCalledTimes(1);
  });
});
