// Tests for lib/widgetAuthoring — dashboard widget authoring helpers (app-face viewer
// /dashboards/[id] admin panel + report pin path).
// BE contract (dashboard_service.go): validWidgetTypes = kpi|bar|line|table|funnel;
// bucket requires groupBy (WIDGET_BUCKET_REQUIRES_GROUPBY) and a date-kind groupBy at run
// time (AGGREGATE_BUCKET_FIELD_NOT_DATE); groupBy needs a physical index
// (AGGREGATE_REQUIRES_INDEXED).
import { describe, expect, it } from "vitest";
import {
  WIDGET_TYPES, WIDGET_METRICS, normalizeWidgetBucket,
  blankWidget, newWidgetId, applyWidgetDraft, upsertWidgetDraft,
  groupByChoices, valueFieldChoices, tabularColumnChoices, isAggregatableField,
} from "./widgetAuthoring";
import type { DashboardWidgetDef } from "./api";

const BACKEND_VALID = ["kpi", "bar", "line", "table", "funnel"];

describe("WIDGET_TYPES", () => {
  it("offers funnel as a REAL selectable type", () => {
    expect(WIDGET_TYPES.some((t) => t.key === "funnel")).toBe(true);
  });

  it("has NO pie option and no vaporware ('sắp ra mắt') labels", () => {
    expect(WIDGET_TYPES.some((t) => (t.key as string) === "pie")).toBe(false);
    for (const t of WIDGET_TYPES) {
      expect(t.label.toLowerCase()).not.toContain("sắp ra mắt");
      expect(t.label.toLowerCase()).not.toContain("pie");
    }
  });

  it("every option saves exactly what it says: key ∈ backend validWidgetTypes", () => {
    expect(WIDGET_TYPES.map((t) => t.key).sort()).toEqual([...BACKEND_VALID].sort());
  });

  it("WIDGET_METRICS matches the aggregate engine's metric set", () => {
    expect([...WIDGET_METRICS].sort()).toEqual(["avg", "count", "max", "min", "sum"]);
  });
});

describe("normalizeWidgetBucket", () => {
  const base = { id: "w1", title: "T", type: "line", entity: "accounts", metric: "count" };

  it("keeps the bucket on a created_at/updated_at groupBy", () => {
    const w = { ...base, groupBy: "created_at", bucket: "month" };
    expect(normalizeWidgetBucket(w)).toEqual(w);
    expect(normalizeWidgetBucket({ ...base, groupBy: "updated_at", bucket: "day" }).bucket).toBe("day");
  });

  it("keeps the bucket on a date/datetime field when entity fields are supplied (pin path)", () => {
    const fields = [{ name: "closed_on", fieldType: "date" }];
    const w = { ...base, groupBy: "closed_on", bucket: "quarter" };
    expect(normalizeWidgetBucket(w, fields).bucket).toBe("quarter");
  });

  it("strips the bucket when groupBy is missing or not date-kind (id / picklist)", () => {
    expect("bucket" in normalizeWidgetBucket({ ...base, bucket: "month" })).toBe(false);
    expect("bucket" in normalizeWidgetBucket({ ...base, groupBy: "id", bucket: "month" })).toBe(false);
    const fields = [{ name: "stage", fieldType: "picklist" }];
    expect("bucket" in normalizeWidgetBucket({ ...base, groupBy: "stage", bucket: "month" }, fields)).toBe(false);
  });

  it("strips an invalid bucket token and an empty-string bucket", () => {
    expect("bucket" in normalizeWidgetBucket({ ...base, groupBy: "created_at", bucket: "decade" })).toBe(false);
    expect("bucket" in normalizeWidgetBucket({ ...base, groupBy: "created_at", bucket: "" })).toBe(false);
  });

  it("leaves a widget without a bucket untouched (same reference)", () => {
    const w = { ...base, groupBy: "created_at" };
    expect(normalizeWidgetBucket(w)).toBe(w);
  });

  it("preserves every other field when stripping (full-replace PUT must not lose data)", () => {
    const w = { ...base, groupBy: "id", bucket: "month", field: "amount", x: 1, y: 2, w: 3, h: 4, filters: [{ field: "stage" }] };
    const out = normalizeWidgetBucket(w);
    expect(out).toEqual({ ...base, groupBy: "id", field: "amount", x: 1, y: 2, w: 3, h: 4, filters: [{ field: "stage" }] });
  });
});

describe("blankWidget / newWidgetId", () => {
  it("seeds a KPI count draft over the entity with a unique id", () => {
    const a = blankWidget("accounts");
    const b = blankWidget("accounts");
    expect(a).toMatchObject({ title: "Ô số liệu mới", type: "kpi", entity: "accounts", metric: "count" });
    expect(a.id).not.toBe(b.id);
    expect(newWidgetId()).not.toBe(newWidgetId());
  });
});

