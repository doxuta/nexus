// specs/075 T103: pure edit steps of the LIST screen (columns, default sort, declared Kanban/Calendar field).
import { describe, expect, it } from "vitest";
import type { FieldMeta, LayoutView } from "@/lib/api";
import {
  listCandidateFields, listDraftEquals, listDraftFromView, listDraftToJson, moveListCol, setListKanban, toggleListCol,
} from "./screenEditorModel";

const f = (name: string, fieldType: string): FieldMeta => ({ id: name, name, label: name, fieldType }) as FieldMeta;
const fields = [f("name", "text"), f("owner", "relation"), f("notes", "rich_text"), f("tags", "multi_select"), f("stage", "picklist"), f("due", "date"), f("a", "text"), f("b", "text"), f("c", "text")];

describe("list screen model", () => {
  it("candidates drop relation / rich_text / multi_select", () => {
    expect(listCandidateFields(fields).map((x) => x.name)).toEqual(["name", "stage", "due", "a", "b", "c"]);
  });

  it("seed: served columns win; no columns falls back to the first five candidates; default sort is created_at desc", () => {
    const served: LayoutView = { columns: [{ field: "stage" }, { field: "name", labelOverride: "Tên KH" }], defaultSort: { field: "updated_at", order: "asc" }, kanbanField: "stage", calendarField: "due" };
    expect(listDraftFromView(served, fields)).toEqual({ cols: ["stage", "name"], labels: { name: "Tên KH" }, sortField: "updated_at", sortOrder: "asc", kanban: "stage", calendar: "due" });
    expect(listDraftFromView(null, fields)).toEqual({ cols: ["name", "stage", "due", "a", "b"], labels: {}, sortField: "created_at", sortOrder: "desc", kanban: "", calendar: "" });
  });

  it("toggle adds at the end, removes, and refuses a non-candidate", () => {
    const d = listDraftFromView(null, fields);
    expect(toggleListCol(d, "c", true, fields).cols.at(-1)).toBe("c");
    expect(toggleListCol(d, "name", false, fields).cols).not.toContain("name");
    expect(toggleListCol(d, "owner", true, fields)).toBe(d);
  });

  it("move steps one place and stays put at the edges", () => {
    const d = { ...listDraftFromView(null, fields), cols: ["a", "b", "c"] };
    expect(moveListCol(d, "c", -1).cols).toEqual(["a", "c", "b"]);
    expect(moveListCol(d, "a", -1)).toBe(d);
    expect(moveListCol(d, "c", 1)).toBe(d);
  });

  it("changing only Kanban or Calendar makes the draft differ (dirty)", () => {
    const d = listDraftFromView(null, fields);
    expect(listDraftEquals(d, setListKanban(d, "stage"))).toBe(false);
    expect(listDraftEquals(d, { ...d })).toBe(true);
  });

  it("json: no sortable key, kanban/calendar only when declared, a kept label travels", () => {
    const d = { ...listDraftFromView(null, fields), cols: ["name", "stage"], labels: { name: "Tên KH", gone: "x" }, kanban: "stage" };
    const j = listDraftToJson(d);
    expect(j).toEqual({ version: 1, columns: [{ field: "name", labelOverride: "Tên KH" }, { field: "stage" }], defaultSort: { field: "created_at", order: "desc" }, kanbanField: "stage" });
    expect(JSON.stringify(j)).not.toContain("sortable");
    expect(j).not.toHaveProperty("calendarField");
  });
});
