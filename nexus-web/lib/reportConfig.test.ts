// Tests for lib/reportConfig — the config payload report authoring saves
// (/reports/[id] admin ReportConfigPanel + the /reports create dialog).
// BE contract (report_service.go validateReportConfig + record_aggregate.go
// isBucketableGroupBy): bucket is summary/matrix-only and rides only on a date-kind groupBy.
import { describe, expect, it } from "vitest";
import { buildReportConfig, isGridConfig, reportShapeNote } from "./reportConfig";
import type { ReportRunData } from "./api";
import { isBucketableGroupBy } from "./timeBuckets";

const FIELDS = [
  { name: "closed_on", fieldType: "date" },
  { name: "signed_at", fieldType: "datetime" },
  { name: "stage", fieldType: "picklist" },
  { name: "amount", fieldType: "currency" },
];

describe("isBucketableGroupBy", () => {
  it("accepts the system timestamps created_at/updated_at", () => {
    expect(isBucketableGroupBy("created_at", [])).toBe(true);
    expect(isBucketableGroupBy("updated_at", FIELDS)).toBe(true);
  });
  it("accepts declared date and datetime fields", () => {
    expect(isBucketableGroupBy("closed_on", FIELDS)).toBe(true);
    expect(isBucketableGroupBy("signed_at", FIELDS)).toBe(true);
  });
  it("rejects non-date fields, unknown fields, id, and empty groupBy", () => {
    expect(isBucketableGroupBy("stage", FIELDS)).toBe(false);
    expect(isBucketableGroupBy("amount", FIELDS)).toBe(false);
    expect(isBucketableGroupBy("nope", FIELDS)).toBe(false);
    expect(isBucketableGroupBy("id", FIELDS)).toBe(false);
    expect(isBucketableGroupBy("", FIELDS)).toBe(false);
    expect(isBucketableGroupBy(undefined, FIELDS)).toBe(false);
  });
});

describe("buildReportConfig", () => {
  it("tabular: columns only — NEVER a bucket (BE 422 REPORT_CONFIG_INVALID on tabular bucket)", () => {
    const cfg = buildReportConfig({
      type: "tabular", columns: ["stage", "amount"], metric: "count",
      groupBy: "closed_on", bucket: "month", fields: FIELDS,
    });
    expect(cfg).toEqual({ columns: ["stage", "amount"] });
    expect("bucket" in cfg).toBe(false);
  });

  it("tabular: empty column list falls back to fallbackColumn", () => {
    const cfg = buildReportConfig({ type: "tabular", columns: [], metric: "count", fields: FIELDS, fallbackColumn: "stage" });
    expect(cfg).toEqual({ columns: ["stage"] });
  });

  it("summary: writes config.bucket for a date groupBy", () => {
    const cfg = buildReportConfig({
      type: "summary", columns: [], metric: "sum", field: "amount",
      groupBy: "closed_on", bucket: "month", fields: FIELDS,
    });
    expect(cfg).toEqual({ metric: "sum", field: "amount", groupBy: "closed_on", bucket: "month" });
  });

  it("matrix: writes config.bucket for created_at (system timestamp) groupBy", () => {
    const cfg = buildReportConfig({
      type: "matrix", columns: [], metric: "count", groupBy: "created_at", bucket: "week", fields: FIELDS,
    });
    expect(cfg.bucket).toBe("week");
  });

  it("summary: DROPS the bucket when groupBy is not date-kind (stale bucket after re-point)", () => {
    const cfg = buildReportConfig({
      type: "summary", columns: [], metric: "count", groupBy: "stage", bucket: "month", fields: FIELDS,
    });
    expect("bucket" in cfg).toBe(false);
  });

  it("summary: no bucket without a groupBy, with an empty bucket, or with a bad token", () => {
    const noGroup = buildReportConfig({ type: "summary", columns: [], metric: "count", bucket: "month", fields: FIELDS });
    expect("bucket" in noGroup).toBe(false);
    const empty = buildReportConfig({ type: "summary", columns: [], metric: "count", groupBy: "closed_on", bucket: "", fields: FIELDS });
    expect("bucket" in empty).toBe(false);
    const bad = buildReportConfig({ type: "summary", columns: [], metric: "count", groupBy: "closed_on", bucket: "decade", fields: FIELDS });
    expect("bucket" in bad).toBe(false);
  });
});