describe("applyWidgetDraft — form fields overlay the stored widget, nothing else moves", () => {
  const stored: DashboardWidgetDef = {
    id: "w1", title: "Cũ", type: "bar", entity: "deals", metric: "sum", field: "amount",
    groupBy: "stage", x: 3, y: 1, w: 6, h: 4, filters: [{ field: "stage", op: "eq", values: ["won"] }],
  };

  it("preserves x/y/w/h and filters when editing (draft carries NO layout keys)", () => {
    const out = applyWidgetDraft(stored, {
      id: "w1", title: "Mới", type: "line", entity: "deals", metric: "avg", field: "amount", groupBy: "stage",
    }, [{ name: "stage", fieldType: "picklist" }]);
    expect(out).toEqual({ ...stored, title: "Mới", type: "line", metric: "avg" });
    expect(out.x).toBe(3); // a stale draft can never clobber a fresher grid rect
  });

  // specs/070 #266: the widget form now edits the fixed filters. A draft that carries them writes
  // them (builder rows → the saved dialect; emptied ⇒ key removed); a draft without the key keeps
  // the stored ones untouched (the round-trip contract above).
  it("writes the draft's fixed filters when the form models them", () => {
    const out = applyWidgetDraft(stored, {
      id: "w1", title: "Phiếu quá hạn", type: "kpi", entity: "deals", metric: "count",
      filters: [
        { id: "a", field: "due", op: "lt", value: "today" },
        { id: "b", field: "stage", op: "neq", value: "done" },
        { id: "c", field: "", op: "", value: "" },
      ],
    });
    expect(out.filters).toEqual([
      { field: "due", op: "lt", values: ["today"] },
      { field: "stage", op: "neq", values: ["done"] },
    ]);
    const cleared = applyWidgetDraft(stored, { id: "w1", title: "x", type: "kpi", entity: "deals", metric: "count", filters: [] });
    expect("filters" in cleared).toBe(false);
  });

  it("REMOVES cleared optionals instead of persisting empty strings", () => {
    const out = applyWidgetDraft(stored, {
      id: "w1", title: "Mới", type: "kpi", entity: "deals", metric: "count", field: "", groupBy: "",
    });
    expect("field" in out).toBe(false);
    expect("groupBy" in out).toBe(false);
    expect("bucket" in out).toBe(false);
  });

  it("strips a bucket that no longer rides a date-kind groupBy", () => {
    const out = applyWidgetDraft(stored, {
      id: "w1", title: "T", type: "bar", entity: "deals", metric: "count", groupBy: "stage", bucket: "month",
    }, [{ name: "stage", fieldType: "picklist" }]);
    expect("bucket" in out).toBe(false);
  });

  it("keeps the bucket on a date field of the entity", () => {
    const out = applyWidgetDraft(stored, {
      id: "w1", title: "T", type: "line", entity: "deals", metric: "count", groupBy: "closed_on", bucket: "quarter",
    }, [{ name: "closed_on", fieldType: "date" }]);
    expect(out.bucket).toBe("quarter");
  });

  it("falls back to a non-empty title and builds a widget from scratch when stored is absent", () => {
    const out = applyWidgetDraft(undefined, { id: "w9", title: "   ", type: "kpi", entity: "tickets", metric: "count" });
    expect(out).toEqual({ id: "w9", title: "Ô số liệu mới", type: "kpi", entity: "tickets", metric: "count" });
  });
});

describe("upsertWidgetDraft", () => {
  const w1: DashboardWidgetDef = { id: "w1", title: "A", type: "kpi", entity: "a", metric: "count", x: 0, y: 0, w: 4, h: 3 };
  const w2: DashboardWidgetDef = { id: "w2", title: "B", type: "bar", entity: "b", metric: "count" };

  it("replaces in place by id, keeping array order and sibling widgets untouched", () => {
    const next = upsertWidgetDraft([w1, w2], { id: "w2", title: "B2", type: "table", entity: "b", metric: "count" });
    expect(next.map((w) => w.id)).toEqual(["w1", "w2"]);
    expect(next[0]).toBe(w1);
    expect(next[1]).toMatchObject({ title: "B2", type: "table" });
  });

  it("appends a new widget when the id is not present", () => {
    const next = upsertWidgetDraft([w1], { id: "w3", title: "C", type: "kpi", entity: "c", metric: "count" });
    expect(next).toHaveLength(2);
    expect(next[1]).toEqual({ id: "w3", title: "C", type: "kpi", entity: "c", metric: "count" });
  });
});

