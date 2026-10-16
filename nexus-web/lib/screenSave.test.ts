import { beforeEach, describe, expect, it, vi } from "vitest";
import type { FieldMeta } from "@/lib/api";
import { draftFromSections } from "@/lib/layoutDraft";

const calls: string[] = [];
const api = vi.hoisted(() => ({
  updateField: vi.fn(),
  createLayout: vi.fn(),
  updateLayout: vi.fn(),
}));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));

import { saveListScreen, saveScreen, savedRequiredSummary } from "./screenSave";

const f = (name: string, isRequired = false): FieldMeta => ({ id: `id-${name}`, name, label: name, fieldType: "text", isRequired });
const fields = [f("a", true), f("b"), f("c"), f("d")];
const draft = draftFromSections([{ columns: 2, fields: ["a", "b"] }]);
const base = { entity: "crm_x", layoutId: "L1", asNew: false, draft, fields };

beforeEach(() => {
  calls.length = 0;
  api.updateField.mockReset().mockImplementation(async (id: string, p: { isRequired: boolean }) => { calls.push(`field:${id}:${p.isRequired}`); return {}; });
  api.createLayout.mockReset().mockImplementation(async () => { calls.push("layout:POST"); return { id: "L9" }; });
  api.updateLayout.mockReset().mockImplementation(async () => { calls.push("layout:PUT"); return { id: "L1" }; });
});

describe("saveScreen: three phases (turn required off -> layout -> turn required on)", () => {
  it("orders the writes off, layout, on", async () => {
    const r = await saveScreen({ ...base, edits: { a: false, c: true } });
    expect(calls).toEqual(["field:id-a:false", "layout:PUT", "field:id-c:true"]);
    expect(r).toEqual({ ok: true, layoutId: "L1" });
  });
  it("first save POSTs, later saves PUT; asNew POSTs again", async () => {
    expect((await saveScreen({ ...base, layoutId: null, edits: {} })).ok).toBe(true);
    expect(calls).toEqual(["layout:POST"]);
    calls.length = 0;
    await saveScreen({ ...base, asNew: true, edits: {} });
    expect(calls).toEqual(["layout:POST"]);
  });
  it("phase 1 failure stops: the layout is not written", async () => {
    api.updateField.mockRejectedValueOnce(new Error("boom"));
    const r = await saveScreen({ ...base, edits: { a: false, c: true } });
    expect(r).toMatchObject({ ok: false, phase: 1, failedField: "a" });
    expect(calls).toEqual([]);
  });
  it("phase 2 failure: required-on edits are not written; the turned-off ones stay off", async () => {
    api.updateLayout.mockRejectedValueOnce(new Error("422"));
    const r = await saveScreen({ ...base, edits: { a: false, c: true } });
    expect(r).toMatchObject({ ok: false, phase: 2 });
    expect(calls).toEqual(["field:id-a:false"]);
    if (!r.ok) expect(r.remaining).toEqual({ c: true });
  });
  it("phase 3 reports a partial result and keeps the rows that failed", async () => {
    api.updateField.mockImplementation(async (id: string, p: { isRequired: boolean }) => {
      if (id === "id-c") throw new Error("FIELD_REQUIRED_HAS_VISIBLE_WHEN");
      calls.push(`field:${id}:${p.isRequired}`);
      return {};
    });
    const r = await saveScreen({ ...base, edits: { b: true, c: true, d: true } });
    expect(r).toMatchObject({ ok: false, phase: 3, failedField: "c", done: 2, total: 3 });
    if (!r.ok) {
      expect(r.remaining).toEqual({ c: true });
      expect(savedRequiredSummary(r, fields, () => "câu lỗi")).toBe("Đã đặt bắt buộc 2 trên 3 trường; lỗi: c: câu lỗi");
    }
  });
  it("a PUT 404 LAYOUT_NOT_FOUND comes back as layoutGone so the UI can offer 'save as new'", async () => {
    const { ApiError } = await import("@/lib/api");
    api.updateLayout.mockRejectedValueOnce(new ApiError(404, "LAYOUT_NOT_FOUND", "x"));
    const r = await saveScreen({ ...base, edits: {} });
    expect(r).toMatchObject({ ok: false, phase: 2, layoutGone: true });
  });
  it("edits equal to the stored value are skipped", async () => {
    await saveScreen({ ...base, edits: { a: true, b: false } });
    expect(calls).toEqual(["layout:PUT"]);
  });
});

describe("saveListScreen: one layout write, viewType list", () => {
  const json = { version: 1, columns: [{ field: "a" }], defaultSort: { field: "created_at", order: "desc" } };
  it("first save POSTs a list layout, later saves PUT", async () => {
    expect(await saveListScreen({ entity: "crm_x", layoutId: null, asNew: false, layoutJson: json })).toEqual({ ok: true, layoutId: "L9" });
    expect(api.createLayout).toHaveBeenCalledWith({ entityName: "crm_x", viewType: "list", layoutJson: json, isDefault: false });
    expect(await saveListScreen({ entity: "crm_x", layoutId: "L1", asNew: false, layoutJson: json })).toEqual({ ok: true, layoutId: "L1" });
    expect(api.updateLayout).toHaveBeenCalledWith("L1", json);
  });
  it("PUT 404 LAYOUT_NOT_FOUND is reported as layoutGone", async () => {
    const { ApiError } = await import("@/lib/api");
    api.updateLayout.mockRejectedValue(new ApiError(404, "LAYOUT_NOT_FOUND", "x"));
    const r = await saveListScreen({ entity: "crm_x", layoutId: "L1", asNew: false, layoutJson: json });
    expect(r).toMatchObject({ ok: false, layoutGone: true, layoutId: "L1" });
  });
});
