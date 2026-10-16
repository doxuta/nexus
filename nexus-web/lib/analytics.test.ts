// Unit tests for analytics.ts — widget-result → chart-data mapping.
// All value strings are decimal strings as returned by the live backend
// (e.g. "250000.0000000000") per curl verification 2026-07-01.

import { describe, it, expect } from "vitest";
import {
  widgetGroupsToChartData,
  orderChartByCategory,
  aggGroupsToChartData,
  relabelChartData,
  formatWidgetValue,
  categorizeWidgets,
} from "./analytics";
import type { WidgetRun } from "./api";

// ── widgetGroupsToChartData ─────────────────────────────────────────────────

describe("widgetGroupsToChartData", () => {
  it("converts grouped widget groups[] to ChartSeriesPoint[] sorted desc by value", () => {
    const w: WidgetRun = {
      widgetId: "w1",
      title: "Pipeline",
      type: "bar",
      result: {
        grouped: true,
        groups: [
          { key: "Prospecting", value: "40000.0000000000", count: 1 },
          { key: "Negotiation", value: "250000.0000000000", count: 1 },
          { key: "Qualification", value: "15000.0000000000", count: 1 },
          { key: "Proposal", value: "75000.0000000000", count: 1 },
        ],
        count: 4,
      },
    };
    const pts = widgetGroupsToChartData(w);
    expect(pts).toHaveLength(4);
    // Sorted descending
    expect(pts[0]).toEqual({ key: "Negotiation", value: 250000, count: 1 });
    expect(pts[1]).toEqual({ key: "Proposal", value: 75000, count: 1 });
    expect(pts[2]).toEqual({ key: "Prospecting", value: 40000, count: 1 });
    expect(pts[3]).toEqual({ key: "Qualification", value: 15000, count: 1 });
  });

  it("returns [] for scalar (non-grouped) widget", () => {
    const w: WidgetRun = {
      widgetId: "w2",
      title: "Tổng giá trị",
      type: "kpi",
      result: { grouped: false, value: "380000.0000000000", count: 4 },
    };
    expect(widgetGroupsToChartData(w)).toEqual([]);
  });

  it("returns [] for errored widget (no result)", () => {
    const w: WidgetRun = {
      widgetId: "w1",
      title: "Lỗi",
      type: "bar",
      result: null,
      error: "không thể group theo field",
      code: "AGGREGATE_REQUIRES_INDEXED",
    };
    expect(widgetGroupsToChartData(w)).toEqual([]);
  });

  it("returns [] for widget with empty groups", () => {
    const w: WidgetRun = {
      widgetId: "w1",
      title: "Empty",
      type: "bar",
      result: { grouped: true, groups: [], count: 0 },
    };
    expect(widgetGroupsToChartData(w)).toEqual([]);
  });

  it("maps empty-string key to '(trống)'", () => {
    const w: WidgetRun = {
      widgetId: "w1",
      title: "Nhóm rỗng",
      type: "bar",
      result: {
        grouped: true,
        groups: [{ key: "", value: "1000.0000000000", count: 2 }],
        count: 2,
      },
    };
    const pts = widgetGroupsToChartData(w);
    expect(pts[0].key).toBe("(trống)");
    expect(pts[0].value).toBe(1000);
    expect(pts[0].count).toBe(2);
  });
});

// ── aggGroupsToChartData ────────────────────────────────────────────────────

describe("aggGroupsToChartData", () => {
  it("converts aggregate groups to ChartSeriesPoint[] sorted desc by value", () => {
    const groups = [
      { key: "New", value: "5.0000000000", count: 5 },
      { key: "Qualified", value: "12.0000000000", count: 12 },
      { key: "Working", value: "8.0000000000", count: 8 },
    ];
    const pts = aggGroupsToChartData(groups);
    expect(pts[0].key).toBe("Qualified");
    expect(pts[0].value).toBe(12);
    expect(pts[1].key).toBe("Working");
    expect(pts[2].key).toBe("New");
  });

  it("returns [] for empty groups", () => {
    expect(aggGroupsToChartData([])).toEqual([]);
  });
});