// ── specs/049 F0 — the config panel saves by FULL replace (PUT), so every key it has no editor
// for must ride along from the saved config, or a rename silently widens the report. ──────────
describe("buildReportConfig — saved keys the editor does not manage (F0 passthrough)", () => {
  const FILTERS = [{ field: "stage", op: "in", values: ["Proposal", "Negotiation"] }];
  const SORT = { field: "created_at", dir: "asc" };
  const SUB = [{ field: "close_date", bucket: "month" }];
  const COL = { field: "close_date", bucket: "month" };
  const HAVING = { on: "value", op: "gt", values: ["10000"] };
  const SAVED = { filters: FILTERS, sort: SORT, subGroups: SUB, columnGroup: COL, having: HAVING, joins: [{ relationship: "r" }] };
  const base = {
    columns: ["name"], metric: "sum", field: "amount", groupBy: "stage", fields: FIELDS,
    entityName: "crm_opportunities", savedEntity: "crm_opportunities", joinRel: "",
  };

  it("tabular keeps filters + sort (and nothing that only groups/thresholds use)", () => {
    const cfg = buildReportConfig({ ...base, type: "tabular", saved: SAVED });
    expect(cfg).toEqual({ columns: ["name"], filters: FILTERS, sort: SORT });
  });

  it("summary keeps filters + subGroups + having — never sort or columnGroup", () => {
    const cfg = buildReportConfig({ ...base, type: "summary", saved: SAVED });
    expect(cfg).toEqual({ metric: "sum", field: "amount", groupBy: "stage", filters: FILTERS, subGroups: SUB, having: HAVING });
  });

  it("matrix keeps filters + columnGroup + having — never sort or subGroups", () => {
    const cfg = buildReportConfig({ ...base, type: "matrix", saved: SAVED });
    expect(cfg).toEqual({ metric: "sum", field: "amount", groupBy: "stage", filters: FILTERS, columnGroup: COL, having: HAVING });
  });

  it("type change drops what the new type cannot hold: matrix→summary loses columnGroup, keeps having + filters", () => {
    const saved = { filters: FILTERS, columnGroup: COL, having: HAVING };
    const cfg = buildReportConfig({ ...base, type: "summary", saved });
    expect(cfg).not.toHaveProperty("columnGroup");
    expect(cfg).toMatchObject({ filters: FILTERS, having: HAVING });
    const tab = buildReportConfig({ ...base, type: "tabular", saved });
    expect(tab).toEqual({ columns: ["name"], filters: FILTERS });
  });

  it("joins are never carried — withReportJoin owns them", () => {
    const cfg = buildReportConfig({ ...base, type: "summary", saved: SAVED });
    expect(cfg).not.toHaveProperty("joins");
  });

  it("no saved config (the /reports create dialog) builds exactly what it did before", () => {
    const plain = { columns: ["name"], metric: "sum", field: "amount", groupBy: "stage", fields: FIELDS };
    expect(buildReportConfig({ ...plain, type: "tabular" })).toEqual({ columns: ["name"] });
    expect(buildReportConfig({ ...plain, type: "summary" })).toEqual({ metric: "sum", field: "amount", groupBy: "stage" });
  });

  it("a different entity carries NOTHING — every carried key names a field of the old entity", () => {
    for (const type of ["tabular", "summary", "matrix"] as const) {
      const cfg = buildReportConfig({ ...base, type, saved: SAVED, entityName: "crm_accounts" });
      for (const k of ["filters", "sort", "subGroups", "columnGroup", "having", "joins"]) expect(cfg).not.toHaveProperty(k);
    }
  });

  it("a join change drops carried axes namespaced onto another relationship; base axes survive", () => {
    const saved = {
      subGroups: [{ field: "opp_products.category" }, { field: "close_date", bucket: "month" }],
      columnGroup: { field: "opp_products.category" },
      having: HAVING,
    };
    // Same relationship still selected → joined axes stay.
    const same = buildReportConfig({ ...base, type: "summary", saved, joinRel: "opp_products" });
    expect(same.subGroups).toEqual(saved.subGroups);
    // Another relationship → the stale joined level is dropped, the base level kept.
    const other = buildReportConfig({ ...base, type: "summary", saved, joinRel: "opp_contacts" });
    expect(other.subGroups).toEqual([{ field: "close_date", bucket: "month" }]);
    expect(other.having).toEqual(HAVING);
    // No join at all → a namespaced column axis cannot run; it is dropped, not saved-then-422.
    const none = buildReportConfig({ ...base, type: "matrix", saved, joinRel: "" });
    expect(none).not.toHaveProperty("columnGroup");
    // Every level stale → the key goes, not an empty list.
    const allStale = buildReportConfig({ ...base, type: "summary", saved: { subGroups: [{ field: "x.y" }] }, joinRel: "" });
    expect(allStale).not.toHaveProperty("subGroups");
  });
});

