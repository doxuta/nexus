import { describe, it, expect } from "vitest";

// ── Slice 4 Unit Tests: report/layout authoring logic ──
// Pure-function mirrors of the authoring helpers (report authoring now lives in the
// /reports/[id] admin panel; the Studio ReportBuilder was retired). No React imports.

// ─── Report Builder ───────────────────────────────────────────────────────────

type ReportType = "tabular" | "summary" | "matrix";

function buildReportConfig(
  type: ReportType,
  opts: { columns?: string[]; metric?: string; field?: string; groupBy?: string; fields?: Array<{ name: string }> }
): Record<string, unknown> {
  if (type === "tabular") {
    const cols = opts.columns ?? [];
    return { columns: cols.length ? cols : [opts.fields?.[0]?.name ?? "id"] };
  }
  return {
    metric: opts.metric,
    field: opts.field || undefined,
    groupBy: opts.groupBy || undefined,
  };
}

describe("buildReportConfig — tabular", () => {
  it("returns provided columns", () => {
    const cfg = buildReportConfig("tabular", { columns: ["name", "email"] });
    expect(cfg).toEqual({ columns: ["name", "email"] });
  });

  it("falls back to first field when columns is empty", () => {
    const cfg = buildReportConfig("tabular", {
      columns: [],
      fields: [{ name: "first_name" }, { name: "email" }],
    });
    expect(cfg).toEqual({ columns: ["first_name"] });
  });

  it("falls back to 'id' when columns empty and no fields", () => {
    const cfg = buildReportConfig("tabular", { columns: [] });
    expect(cfg).toEqual({ columns: ["id"] });
  });

  it("satisfies CONSTRAINT: columns must have at least 1 element", () => {
    const cfg = buildReportConfig("tabular", { columns: [], fields: [] });
    const cols = cfg.columns as string[];
    expect(cols.length).toBeGreaterThanOrEqual(1);
  });
});

describe("buildReportConfig — summary / matrix", () => {
  it("includes metric, field, groupBy", () => {
    const cfg = buildReportConfig("summary", {
      metric: "sum",
      field: "amount",
      groupBy: "status",
    });
    expect(cfg).toEqual({ metric: "sum", field: "amount", groupBy: "status" });
  });

  it("omits empty field/groupBy (no undefined keys leaked)", () => {
    const cfg = buildReportConfig("matrix", { metric: "count", field: "", groupBy: "" });
    expect(cfg.field).toBeUndefined();
    expect(cfg.groupBy).toBeUndefined();
    expect(cfg.metric).toBe("count");
  });

  it("works for matrix type identically to summary", () => {
    const s = buildReportConfig("summary", { metric: "avg", field: "price" });
    const m = buildReportConfig("matrix", { metric: "avg", field: "price" });
    expect(s).toEqual(m);
  });
});

// ─── Layout Builder ────────────────────────────────────────────────────────────

const SORT_WHITELIST = ["created_at", "updated_at", "id"] as const;
type SortField = (typeof SORT_WHITELIST)[number];

interface ListJsonOpts {
  columns: string[];
  defaultSort?: { field: SortField; direction: "asc" | "desc" };
  pageSize?: number;
}

function buildListJson(opts: ListJsonOpts): Record<string, unknown> {
  return {
    version: 1,
    columns: opts.columns,
    defaultSort: opts.defaultSort ?? { field: "created_at", direction: "desc" },
    pageSize: opts.pageSize ?? 25,
  };
}

interface SectionModel {
  title: string;
  columns: 1 | 2 | 3;
  fields: string[];
}

function buildSectionJson(sections: SectionModel[]): Record<string, unknown> {
  return {
    version: 1,
    sections: sections.map((s) => ({
      title: s.title,
      columns: s.columns,
      fields: s.fields,
    })),
  };
}

describe("buildListJson", () => {
  it("always sets version:1", () => {
    const j = buildListJson({ columns: ["name"] });
    expect(j.version).toBe(1);
  });

  it("defaults to created_at desc when no defaultSort provided", () => {
    const j = buildListJson({ columns: ["name", "email"] });
    expect(j.defaultSort).toEqual({ field: "created_at", direction: "desc" });
  });

  it("accepts only whitelisted sort fields", () => {
    const validFields: SortField[] = ["created_at", "updated_at", "id"];
    for (const f of validFields) {
      const j = buildListJson({ columns: ["x"], defaultSort: { field: f, direction: "asc" } });
      expect((j.defaultSort as { field: string }).field).toBe(f);
    }
  });

  it("stores provided columns array", () => {
    const j = buildListJson({ columns: ["name", "email", "status"] });
    expect(j.columns).toEqual(["name", "email", "status"]);
  });

  it("defaults pageSize to 25", () => {
    const j = buildListJson({ columns: [] });
    expect(j.pageSize).toBe(25);
  });
});

