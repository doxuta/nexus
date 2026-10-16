import { describe, expect, it } from "vitest";
import type { ProposedChange } from "@/components/studio/assistant/ProposalOverlay";
import { screenOverlayFrom } from "./screenProposalOverlay";

const labelOf = (n: string) => ({ ten: "Tên", ngay: "Ngày tái khám" })[n] ?? n;
const lay = (sections: unknown[], viewType = "edit") => ({ entityName: "lich_hen", viewType, layout: { version: 1, sections } });
const mk = (over: Partial<ProposedChange>): ProposedChange => ({
  proposalKey: "k", docType: "layout", op: "update", label: "x", target: { docType: "layout", entity: "lich_hen", view: "edit" }, ...over,
});
const f = (op: ProposedChange["op"], over: Partial<ProposedChange>) =>
  mk({ docType: "field", op, target: { docType: "field", entity: "lich_hen", field: "ngay" }, ...over });

describe("screenOverlayFrom", () => {
  it("layout update: a field only in After becomes a ghost in After's section; one only in Before is 'will remove'", () => {
    const o = screenOverlayFrom(mk({
      before: lay([{ title: "A", fields: ["ten", "bo"] }, { title: "B", fields: [] }]),
      after: lay([{ title: "A", fields: ["ten"] }, { title: "B", fields: ["ngay"] }]),
    }), "edit", labelOf);
    expect(o?.adds).toEqual([{ section: 1, label: "Ngày tái khám", field: "ngay" }]);
    expect(o?.removes).toEqual(["bo"]);
  });
  it("layout update: label override change gives Before/After; labelOverride beats the field label", () => {
    const o = screenOverlayFrom(mk({
      before: lay([{ fields: ["ten", { field: "ngay", labelOverride: "Hẹn lại" }] }]),
      after: lay([{ fields: [{ field: "ten", labelOverride: "Họ tên" }, "ngay"] }]),
    }), "edit", labelOf);
    expect(o?.relabels).toEqual([{ field: "ten", before: "Tên", after: "Họ tên" }, { field: "ngay", before: "Hẹn lại", after: "Ngày tái khám" }]);
  });
  it("layout of another screen kind is not drawn here", () => {
    expect(screenOverlayFrom(mk({ before: lay([]), after: lay([], "detail"), target: { docType: "layout", view: "detail" } }), "edit", labelOf)).toBeNull();
  });
  it("field create: ghost at the end of the last section; update: Before/After label; delete: will remove", () => {
    expect(screenOverlayFrom(f("create", { after: { label: "Ngày tái khám" } }), "edit", labelOf)?.adds).toEqual([{ section: -1, label: "Ngày tái khám", field: "ngay" }]);
    expect(screenOverlayFrom(f("update", { before: { label: "Ngày" }, after: { label: "Ngày tái khám" } }), "edit", labelOf)?.relabels)
      .toEqual([{ field: "ngay", before: "Ngày", after: "Ngày tái khám" }]);
    expect(screenOverlayFrom(f("delete", { before: { label: "Ngày" } }), "edit", labelOf)?.removes).toEqual(["ngay"]);
  });
  it("other docType, or a change with no usable data, falls back to null (ChangeList)", () => {
    expect(screenOverlayFrom(mk({ docType: "workflow" }), "edit", labelOf)).toBeNull();
    expect(screenOverlayFrom(mk({ after: "junk" }), "edit", labelOf)).toBeNull();
    expect(screenOverlayFrom(f("create", {}), "edit", labelOf)).toBeNull();
  });
});
