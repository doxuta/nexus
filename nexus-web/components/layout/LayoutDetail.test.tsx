// LayoutDetail — headerHighlights contract (MOT-XUONG "không có tóm tắt đầu hồ sơ").
// Pure function: picks up to `max` real, non-empty values from the layout's FIRST detail section
// to give the record header more than a bare title, without inventing data or hard-coding a field
// name — order comes from the layout the maker already authored.
import { describe, it, expect } from "vitest";
import { headerHighlights } from "./LayoutDetail";
import type { FieldMeta, LayoutSection } from "@/lib/api";

const FIELDS: Record<string, FieldMeta> = {
  name: { name: "name", label: "Tên", fieldType: "text" },
  status: { name: "status", label: "Trạng thái", fieldType: "picklist" },
  email: { name: "email", label: "Email", fieldType: "email" },
  notes: { name: "notes", label: "Ghi chú", fieldType: "rich_text" },
  avatar: { name: "avatar", label: "Ảnh", fieldType: "image" },
  tags: { name: "tags", label: "Nhãn", fieldType: "multi_select" },
};

const SECTION = (fields: LayoutSection["fields"]): LayoutSection[] => [{ title: "Thông tin", fields }];

describe("headerHighlights", () => {
  it("picks up to 2 non-empty values from the first section, skipping the title's own field", () => {
    const sections = SECTION(["name", "status", "email"]);
    const data = { name: "Công ty ABC", status: "Đang xử lý", email: "a@b.vn" };
    const out = headerHighlights(sections, FIELDS, data, "Công ty ABC");

    expect(out).toHaveLength(2);
    expect(out.map((h) => h.field.name)).toEqual(["status", "email"]); // "name" dropped — IS the title
  });

  it("caps at the requested max even when more candidates qualify", () => {
    const sections = SECTION(["status", "email", "tags"]);
    const data = { status: "Mới", email: "a@b.vn", tags: ["vip"] };
    const out = headerHighlights(sections, FIELDS, data, "Bản ghi", 1);
    expect(out).toHaveLength(1);
    expect(out[0].field.name).toBe("status");
  });

  it("skips empty values (null, empty string, empty array) and spacer cells", () => {
    const sections = SECTION([{ spacer: true }, "status", "email", "tags"]);
    const data = { status: "", email: null, tags: [] as string[] };
    const out = headerHighlights(sections, FIELDS, data, "Bản ghi");
    expect(out).toHaveLength(0);
  });

  it("skips bulky/non-scalar field types (rich_text, image, composite, file)", () => {
    const sections = SECTION(["notes", "avatar", "status"]);
    const data = { notes: "<p>dài dòng</p>", avatar: "https://x/y.png", status: "Mới" };
    const out = headerHighlights(sections, FIELDS, data, "Bản ghi");
    expect(out.map((h) => h.field.name)).toEqual(["status"]);
  });

  it("degrades gracefully when a layout cell references a field missing from metadata", () => {
    const sections = SECTION(["ghost_field", "status"]);
    const data = { status: "Mới" };
    const out = headerHighlights(sections, FIELDS, data, "Bản ghi");
    expect(out.map((h) => h.field.name)).toEqual(["status"]);
  });

  it("returns [] when there is no first section", () => {
    expect(headerHighlights([], FIELDS, { status: "Mới" }, "Bản ghi")).toEqual([]);
  });
});