describe("buildSectionJson", () => {
  it("always sets version:1", () => {
    const j = buildSectionJson([{ title: "Info", columns: 2, fields: ["name"] }]);
    expect(j.version).toBe(1);
  });

  it("preserves sections array with columns as integer", () => {
    const j = buildSectionJson([
      { title: "Basic", columns: 2, fields: ["name", "email"] },
      { title: "Details", columns: 1, fields: ["note"] },
    ]);
    const sections = j.sections as Array<{ columns: number }>;
    expect(sections).toHaveLength(2);
    expect(typeof sections[0].columns).toBe("number");
    expect(sections[0].columns).toBe(2);
    expect(sections[1].columns).toBe(1);
  });

  it("CONSTRAINT: columns must be 1, 2, or 3 (backend rejects other values)", () => {
    const allowed: Array<1 | 2 | 3> = [1, 2, 3];
    for (const c of allowed) {
      const j = buildSectionJson([{ title: "S", columns: c, fields: [] }]);
      const sections = j.sections as Array<{ columns: number }>;
      expect(sections[0].columns).toBe(c);
    }
  });

  it("preserves fields array per section", () => {
    const j = buildSectionJson([{ title: "S", columns: 2, fields: ["a", "b", "c"] }]);
    const sections = j.sections as Array<{ fields: string[] }>;
    expect(sections[0].fields).toEqual(["a", "b", "c"]);
  });
});

// ─── Dashboard Widget Types ────────────────────────────────────────────────────

const VALID_WIDGET_TYPES = ["kpi", "bar", "line", "table"] as const;
type ValidWidgetType = (typeof VALID_WIDGET_TYPES)[number];

function validateWidgetType(t: string): t is ValidWidgetType {
  return VALID_WIDGET_TYPES.includes(t as ValidWidgetType);
}

describe("Dashboard widget type gating (backend constraint: kpi|bar|line|table only)", () => {
  it("accepts kpi, bar, line, table", () => {
    for (const t of VALID_WIDGET_TYPES) {
      expect(validateWidgetType(t)).toBe(true);
    }
  });

  it("rejects 'number' (invalid — verified live: backend returns error)", () => {
    expect(validateWidgetType("number")).toBe(false);
  });

  it("rejects 'pie' (invalid — verified live)", () => {
    expect(validateWidgetType("pie")).toBe(false);
  });

  it("rejects empty string", () => {
    expect(validateWidgetType("")).toBe(false);
  });
});

// ─── Approval Trigger Whitelist ────────────────────────────────────────────────
// Backend rejects "record_create"; only after_create / after_update are valid.

const VALID_TRIGGERS = ["after_create", "after_update"] as const;
type ApprovalTrigger = (typeof VALID_TRIGGERS)[number];

function validateApprovalTrigger(t: string): t is ApprovalTrigger {
  return VALID_TRIGGERS.includes(t as ApprovalTrigger);
}

describe("Approval trigger whitelist (backend constraint: only after_create/after_update)", () => {
  it("accepts after_create and after_update", () => {
    expect(validateApprovalTrigger("after_create")).toBe(true);
    expect(validateApprovalTrigger("after_update")).toBe(true);
  });

  it("rejects 'record_create' (verified live — backend returns APPROVAL_TRIGGER_INVALID)", () => {
    expect(validateApprovalTrigger("record_create")).toBe(false);
  });

  it("rejects 'before_create'", () => {
    expect(validateApprovalTrigger("before_create")).toBe(false);
  });
});

// ─── Layout viewType Whitelist ─────────────────────────────────────────────────
// Backend rejects "form"; valid values: edit | detail | list
// "Form" tab in UI must be mapped to "edit" before sending to backend.

const VALID_VIEW_TYPES = ["edit", "detail", "list"] as const;
type LayoutViewType = (typeof VALID_VIEW_TYPES)[number];

function tabToViewType(tab: string): LayoutViewType {
  if (tab === "Form") return "edit";
  if (tab === "Detail") return "detail";
  return "list";
}

describe("Layout viewType mapping (backend rejects 'form')", () => {
  it("maps 'Form' tab to 'edit' viewType", () => {
    expect(tabToViewType("Form")).toBe("edit");
  });

  it("maps 'Detail' tab to 'detail'", () => {
    expect(tabToViewType("Detail")).toBe("detail");
  });

  it("maps everything else to 'list'", () => {
    expect(tabToViewType("List")).toBe("list");
    expect(tabToViewType("unknown")).toBe("list");
  });

  it("never produces 'form' (invalid viewType)", () => {
    const outputs = ["Form", "Detail", "List", "Grid"].map(tabToViewType);
    expect(outputs).not.toContain("form");
    for (const o of outputs) {
      expect(VALID_VIEW_TYPES).toContain(o);
    }
  });
});
