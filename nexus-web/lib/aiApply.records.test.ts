import { describe, it, expect } from "vitest";
import { QueryClient } from "@tanstack/react-query";
import { needsLiveConfirm } from "./aiApply";
import { invalidateAfterApply } from "./aiInvalidate";
import { errorMessage } from "./errorMessages";
import { ApiError } from "./api";
import type { AiDraftItem } from "./aiBuilder";

const RECORDS: AiDraftItem = { kind: "records", records: { entity: "benh_nhan", rows: [{ ten: "An" }] } };

describe("ai10 records live confirm", () => {
  it("toggle off + delivered ⇒ needs confirm", () => expect(needsLiveConfirm(RECORDS, true, false)).toBe(true));
  it("toggle on + delivered ⇒ no confirm", () => expect(needsLiveConfirm(RECORDS, true, true)).toBe(false));
  it("toggle off + not delivered ⇒ no confirm", () => expect(needsLiveConfirm(RECORDS, false, false)).toBe(false));
  it("other live kinds unchanged by the toggle", () =>
    expect(needsLiveConfirm({ kind: "app", app: { name: "a", label: "A", navEntities: [] } } as AiDraftItem, true, true)).toBe(true));
  it("SAMPLE_ENTITY_NOT_FOUND shows its own sentence", () =>
    expect(errorMessage(new ApiError(422, "SAMPLE_ENTITY_NOT_FOUND", "x"))).toBe("Không tìm thấy bảng dữ liệu đã chọn."));
  it("apply refreshes the sample-data card keys (FR-005)", () => {
    const qc = new QueryClient();
    qc.setQueryData(["project", "northwind", "studio", "sampleData", "a"], 1);
    qc.setQueryData(["studio", "sampleData", "northwind"], 1);
    invalidateAfterApply(qc);
    expect(qc.getQueryState(["project", "northwind", "studio", "sampleData", "a"])?.isInvalidated).toBe(true);
    expect(qc.getQueryState(["studio", "sampleData", "northwind"])?.isInvalidated).toBe(true);
  });
});
