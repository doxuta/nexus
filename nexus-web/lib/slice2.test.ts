// Slice 2 — test suite for new field types (image + formula) added in the records-core build.
// Covers: registry (FieldType), zod-derivation (layoutSchema), payload transforms (recordPayload),
//         and the filter query serializer (filtersToQuery) from LayoutList.

import { describe, it, expect } from "vitest";
import type { FieldMeta } from "@/lib/api";
import { fieldZodType } from "./layoutSchema";
import { emptyDefault, reverseValue, buildPayload } from "./recordPayload";
import { filtersToQuery, type FilterRow } from "@/components/layout/LayoutList";

const f = (name: string, fieldType: string, extra: Partial<FieldMeta> = {}): FieldMeta => ({
  name, fieldType, ...extra,
});

// ── image field ──────────────────────────────────────────────────────────────────
describe("image field — zod schema", () => {
  it("optional image: accepts empty string", () => {
    const field = f("photo", "image");
    const r = fieldZodType(field).safeParse("");
    expect(r.success).toBe(true);
  });

  it("optional image: accepts URL string", () => {
    const field = f("photo", "image");
    const r = fieldZodType(field).safeParse("https://cdn.example.com/photo.jpg");
    expect(r.success).toBe(true);
  });

  it("optional image: accepts {url, name} object", () => {
    const field = f("photo", "image");
    const r = fieldZodType(field).safeParse({ url: "https://cdn.example.com/photo.jpg", name: "photo.jpg" });
    expect(r.success).toBe(true);
  });
});

describe("image field — recordPayload", () => {
  it("emptyDefault gives empty string", () => {
    expect(emptyDefault(f("photo", "image"))).toBe("");
  });

  it("reverseValue: URL string passes through", () => {
    expect(reverseValue(f("photo", "image"), "https://cdn/x.png")).toBe("https://cdn/x.png");
  });

  it("reverseValue: {url, name} object passes through", () => {
    const val = { url: "https://cdn/x.png", name: "x.png" };
    expect(reverseValue(f("photo", "image"), val)).toEqual(val);
  });

  it("reverseValue: null → empty string", () => {
    expect(reverseValue(f("photo", "image"), null)).toBe("");
  });

  // specs/060: the server stores image as [{id,name}] refs written by the file API only (checkFile
  // answers a URL string with 422), so the form never sends an image — whatever it holds.
  it("buildPayload: image is never sent, even when it holds a value", () => {
    const fields = [f("photo", "image", { isRequired: true })];
    expect(buildPayload(fields, { photo: "https://cdn/x.png" })).toEqual({});
    expect(buildPayload(fields, { photo: [{ id: "f1", name: "x.png" }] }, "update")).toEqual({});
  });

  it("buildPayload: empty optional image dropped", () => {
    const fields = [f("photo", "image")];
    expect(buildPayload(fields, { photo: "" })).toEqual({});
  });
});

// ── formula field ─────────────────────────────────────────────────────────────────
describe("formula field — zod schema", () => {
  it("formula: accepts any value (permissive — server-computed)", () => {
    const field = f("total", "formula");
    expect(fieldZodType(field).safeParse("10000").success).toBe(true);
    expect(fieldZodType(field).safeParse(10000).success).toBe(true);
    expect(fieldZodType(field).safeParse(null).success).toBe(true);
    expect(fieldZodType(field).safeParse({}).success).toBe(true);
  });
});

describe("formula field — recordPayload", () => {
  it("emptyDefault gives empty string", () => {
    expect(emptyDefault(f("total", "formula"))).toBe("");
  });

  it("reverseValue: string passes through", () => {
    expect(reverseValue(f("total", "formula"), "12000")).toBe("12000");
  });

  it("reverseValue: number → string", () => {
    expect(reverseValue(f("total", "formula"), 5000)).toBe("5000");
  });

  it("reverseValue: null → empty string", () => {
    expect(reverseValue(f("total", "formula"), null)).toBe("");
  });

  it("buildPayload: formula fields are NEVER included (server-computed)", () => {
    const fields = [
      f("name", "text", { isRequired: true }),
      f("total", "formula"),
    ];
    const out = buildPayload(fields, { name: "Acme", total: "999" });
    expect(out).toEqual({ name: "Acme" }); // formula excluded
    expect("total" in out).toBe(false);
  });
});

// ── registry completeness — all 16 types must be handled without throwing ────────
describe("fieldZodType — all 16 types parse without throwing", () => {
  const TYPES = [
    "text", "rich_text", "number", "boolean", "date", "datetime",
    "email", "currency", "picklist", "multi_select", "relation",
    "rating", "composite", "file", "image", "formula",
  ];

  for (const ft of TYPES) {
    it(`${ft}: does not throw on any call`, () => {
      const field = f("x", ft);
      expect(() => fieldZodType(field)).not.toThrow();
    });
  }
});

// ── Filter query builder — real filtersToQuery from LayoutList ────────────────────
describe("filtersToQuery (real function from LayoutList)", () => {
  const f = (id: string, field: string, op: string, value: string): FilterRow => ({
    id, field, op, value,
  });

  it("emits field[op]=value for eq operator", () => {
    const qs = filtersToQuery([f("1", "subject", "eq", "hello")]);
    const p = new URLSearchParams(qs);
    expect(p.get("subject[eq]")).toBe("hello");
  });

  it("isnull: sets value to '1', ignores blank value input", () => {
    const qs = filtersToQuery([f("1", "deleted_at", "isnull", "")]);
    const p = new URLSearchParams(qs);
    expect(p.get("deleted_at[isnull]")).toBe("1");
  });

  it("notnull: sets value to '1'", () => {
    const qs = filtersToQuery([f("1", "phone", "notnull", "")]);
    const p = new URLSearchParams(qs);
    expect(p.get("phone[notnull]")).toBe("1");
  });

  it("skips rows with blank value (non-null ops)", () => {
    const qs = filtersToQuery([f("1", "name", "contains", "")]);
    expect(qs).toBe("");
  });

  it("skips rows with missing field", () => {
    const qs = filtersToQuery([f("1", "", "eq", "value")]);
    expect(qs).toBe("");
  });

  it("handles multiple filter rows", () => {
    const qs = filtersToQuery([
      f("1", "status", "eq", "open"),
      f("2", "priority", "eq", "high"),
    ]);
    const p = new URLSearchParams(qs);
    expect(p.get("status[eq]")).toBe("open");
    expect(p.get("priority[eq]")).toBe("high");
  });
});