describe("groupByChoices — indexed gate + keep-current", () => {
  const fields = [
    { name: "stage", label: "Giai đoạn", fieldType: "picklist", options: { indexed: true } },
    { name: "code", fieldType: "text", isUnique: true },
    { name: "note", label: "Ghi chú", fieldType: "text" },
    { name: "owner", fieldType: "relation", options: { indexed: true } },
    { name: "body", fieldType: "rich_text" },
  ];

  it("always offers the system timestamps and the record owner first, indexed", () => {
    const out = groupByChoices([]);
    expect(out.map((c) => c.value)).toEqual(["created_at", "updated_at", "ownerId"]);
    expect(out.find((c) => c.value === "ownerId")?.label).toBe("Người phụ trách");
    expect(out.every((c) => c.indexed)).toBe(true);
  });

  it("marks unique/options.indexed fields selectable and the rest not indexed", () => {
    const out = groupByChoices(fields);
    const by = Object.fromEntries(out.map((c) => [c.value, c]));
    expect(by.stage.indexed).toBe(true);
    expect(by.code.indexed).toBe(true);
    expect(by.note.indexed).toBe(false);
  });

  // specs/070 #265: the engine groups by relation / user / owner through the same sort allowlist
  // (record_orchestration.go buildSortAllowlist: <field>_id / owner_id are always indexed), so the
  // picker offers them — "Doanh thu theo bác sĩ", "Phiếu theo KTV". Only shapes with no scalar
  // column stay out.
  it("offers relation and user fields as always-indexed axes; still excludes rich_text/multi_select/polymorphic", () => {
    const out = groupByChoices([
      ...fields,
      { name: "doctor", label: "Bác sĩ", fieldType: "relation" },
      { name: "technician", label: "KTV", fieldType: "user" },
      { name: "tags", fieldType: "multi_select" },
      { name: "related", fieldType: "polymorphic_relation" },
    ]);
    const by = Object.fromEntries(out.map((c) => [c.value, c]));
    expect(by.doctor).toMatchObject({ label: "Bác sĩ", indexed: true });
    expect(by.technician).toMatchObject({ label: "KTV", indexed: true });
    expect(by.owner).toMatchObject({ indexed: true });
    expect(out.some((c) => ["body", "tags", "related"].includes(c.value))).toBe(false);
  });

  it("a declared field named ownerId wins over the system owner entry", () => {
    const out = groupByChoices([{ name: "ownerId", label: "Chủ", fieldType: "text" }]);
    expect(out.filter((c) => c.value === "ownerId")).toEqual([{ value: "ownerId", label: "Chủ", indexed: false, fixable: true }]);
  });

  // "Bật index ngay" (#265): an unindexed field that CAN take an index is fixable in place; one
  // that cannot (boolean, datetime, a formula…) is not — mirror of lib/fieldOptions supportsIndexed.
  it("flags which unindexed fields can be indexed in place", () => {
    const out = groupByChoices([
      { name: "note", fieldType: "text" },
      { name: "due", fieldType: "date" },
      { name: "active", fieldType: "boolean" },
      { name: "at", fieldType: "datetime" },
      { name: "total", fieldType: "formula" },
      { name: "stage", fieldType: "picklist", options: { indexed: true } },
    ]);
    const by = Object.fromEntries(out.map((c) => [c.value, c]));
    expect(by.note.fixable).toBe(true);
    expect(by.due.fixable).toBe(true);
    expect(by.active.fixable).toBe(false);
    expect(by.at.fixable).toBe(false);
    expect(by.total.fixable).toBe(false);
    expect(by.stage.fixable).toBe(false); // already indexed — nothing to fix
  });

  it("keeps a stored out-of-list value selectable (legacy 'id', deleted field)", () => {
    const out = groupByChoices(fields, "id");
    expect(out.at(-1)).toEqual({ value: "id", label: "id", indexed: true, fixable: false });
    // …but does not duplicate a value already present
    expect(groupByChoices(fields, "stage").filter((c) => c.value === "stage")).toHaveLength(1);
  });
});

describe("valueFieldChoices — formula reading a lookup/rollup (specs/080)", () => {
  it("hides a formula computed at view time; keeps a stored formula", () => {
    const out = valueFieldChoices([
      { name: "sl", fieldType: "number" },
      { name: "don_gia", fieldType: "lookup" },
      { name: "thanh_tien", fieldType: "formula", options: { formula: "sl * don_gia" } },
      { name: "gap_doi", fieldType: "formula", options: { formula: "sl * 2" } },
      { name: "nhan", fieldType: "formula", options: { formula: 'IF(sl > 1, "don_gia", "x")' } },
    ]);
    expect(out.map((c) => c.value)).toEqual(["sl", "gap_doi", "nhan"]);
  });
});

