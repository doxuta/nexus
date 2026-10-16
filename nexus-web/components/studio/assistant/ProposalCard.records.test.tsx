import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { ProposalCard } from "./ProposalCard";
import type { AiDraftItem } from "@/lib/aiBuilder";

const RECORDS: AiDraftItem = { kind: "records", records: { entity: "benh_nhan", rows: [{ ten: "An" }] } };
const link = (id: string) => `/studio/${id}`;
function card(asSample: boolean, delivered = true) {
  const onApply = vi.fn();
  render(<ProposalCard item={RECORDS} delivered={delivered} asSample={asSample} onAsSampleChange={vi.fn()} onApply={onApply} onDiscard={vi.fn()} link={link} />);
  return onApply;
}

describe("ProposalCard records — live confirm", () => {
  it("toggle on: applies at once on a delivered project", () => {
    const onApply = card(true);
    expect(screen.getByRole("checkbox")).toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: "Áp dụng" }));
    expect(onApply).toHaveBeenCalledTimes(1);
  });
  it("toggle off: asks first, applies only after confirm", () => {
    const onApply = card(false);
    fireEvent.click(screen.getByRole("button", { name: "Áp dụng" }));
    expect(onApply).not.toHaveBeenCalled();
    expect(screen.getByText(/dữ liệu thật của khách/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Ghi vào dữ liệu thật" }));
    expect(onApply).toHaveBeenCalledTimes(1);
  });
  it("toggle off on an undelivered project: no confirm", () => {
    const onApply = card(false, false);
    fireEvent.click(screen.getByRole("button", { name: "Áp dụng" }));
    expect(onApply).toHaveBeenCalledTimes(1);
  });
});
