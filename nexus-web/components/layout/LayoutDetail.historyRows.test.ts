// LayoutDetail — which audit diff keys a record's History tab prints (DAILY-QUEUE #70).
// Approval decisions land in the same audit_log as record edits, so the History tab lists them
// too. Since #70 their diff also carries `definition` — the chain the decision executed, an
// OBJECT that no field of the record describes. HistoryValue prints a field-less value with
// String(), so without a filter the customer-facing record page would show "[object Object]".
import { describe, it, expect } from "vitest";
import { historyDiffRows } from "./LayoutDetail";
import type { FieldMeta } from "@/lib/api";

const FIELDS: Record<string, FieldMeta> = {
  price: { name: "price", label: "Giá", fieldType: "currency" },
  status: { name: "status", label: "Trạng thái", fieldType: "picklist" },
};

describe("historyDiffRows", () => {
  it("drops a field-less object value (an approval row's definition) instead of printing [object Object]", () => {
    const after = {
      approvalInstanceId: "i-1",
      comment: "ok",
      definition: { steps: [{ name: "Trưởng phòng", approverType: "role", approverId: "r-1" }] },
      processId: "p-1",
      step: 0,
    };
    expect(historyDiffRows(after, FIELDS).map(([k]) => k)).toEqual(["approvalInstanceId", "comment", "processId", "step"]);
  });

  it("keeps an object value that belongs to a field (currency) — FieldDisplay renders it", () => {
    const after = { price: { amount: 100, currencyCode: "VND" }, status: "New" };
    expect(historyDiffRows(after, FIELDS)).toEqual([["price", { amount: 100, currencyCode: "VND" }], ["status", "New"]]);
  });

  it("keeps null (a cleared value renders as ∅) and still caps at max", () => {
    const after = { a: null, b: 1, c: 2 };
    expect(historyDiffRows(after, FIELDS, 2)).toEqual([["a", null], ["b", 1]]);
  });
});
