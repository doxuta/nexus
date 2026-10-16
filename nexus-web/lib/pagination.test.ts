// lib/pagination.test.ts — cursor-vs-offset selection contract (§5.7).
import { describe, it, expect } from "vitest";
import type { FieldMeta } from "@/lib/api";
import {
  isCursorSortable,
  choosePaginationMode,
  parseNextCursor,
  CURSOR_SYSTEM_COLUMNS,
} from "./pagination";

const f = (name: string, fieldType: string, extra: Partial<FieldMeta> = {}): FieldMeta => ({
  name, fieldType, ...extra,
});

const byName = (list: FieldMeta[]): Record<string, FieldMeta> =>
  Object.fromEntries(list.map((x) => [x.name, x]));

describe("isCursorSortable", () => {
  const fields = byName([
    f("name", "text"),                              // JSON-only, not indexed → NOT cursor-sortable
    f("email", "email", { isUnique: true }),        // unique index → sortable
    f("code", "text", { options: { indexed: true } }), // D1 index → sortable
    f("account", "relation", { options: { relatedEntity: "crm_accounts" } }), // FK col → sortable
    f("revenue", "number"),                         // plain number → NOT sortable
  ]);

  it("system columns are always cursor-sortable", () => {
    for (const c of CURSOR_SYSTEM_COLUMNS) {
      expect(isCursorSortable(c, {})).toBe(true);
    }
  });
  it("unique field → sortable", () => {
    expect(isCursorSortable("email", fields)).toBe(true);
  });
  it("indexed (D1) field → sortable", () => {
    expect(isCursorSortable("code", fields)).toBe(true);
  });
  it("relation FK → sortable", () => {
    expect(isCursorSortable("account", fields)).toBe(true);
  });
  it("plain JSON text/number without index → NOT sortable", () => {
    expect(isCursorSortable("name", fields)).toBe(false);
    expect(isCursorSortable("revenue", fields)).toBe(false);
  });
  it("unknown / undefined field → NOT sortable", () => {
    expect(isCursorSortable("ghost", fields)).toBe(false);
    expect(isCursorSortable(undefined, fields)).toBe(false);
  });
});

describe("choosePaginationMode", () => {
  const fields = byName([
    f("name", "text"),
    f("email", "email", { isUnique: true }),
  ]);
  it("no sort → offset (keeps total/count UX)", () => {
    expect(choosePaginationMode(null, fields)).toBe("offset");
    expect(choosePaginationMode(undefined, fields)).toBe("offset");
  });
  it("sort on system column → cursor", () => {
    expect(choosePaginationMode({ field: "created_at", order: "desc" }, fields)).toBe("cursor");
  });
  it("sort on unique field → cursor", () => {
    expect(choosePaginationMode({ field: "email", order: "asc" }, fields)).toBe("cursor");
  });
  it("sort on non-indexed JSON field → offset fallback", () => {
    expect(choosePaginationMode({ field: "name", order: "asc" }, fields)).toBe("offset");
  });
});

describe("parseNextCursor", () => {
  it("extracts a non-empty string cursor", () => {
    expect(parseNextCursor({ limit: 20, nextCursor: "eyJ..." })).toBe("eyJ...");
  });
  it("null cursor (last page) → null", () => {
    expect(parseNextCursor({ limit: 20, nextCursor: null })).toBeNull();
  });
  it("absent cursor (offset envelope) → null", () => {
    expect(parseNextCursor({ page: 1, limit: 20, total: 4, totalPages: 2 })).toBeNull();
  });
  it("empty-string / non-string cursor → null", () => {
    expect(parseNextCursor({ nextCursor: "" })).toBeNull();
    expect(parseNextCursor({ nextCursor: 123 })).toBeNull();
    expect(parseNextCursor(null)).toBeNull();
    expect(parseNextCursor(undefined)).toBeNull();
  });
});