describe("valueFieldChoices", () => {
  it("lists scalar fields with labels and keeps the stored out-of-list value", () => {
    const out = valueFieldChoices(
      [{ name: "amount", label: "Giá trị", fieldType: "currency" }, { name: "owner", fieldType: "relation" }],
      "legacy_field",
    );
    expect(out).toEqual([
      { value: "amount", label: "Giá trị" },
      { value: "legacy_field", label: "legacy_field" },
    ]);
  });

  // specs/070 #264: the picker offers EXACTLY what the aggregate engine can sum
  // (record_aggregate.go resolveMetricField): number/currency/rating plus a formula whose result
  // is stored as a number or a currency. A text formula, a roll-up (computed at read time), a
  // lookup, text/picklist/date… were offered and then died with AGGREGATE_FIELD_NOT_NUMERIC.
  it("offers only fields the engine can sum — formula number/currency in, text formula and roll-up out", () => {
    const out = valueFieldChoices([
      { name: "qty", label: "Số lượng", fieldType: "number" },
      { name: "stars", label: "Đánh giá", fieldType: "rating" },
      { name: "price", label: "Đơn giá", fieldType: "currency" },
      { name: "balance", label: "Còn nợ", fieldType: "formula", options: { formula: "a - b" } },
      { name: "margin", label: "Lãi", fieldType: "formula", options: { formula: "a - b", resultType: "number" } },
      { name: "line_total", label: "Thành tiền", fieldType: "formula", options: { formula: "a * b", resultType: "currency" } },
      { name: "tier", label: "Hạng", fieldType: "formula", options: { formula: "IF(a > 1, \"x\", \"y\")", resultType: "text" } },
      { name: "enrolled", label: "Sĩ số", fieldType: "rollup" },
      { name: "account_name", label: "Tên KH", fieldType: "lookup" },
      { name: "name", label: "Tên", fieldType: "text" },
      { name: "stage", label: "Giai đoạn", fieldType: "picklist" },
      { name: "due", label: "Hạn", fieldType: "date" },
    ]);
    expect(out.map((c) => c.value)).toEqual(["qty", "stars", "price", "balance", "margin", "line_total"]);
  });
});

describe("tabularColumnChoices", () => {
  // REPORT-RELATION-LABELS: a tabular report's columns CAN be relation/user fields — the run and
  // the CSV resolve them to labels now — so the column picker must offer them. groupBy/value
  // pickers keep excluding relations (they aggregate on raw UUID keys).
  it("offers relation and user fields as columns, still hides types that cannot fill a cell", () => {
    const out = tabularColumnChoices([
      { name: "name", label: "Tiêu đề", fieldType: "text" },
      { name: "account", label: "Khách hàng", fieldType: "relation" },
      { name: "owner", label: "Người phụ trách", fieldType: "user" },
      { name: "related", fieldType: "polymorphic_relation" },
      { name: "body", fieldType: "rich_text" },
    ]);
    expect(out).toEqual([
      { value: "name", label: "Tiêu đề" },
      { value: "account", label: "Khách hàng" },
      { value: "owner", label: "Người phụ trách" },
    ]);
  });
});

describe("isAggregatableField — stored roll-up (specs/083)", () => {
  it("offers a roll-up only when it keeps its total in the record", () => {
    expect(isAggregatableField({ fieldType: "rollup", options: { stored: true } })).toBe(true);
    expect(isAggregatableField({ fieldType: "rollup", options: {} })).toBe(false);
    expect(isAggregatableField({ fieldType: "rollup" })).toBe(false);
  });
});

describe("applyWidgetDraft — joins (queue #294)", () => {
  const stored = {
    id: "w1", title: "Theo khoá", type: "bar", entity: "enrollments", metric: "count", groupBy: "course.name",
    joins: [{ relationship: "klass" }, { relationship: "course" }],
  } as DashboardWidgetDef;
  const draft = { id: "w1", title: "Theo khoá", type: "bar", entity: "enrollments", metric: "count", groupBy: "course.name" };
  it("keeps the stored joins when the form edits something else", () => {
    expect(applyWidgetDraft(stored, { ...draft, title: "Sĩ số theo khoá" }).joins).toEqual(stored.joins);
  });
  it("drops them once neither groupBy nor field uses a joined token", () => {
    expect(applyWidgetDraft(stored, { ...draft, groupBy: "status" }).joins).toBeUndefined();
  });
  it("drops them when the card is pointed at another entity (they belong to the old one)", () => {
    expect(applyWidgetDraft(stored, { ...draft, entity: "invoices", groupBy: undefined }).joins).toBeUndefined();
  });
});
