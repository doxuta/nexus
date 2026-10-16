import { describe, expect, it } from "vitest";
import { keptReason, recordHref, sampleSourceText, shortRecord } from "./sampleLabels";

describe("sampleSourceText", () => {
  it("labels and orders the sources", () => {
    expect(sampleSourceText({ pack: 2, generator: 5, ai: 3 })).toBe("Trợ lý 3 · Tự sinh 5 · Gói 2");
  });
  it("drops zero and missing sources", () => {
    expect(sampleSourceText({ ai: 0, generator: 4 })).toBe("Tự sinh 4");
    expect(sampleSourceText({})).toBe("");
    expect(sampleSourceText(undefined)).toBe("");
  });
});

// specs/077 f5-sample-wipe T122: kept-row labels and the record link.
const label = (e: string) => ({ appointments: "Lịch hẹn" })[e] ?? e;

describe("sampleLabels", () => {
  it("recordHref is a same-tab record path, ids escaped", () => {
    expect(recordHref("customers", "a/b")).toBe("/customers/a%2Fb");
  });
  it("shortRecord keeps the last 6 characters of the id", () => {
    expect(shortRecord("Khách hàng", "aaaa-123456")).toBe("Khách hàng · …123456");
  });
  it("field_default names table › field from blockedBy; busy and too_large need none", () => {
    const b = [{ entity: "appointments", field: "customer", count: 1 }];
    expect(keptReason({ entity: "x", recordId: "1", reason: "field_default", blockedBy: b }, label)).toContain("Lịch hẹn › customer");
    expect(keptReason({ entity: "x", recordId: "1", reason: "busy", blockedBy: [] }, label)).toContain("bấm xoá lại");
    expect(keptReason({ entity: "x", recordId: "1", reason: "too_large", blockedBy: [] }, label)).toContain("Quá nhiều");
  });
});
