// specs/076 f4-proposal-overlay-hook T144/T144a: the "Xem trước" button, its status lines and the honest fallback.
import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";

const push = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }), usePathname: () => "/studio/du-an/demo/dung/du-lieu" }));

import { ProposalCard } from "./ProposalCard";
import { ProposalOverlayProvider } from "./ProposalOverlay";
import type { AiDraftItem } from "@/lib/aiBuilder";

const FIELD: AiDraftItem = {
  kind: "field",
  field: { entity: "lich_hen", name: "ngay", label: "Ngày tái khám", fieldType: "date" },
  change: { docType: "field", op: "create", label: "Ngày tái khám", target: { entity: "lich_hen", field: "ngay" } },
};
const ENTITY: AiDraftItem = {
  kind: "entity",
  entity: { name: "bn", label: "Bệnh nhân", appName: "Phòng khám", fields: [{ name: "ten", label: "Tên", fieldType: "text" }] },
  change: { docType: "entity", op: "create", label: "Bệnh nhân", target: { entity: "bn" } },
};
const link = (id: string) => `/studio/${id}`;
const mount = (item: AiDraftItem, painted: { docType: string; op: "create" | "update" | "delete" }[], state?: { status: "done" }) =>
  render(
    <ProposalOverlayProvider slug="demo" painted={painted}>
      <ProposalCard item={item} state={state} asSample onAsSampleChange={() => {}} onApply={() => {}} onDiscard={() => {}} link={link} proposalKey="t:0" />
    </ProposalOverlayProvider>,
  );

beforeEach(() => push.mockClear());

describe("ProposalCard preview", () => {
  it("no button while PAINTED has no row for the kind (honest fallback)", () => {
    mount(FIELD, []);
    expect(screen.queryByRole("button", { name: /xem trước/i })).toBeNull();
    expect(screen.getByRole("button", { name: "Áp dụng" })).toBeInTheDocument();
  });

  it("button shows when painted; click navigates with scroll:false and writes the status lines", () => {
    mount(FIELD, [{ docType: "field", op: "create" }]);
    const b = screen.getByRole("button", { name: "Xem trước" });
    expect(b).toHaveAttribute("aria-pressed", "false");
    fireEvent.click(b);
    expect(push).toHaveBeenCalledWith("/studio/du-an/demo/dung/du-lieu/lich_hen?truong=ngay", { scroll: false });
    const off = screen.getByRole("button", { name: "Tắt xem trước" });
    expect(off).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByText("Đang tô sáng trên Bảng.")).toBeInTheDocument();
    expect(screen.getByText("Đã mở Dựng › Dữ liệu để anh xem trước.")).toBeInTheDocument();
    expect(screen.getByRole("article")).toHaveClass("nx-proposed");
    fireEvent.keyDown(off, { key: "Escape" });
    expect(screen.getByRole("button", { name: "Tắt xem trước" })).toBeInTheDocument();
    fireEvent.click(off);
    expect(screen.queryByText(/Đang tô sáng/)).toBeNull();
    expect(screen.getByRole("article")).not.toHaveClass("nx-proposed");
  });

  it("a new table with appName says which menu it will join", () => {
    mount(ENTITY, []);
    expect(screen.getByText("sẽ vào menu Phòng khám")).toBeInTheDocument();
  });

  it("Bỏ turns the preview off", () => {
    mount(FIELD, [{ docType: "field", op: "create" }]);
    fireEvent.click(screen.getByRole("button", { name: "Xem trước" }));
    fireEvent.click(screen.getByRole("button", { name: "Bỏ" }));
    expect(screen.queryByText(/Đang tô sáng/)).toBeNull();
  });
});