// ── formatWidgetValue ────────────────────────────────────────────────────────

describe("formatWidgetValue", () => {
  it("formats count metric as integer locale string", () => {
    expect(formatWidgetValue("4.0000000000", "count")).toBe("4");
  });

  // DASH-CURRENCY: a sum over a NON-currency field (empty currencyCode) is a plain number,
  // NEVER a bogus VNĐ tacked on.
  it("formats sum WITHOUT a currencyCode as a plain number (no VNĐ)", () => {
    const result = formatWidgetValue("380000.0000000000", "sum");
    expect(result).not.toContain("VNĐ");
    expect(result).not.toMatch(/US\$|\$/);
    expect(result.replace(/\D/g, "")).toBe("380000");
  });

  it("formats a VND-currency sum as compact VNĐ", () => {
    expect(formatWidgetValue("380000.0000000000", "sum", "VND")).toContain("VNĐ");
    expect(formatWidgetValue("3500000000", "sum", "VND")).toContain("tỷ VNĐ");
  });

  it("formats a USD-currency sum in its own currency, not VNĐ", () => {
    const result = formatWidgetValue("500000", "sum", "USD");
    expect(result).not.toContain("VNĐ");
    expect(result).toMatch(/US\$|\$/);
  });

  it("handles undefined raw value as 0 (plain number, no currency)", () => {
    expect(formatWidgetValue(undefined, "sum")).toBe("0");
  });

  it("undefined metric without currency → plain number, not VNĐ", () => {
    const result = formatWidgetValue("5000.0000000000", undefined);
    expect(result).not.toContain("VNĐ");
  });
});

// ── orderChartByCategory (DASH-FUNNEL-ORDER) ─────────────────────────────────

describe("orderChartByCategory", () => {
  const p = (key: string, value: number) => ({ key, value, count: value });

  it("reorders points to the picklist's declared stage order, not value-desc", () => {
    // value-desc input (as the BE returns it): Negotiation biggest, Qualification smallest.
    const pts = [p("Negotiation", 250), p("Closed Won", 120), p("Prospecting", 40), p("Qualification", 15)];
    const order = ["Prospecting", "Qualification", "Proposal", "Negotiation", "Closed Won"];
    expect(orderChartByCategory(pts, order).map((x) => x.key)).toEqual([
      "Prospecting", "Qualification", "Negotiation", "Closed Won",
    ]);
  });

  it("keeps unknown keys (legacy stage / null bucket) after the declared ones, in incoming order", () => {
    const pts = [p("Negotiation", 250), p("(trống)", 5), p("Legacy", 3), p("Prospecting", 40)];
    const order = ["Prospecting", "Negotiation"];
    expect(orderChartByCategory(pts, order).map((x) => x.key)).toEqual([
      "Prospecting", "Negotiation", "(trống)", "Legacy",
    ]);
  });

  it("empty order → returns points unchanged", () => {
    const pts = [p("B", 2), p("A", 1)];
    expect(orderChartByCategory(pts, []).map((x) => x.key)).toEqual(["B", "A"]);
  });
});

// ── categorizeWidgets ────────────────────────────────────────────────────────

describe("categorizeWidgets", () => {
  const defs = new Map();

  it("separates kpi, bar, and error widgets", () => {
    const widgets: WidgetRun[] = [
      { widgetId: "w1", title: "Bar", type: "bar", result: { grouped: true, groups: [{ key: "A", value: "1", count: 1 }], count: 1 } },
      { widgetId: "w2", title: "KPI", type: "kpi", result: { grouped: false, value: "100", count: 1 } },
      { widgetId: "w3", title: "Error", type: "bar", result: null, error: "fail", code: "ERR" },
    ];
    const { kpis, bars, errors } = categorizeWidgets(widgets, defs);
    expect(kpis).toHaveLength(1);
    expect(kpis[0].widgetId).toBe("w2");
    expect(bars).toHaveLength(1);
    expect(bars[0].widgetId).toBe("w1");
    expect(errors).toHaveLength(1);
    expect(errors[0].widgetId).toBe("w3");
  });

  it("puts widget with result=null in errors", () => {
    const w: WidgetRun = { widgetId: "w1", title: "Bad", type: "kpi", result: null };
    const { errors } = categorizeWidgets([w], defs);
    expect(errors).toHaveLength(1);
  });
});