// specs/070 #266: the panel now EDITS filters (every type) and sort (tabular) — the value it passes
// replaces the saved one (an emptied editor removes the key); leaving them undefined keeps F0's
// passthrough so the create dialog and every other caller build exactly what they did.
describe("buildReportConfig — filters and sort managed by the editor", () => {
  const SAVED = {
    filters: [{ field: "stage", op: "eq", values: ["Proposal"] }],
    sort: { field: "created_at", dir: "asc" },
    having: { on: "value", op: "gt", values: ["1"] },
  };
  const base = {
    columns: ["name"], metric: "count", groupBy: "stage", fields: FIELDS,
    entityName: "e", savedEntity: "e", saved: SAVED,
  };
  const OVERDUE = [
    { field: "closed_on", op: "lt", values: ["today"] },
    { field: "stage", op: "neq", values: ["Done"] },
  ];

  it("the edited filters and sort replace the saved ones", () => {
    expect(buildReportConfig({ ...base, type: "tabular", filters: OVERDUE, sort: { field: "closed_on", dir: "desc" } }))
      .toEqual({ columns: ["name"], filters: OVERDUE, sort: { field: "closed_on", dir: "desc" } });
    expect(buildReportConfig({ ...base, type: "summary", filters: OVERDUE, sort: null }))
      .toEqual({ metric: "count", groupBy: "stage", filters: OVERDUE, having: SAVED.having });
  });

  it("an emptied editor removes the key instead of keeping the saved value", () => {
    expect(buildReportConfig({ ...base, type: "tabular", filters: [], sort: null })).toEqual({ columns: ["name"] });
  });

  it("sort is tabular-only — the aggregate path never reads it", () => {
    expect(buildReportConfig({ ...base, type: "summary", filters: [], sort: { field: "closed_on", dir: "asc" } }))
      .not.toHaveProperty("sort");
  });
});

describe("isGridConfig — the config leaves the one-axis path", () => {
  it("columnGroup / non-empty subGroups / having ⇒ grid; a plain one-axis config ⇒ not", () => {
    expect(isGridConfig({ metric: "count", groupBy: "stage", columnGroup: { field: "close_date" } })).toBe(true);
    expect(isGridConfig({ metric: "count", groupBy: "stage", subGroups: [{ field: "close_date" }] })).toBe(true);
    expect(isGridConfig({ metric: "count", groupBy: "stage", having: { on: "count", op: "gt", values: ["1"] } })).toBe(true);
    expect(isGridConfig({ metric: "count", groupBy: "stage", subGroups: [] })).toBe(false);
    expect(isGridConfig({ metric: "count", groupBy: "stage", filters: [{ field: "stage" }] })).toBe(false);
    expect(isGridConfig(undefined)).toBe(false);
  });
});

describe("reportShapeNote — the viewer says what it is (not) drawing", () => {
  const label = (n: string) => ({ stage: "Giai đoạn", close_date: "Ngày chốt" } as Record<string, string>)[n];
  const flat: ReportRunData = { type: "summary", groups: [{ key: "Proposal", value: "3", count: 3 }] };
  const grid: ReportRunData = {
    type: "matrix", groups: [], rowAxis: { field: "stage" }, columnAxis: { field: "close_date", bucket: "month" },
    rows: [], columns: [], cells: [], total: { value: "0", count: 0 }, having: null,
  };
  const tree2: ReportRunData = {
    type: "summary", levels: [{ field: "stage" }, { field: "close_date", bucket: "month" }],
    groups: [{ key: "Proposal", value: "3", count: 3, children: [] }], total: { value: "3", count: 3 },
  };

  it("one-axis summary: nothing to say", () => {
    expect(reportShapeNote(flat, "summary", true, label)).toEqual([]);
  });

  it("grid: drawn as a cross-tab (F1) — no \"not drawn yet\" note", () => {
    expect(reportShapeNote(grid, "matrix", false, label)).toEqual([]);
  });

  it("tree: drawn as a tree grid (F2) — no \"first level only\" note at any depth", () => {
    expect(reportShapeNote(tree2, "summary", false, label)).toEqual([]);
    const tree1 = { ...tree2, levels: [{ field: "stage" }] } as ReportRunData;
    expect(reportShapeNote(tree1, "summary", false, label)).toEqual([]);
  });

  it("a matrix report without a column axis: told to the admin, nothing for staff", () => {
    expect(reportShapeNote(flat, "matrix", true, label)).toEqual([
      "Báo cáo Ma trận này chưa có trục cột — đang chạy như Tóm tắt (một trục nhóm).",
    ]);
    expect(reportShapeNote(flat, "matrix", false, label)).toEqual([]);
  });

  it("having on a grid filters CELLS", () => {
    const g = { ...grid, having: { on: "value", op: "gt", values: ["10000"] } } as ReportRunData;
    expect(reportShapeNote(g, "matrix", false, label)).toContain("Chỉ giữ ô có giá trị > 10.000.");
  });

  it("having on a tree filters groups of the DEEPEST level, named by its label — N=1 included", () => {
    const t2 = { ...tree2, having: { on: "count", op: "between", values: ["2", "1500.5"] } } as ReportRunData;
    expect(reportShapeNote(t2, "summary", false, label)).toContain("Chỉ giữ nhóm Ngày chốt có số lượng từ 2 đến 1.500,5.");
    const t1 = { ...tree2, levels: [{ field: "stage" }], having: { on: "value", op: "lte", values: ["-5"] } } as ReportRunData;
    expect(reportShapeNote(t1, "summary", false, label)).toEqual(["Chỉ giữ nhóm Giai đoạn có giá trị ≤ -5."]);
  });

  it("an axis without a readable label never leaks the field token", () => {
    const t = { ...tree2, levels: [{ field: "opp_products.category" }], having: { on: "value", op: "gte", values: ["1"] } } as ReportRunData;
    const notes = reportShapeNote(t, "summary", false, label);
    expect(notes).toEqual(["Chỉ giữ nhóm ở cấp sâu nhất có giá trị ≥ 1."]);
    expect(notes.join(" ")).not.toContain("opp_products");
  });
});

