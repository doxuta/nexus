import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import type { ApiRecord, FieldMeta } from "@/lib/api";
import { draftFromSections } from "@/lib/layoutDraft";
import type { ScreenOverlay } from "@/lib/screenProposalOverlay";
import type { ProposedChange } from "@/components/studio/assistant/ProposalOverlay";
import { FormCanvas, type FormCanvasProps } from "./FormCanvas";

const F = (name: string, label: string): FieldMeta => ({ name, label, fieldType: "text", isRequired: false }) as FieldMeta;
const fields = [F("a", "Tên"), F("b", "Email"), F("c", "Chức danh")];
const draft = () => draftFromSections([
  { title: "Thông tin chung", columns: 2, fields: ["a", "b"] },
  { title: "Khác", columns: 2, fields: ["c"] },
]);
const change: ProposedChange = { proposalKey: "k1", docType: "layout", op: "update", label: "Sửa form", target: { docType: "layout", entity: "x" } };
const ov = (o: Partial<ScreenOverlay>): ScreenOverlay => ({ change, adds: [], removes: [], relabels: [], ...o });

function setup(overlay: ScreenOverlay | null) {
  const props: FormCanvasProps = {
    tableLabel: "Lịch hẹn", fields, draft: draft(), record: { id: "r", version: 1, data: {} } as ApiRecord, appName: "Phòng khám",
    selected: null, onSelect: vi.fn(), onDeselect: vi.fn(), onAddSection: vi.fn(), overlay,
  };
  return render(<FormCanvas {...props} />);
}

describe("FormCanvas proposed overlay (specs/075 T211)", () => {
  it("no overlay: no ghost at all", () => {
    const { container } = setup(null);
    expect(container.querySelector(".nx-proposed")).toBeNull();
  });
  it("a new field is a purple ghost with the word Đề xuất at the end of the last section, with no control inside", () => {
    const { container } = setup(ov({ adds: [{ section: -1, label: "Ngày tái khám", field: "ngay" }] }));
    const ghost = screen.getByRole("group", { name: "Đề xuất của trợ lý: Ngày tái khám" });
    expect(ghost).toHaveClass("nx-proposed");
    expect(within(ghost).getByText("Đề xuất")).toBeInTheDocument();
    expect(within(ghost).queryAllByRole("button")).toHaveLength(0);
    expect(ghost.getAttribute("data-proposal-key")).toBe("k1");
    // last section = "Khác": the ghost sits after its only field, before nothing else
    const khac = screen.getByRole("button", { name: /Trường Chức danh/ });
    expect(khac.compareDocumentPosition(ghost) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(container.querySelectorAll("[data-proposal-key]")).toHaveLength(1);
    expect(screen.queryByRole("button", { name: /Áp dụng|Bỏ$/ })).toBeNull();
  });
  it("a cell the proposal removes is marked Sẽ bỏ; a relabel shows Trước: …", () => {
    setup(ov({ removes: ["b"], relabels: [{ field: "a", before: "Tên", after: "Họ tên" }] }));
        expect(screen.getByText("Trước: Tên")).toBeInTheDocument();
    expect(screen.getByRole("group", { name: "Đề xuất của trợ lý: Họ tên" })).toBeInTheDocument();
    expect(screen.getByRole("group", { name: "Đề xuất của trợ lý: Sẽ bỏ khỏi màn: Email" })).toBeInTheDocument();
  });
});