// ── relabelChartData (REPORT-LABELS, WAVE 6) ────────────────────────────────
// Biểu đồ báo cáo/dashboard hiện mã thô ("Proposal", "Closed Lost") trong khi màn chi tiết cùng
// field đã nói "Báo giá", "Thua" — bề mặt thứ 6 của tầng nhãn. Chart dùng `key` VỪA làm chữ trên
// trục VỪA làm giá trị drill, nên đổi nhãn tay không là drill lọc theo tiếng Việt trong khi record
// lưu mã — helper này tách hai vai đó.
describe("relabelChartData", () => {
  const pts = [
    { key: "Proposal", value: 5, count: 1 },
    { key: "Closed Lost", value: 2, count: 1 },
    { key: "Mystery", value: 1, count: 1 },
  ];
  const labels = { Proposal: "Báo giá", "Closed Lost": "Thua" };

  it("hiển thị nhãn, codeOf trả về đúng mã để drill", () => {
    const { pts: out, codeOf } = relabelChartData(pts, labels);
    expect(out.map((p) => p.key)).toEqual(["Báo giá", "Thua", "Mystery"]);
    expect(codeOf("Báo giá")).toBe("Proposal");
    expect(codeOf("Mystery")).toBe("Mystery"); // mã không nhãn: hiện nguyên, drill nguyên
  });

  it("không có nhãn → identity (tenant chưa có valueLabels không đổi gì)", () => {
    const { pts: out, codeOf } = relabelChartData(pts, undefined);
    expect(out).toBe(pts);
    expect(codeOf("Proposal")).toBe("Proposal");
  });

  it("hai mã trùng một nhãn: mã thứ hai giữ nguyên mã — trung thực hơn là drill sai chỗ", () => {
    const twins = [{ key: "A", value: 2, count: 1 }, { key: "B", value: 1, count: 1 }];
    const { pts: out, codeOf } = relabelChartData(twins, { A: "Chung", B: "Chung" });
    expect(out.map((p) => p.key)).toEqual(["Chung", "B"]);
    expect(codeOf("Chung")).toBe("A");
    expect(codeOf("B")).toBe("B");
  });

  // specs/049 F2 T091a: the server's `label` (relation/user names) outranks valueLabels, which outrank the key.
  it("label ?? valueLabels[key] ?? key, drill still returns the key", () => {
    const withLabels = aggGroupsToChartData([
      { key: "uuid-1", label: "Công ty An Phát", value: "30", count: 3 },
      { key: "Proposal", value: "20", count: 2 },
      { key: "Other", value: "10", count: 1 },
    ]);
    const { pts: out, codeOf } = relabelChartData(withLabels, { Proposal: "Báo giá" });
    expect(out.map((p) => p.key)).toEqual(["Công ty An Phát", "Báo giá", "Other"]);
    expect(codeOf("Công ty An Phát")).toBe("uuid-1");
    expect(codeOf("Báo giá")).toBe("Proposal");
  });

  it("server labels apply without any valueLabels map; no labels at all stays identity", () => {
    const withLabel = aggGroupsToChartData([{ key: "u1", label: "Trần Sales Rep", value: "5", count: 1 }]);
    const r = relabelChartData(withLabel, undefined);
    expect(r.pts[0].key).toBe("Trần Sales Rep");
    expect(r.codeOf("Trần Sales Rep")).toBe("u1");
    const plain = aggGroupsToChartData([{ key: "x", value: "5", count: 1 }]);
    expect(relabelChartData(plain, undefined).pts).toBe(plain);
  });
});