// ── specs/049 F3 (T094): the three grid keys are managed by the panel ──
describe("buildReportConfig — managed grid keys (F3)", () => {
  const b = { columns: [], metric: "sum", field: "amount", groupBy: "stage", fields: [...FIELDS] };
  const SAVED = { columnGroup: { field: "closed_on" }, subGroups: [{ field: "signed_at" }], having: { on: "value", op: "gt", values: ["1"] } };
  const same = { saved: SAVED, entityName: "e", savedEntity: "e" };

  it("given values replace the saved ones; null/[] clears them", () => {
    const m = buildReportConfig({ ...b, ...same, type: "matrix", columnGroup: { field: "signed_at", bucket: "month" }, having: null });
    expect(m).toMatchObject({ columnGroup: { field: "signed_at", bucket: "month" } });
    expect(m).not.toHaveProperty("having");
    const s = buildReportConfig({ ...b, ...same, type: "summary", subGroups: [] });
    expect(s).not.toHaveProperty("subGroups");
  });
  it("undefined ⇒ saved ride along", () => {
    const m = buildReportConfig({ ...b, ...same, type: "matrix" });
    expect(m).toMatchObject({ columnGroup: SAVED.columnGroup, having: SAVED.having });
  });
  it("between needs 2 values; others 1; blanks never emitted", () => {
    const h = (op: string, values: string[]) => buildReportConfig({ ...b, type: "summary", having: { on: "count", op, values } });
    expect(h("between", ["1"])).not.toHaveProperty("having");
    expect(h("gt", [""])).not.toHaveProperty("having");
    expect(h("between", ["1", "5"]).having).toEqual({ on: "count", op: "between", values: ["1", "5"] });
    expect(h("gt", ["3", "9"]).having).toEqual({ on: "count", op: "gt", values: ["3"] });
  });
  it("duplicate axis not emitted; bucket only on a date field; sub-levels capped at 2", () => {
    const m = buildReportConfig({ ...b, type: "matrix", columnGroup: { field: "stage" } });
    expect(m).not.toHaveProperty("columnGroup");
    const s = buildReportConfig({
      ...b, type: "summary",
      subGroups: [{ field: "stage" }, { field: "signed_at", bucket: "month" }, { field: "signed_at", bucket: "month" }, { field: "closed_on" }, { field: "amount" }],
    });
    expect(s.subGroups).toEqual([{ field: "signed_at", bucket: "month" }, { field: "closed_on" }]);
    const n = buildReportConfig({ ...b, type: "summary", subGroups: [{ field: "amount", bucket: "month" }] });
    expect(n.subGroups).toEqual([{ field: "amount" }]);
  });
  it("tabular never gets grid keys", () => {
    const t = buildReportConfig({ ...b, type: "tabular", columnGroup: { field: "closed_on" }, having: { on: "count", op: "gt", values: ["1"] } });
    expect(t).not.toHaveProperty("columnGroup");
    expect(t).not.toHaveProperty("having");
  });
});
