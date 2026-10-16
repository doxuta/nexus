// specs/079 f7-lich-su-dong-thoi-gian T052: pure helpers of the version page (path lookup, change rows, filter, type label).
import { describe, expect, it } from "vitest";
import type { PublishVersionDoc } from "./api";
import { docChangeRows, docTypeLabel, filterDocs, valueAtPath } from "./versionDiff";

const doc = (extra: Partial<PublishVersionDoc> & { entityName?: string } = {}): PublishVersionDoc => ({
  docType: "workflow", docKey: "k", label: "Luật A", op: "update", changedPaths: [], moreChanged: 0, before: {}, after: {}, applyStatus: "applied", ...extra,
});

describe("valueAtPath", () => {
  it("walks dotted keys and [n] indexes", () => {
    const d = { actions: [{ stampField: "x" }], options: { values: ["a", "b"] } };
    expect(valueAtPath(d, "actions[0].stampField")).toEqual({ found: true, value: "x" });
    expect(valueAtPath(d, "options.values[1]")).toEqual({ found: true, value: "b" });
  });
  it("a path that is not there is found:false, also on null / scalar docs", () => {
    expect(valueAtPath({ a: 1 }, "a.b")).toEqual({ found: false });
    expect(valueAtPath({ a: [] }, "a[2]")).toEqual({ found: false });
    expect(valueAtPath(null, "a")).toEqual({ found: false });
  });
});

describe("docChangeRows", () => {
  it("marks + when only after has the path, − when only before, ~ when both differ, = when equal", () => {
    const rows = docChangeRows(doc({ before: { a: 1, b: 2, c: 3 }, after: { a: 2, d: 4, c: 3 }, changedPaths: ["a", "b", "d", "c"] }));
    expect(rows.map((r) => [r.path, r.mark])).toEqual([["a", "~"], ["b", "−"], ["d", "+"], ["c", "="]]);
    expect(rows[0]).toMatchObject({ before: 1, after: 2, label: null });
  });
  it("a path neither side can look up still gets a row (never dropped)", () => {
    const rows = docChangeRows(doc({ changedPaths: ["x.y"] }));
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ path: "x.y", found: false });
  });
  it("keeps the server mask and masks secret-named keys even when the server sent clear text", () => {
    const rows = docChangeRows(doc({ before: { signingKey: "abc", note: "[đã che]" }, after: { signingKey: "def", note: "[đã che]" }, changedPaths: ["signingKey", "note"] }));
    expect(rows[0]).toMatchObject({ before: "[đã che]", after: "[đã che]" });
    expect(rows[1].before).toBe("[đã che]");
  });
});

describe("filterDocs", () => {
  const docs = [doc({ docType: "workflow" }), doc({ docType: "report", entityName: "crm_leads" }), doc({ docType: "layout", entityName: "crm_leads" })];
  it("filters by loai", () => expect(filterDocs(docs, { loai: "report" })).toHaveLength(1));
  it("filters by bang", () => expect(filterDocs(docs, { bang: "crm_leads" })).toHaveLength(2));
  it("ignores bang when no doc has an entityName", () => expect(filterDocs([doc(), doc()], { bang: "x" })).toHaveLength(2));
});

describe("docTypeLabel", () => {
  it("known types get a Vietnamese name, an unknown one shows the raw code", () => {
    expect(docTypeLabel("workflow")).toBe("Luật tự động");
    expect(docTypeLabel("weird_type")).toBe("weird_type");
  });
});
