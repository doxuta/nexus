import { describe, it, expect } from "vitest";
import { fieldZodType, buildLayoutSchema } from "./layoutSchema";
import type { FieldMeta } from "@/lib/api";

// Helper builder for a FieldMeta.
const f = (over: Partial<FieldMeta> & { name: string; fieldType: string }): FieldMeta => ({
  ...over,
});

// Run a single-field schema and report success + first error message.
function check(field: FieldMeta, value: unknown): { ok: boolean; msg?: string } {
  const schema = fieldZodType(field);
  const r = schema.safeParse(value);
  return r.success ? { ok: true } : { ok: false, msg: r.error.issues[0]?.message };
}

describe("layoutSchema — dynamic zod from field metadata (§4.1)", () => {
  // ── required (text) ──
  it("required text: rejects empty, accepts non-empty", () => {
    const field = f({ name: "name", fieldType: "text", isRequired: true });
    expect(check(field, "").ok).toBe(false);
    expect(check(field, "  ").ok).toBe(false); // trimmed → empty
    expect(check(field, "Acme").ok).toBe(true);
  });

  it("optional text: accepts empty string", () => {
    const field = f({ name: "note", fieldType: "text" });
    expect(check(field, "").ok).toBe(true);
  });

  // ── email ──
  it("email: rejects malformed, accepts valid", () => {
    const field = f({ name: "email", fieldType: "email", isRequired: true });
    expect(check(field, "not-an-email").ok).toBe(false);
    expect(check(field, "a@b.vn").ok).toBe(true);
  });

  it("optional email: accepts empty but rejects malformed", () => {
    const field = f({ name: "email", fieldType: "email" });
    expect(check(field, "").ok).toBe(true);
    expect(check(field, "bad").ok).toBe(false);
  });

  // ── number (coerce) + min/max ──
  it("number: coerces string, enforces min/max", () => {
    const field = f({ name: "qty", fieldType: "number", isRequired: true, options: { min: 1, max: 10 } });
    expect(check(field, "5").ok).toBe(true); // string coerced
    expect(check(field, 0).ok).toBe(false); // below min
    expect(check(field, 11).ok).toBe(false); // above max
  });

  it("optional number: empty string → valid (undefined)", () => {
    const field = f({ name: "qty", fieldType: "number" });
    expect(check(field, "").ok).toBe(true);
  });

  it("number: rejects non-numeric text", () => {
    const field = f({ name: "qty", fieldType: "number", isRequired: true });
    expect(check(field, "abc").ok).toBe(false);
  });

  // ── currency (number-like) ──
  it("currency: coerces string amount", () => {
    const field = f({ name: "amount", fieldType: "currency", isRequired: true });
    expect(check(field, "1000000").ok).toBe(true);
  });

  // ── boolean ──
  it("boolean: accepts true/false", () => {
    const field = f({ name: "active", fieldType: "boolean" });
    expect(check(field, true).ok).toBe(true);
    expect(check(field, false).ok).toBe(true);
  });

  // ── picklist (enum) ──
  it("picklist with values: rejects out-of-set, accepts in-set", () => {
    const field = f({ name: "priority", fieldType: "picklist", isRequired: true, options: { values: ["high", "medium", "low"] } });
    expect(check(field, "high").ok).toBe(true);
    expect(check(field, "urgent").ok).toBe(false);
  });

  // The form shows this message under the field: zod's default "Invalid input" is English
  // (seen on the SLDS form, 2026-09-29) — the customer face is Vietnamese only.
  it("required picklist left empty: Vietnamese message, not zod's English default", () => {
    const field = f({ name: "stage", fieldType: "picklist", isRequired: true, options: { values: ["New", "Won"] } });
    expect(check(field, "")).toEqual({ ok: false, msg: "Chọn một giá trị" });
  });

  it("optional picklist: empty allowed", () => {
    const field = f({ name: "priority", fieldType: "picklist", options: { values: ["high", "low"] } });
    expect(check(field, "").ok).toBe(true);
  });

  // ── multi_select ──
  it("required multi_select: rejects empty array, accepts non-empty", () => {
    const field = f({ name: "tags", fieldType: "multi_select", isRequired: true });
    expect(check(field, []).ok).toBe(false);
    expect(check(field, ["a"]).ok).toBe(true);
  });

  // ── relation ──
  it("required relation: rejects empty id", () => {
    const field = f({ name: "account", fieldType: "relation", isRequired: true });
    expect(check(field, "").ok).toBe(false);
    expect(check(field, "ce3fcd24").ok).toBe(true);
  });

  // ── polymorphic_relation ──
  it("optional polymorphic_relation: accepts null/'' (unset) and a full {type,id}", () => {
    const field = f({ name: "related_to", fieldType: "polymorphic_relation" });
    expect(check(field, null).ok).toBe(true);
    expect(check(field, "").ok).toBe(true);
    expect(check(field, { type: "crm_accounts", id: "a1" }).ok).toBe(true);
  });
  it("required polymorphic_relation: rejects unset + partial, accepts both-present", () => {
    const field = f({ name: "related_to", fieldType: "polymorphic_relation", isRequired: true });
    expect(check(field, null).ok).toBe(false);
    expect(check(field, { type: "crm_accounts", id: "" }).ok).toBe(false);
    expect(check(field, { type: "", id: "a1" }).ok).toBe(false);
    expect(check(field, { type: "crm_accounts", id: "a1" }).ok).toBe(true);
  });

  // ── rating ──
  it("required rating: rejects 0, accepts >=1", () => {
    const field = f({ name: "stars", fieldType: "rating", isRequired: true, options: { max: 5 } });
    expect(check(field, 0).ok).toBe(false);
    expect(check(field, 4).ok).toBe(true);
  });

  // ── text maxLength + pattern ──
  it("text maxLength: rejects over-long", () => {
    const field = f({ name: "code", fieldType: "text", options: { maxLength: 5 } });
    expect(check(field, "12345").ok).toBe(true);
    expect(check(field, "123456").ok).toBe(false);
  });

  it("text pattern: enforces regex", () => {
    const field = f({ name: "code", fieldType: "text", isRequired: true, options: { pattern: "^[A-Z]{3}$" } });
    expect(check(field, "ABC").ok).toBe(true);
    expect(check(field, "abc").ok).toBe(false);
  });

  it("text pattern: invalid stored regex is skipped (no crash)", () => {
    const field = f({ name: "code", fieldType: "text", options: { pattern: "([" } });
    expect(check(field, "anything").ok).toBe(true);
  });

  // ── date ──
  it("required date: rejects empty", () => {
    const field = f({ name: "due", fieldType: "date", isRequired: true });
    expect(check(field, "").ok).toBe(false);
    expect(check(field, "01/02/2026").ok).toBe(true);
  });

  // ── unknown type fallback ──
  it("unknown type: permissive (forward-compat)", () => {
    const field = f({ name: "x", fieldType: "some_future_type" });
    expect(check(field, { whatever: 1 }).ok).toBe(true);
  });

  // ── buildLayoutSchema assembles an object ──
  it("buildLayoutSchema: validates a multi-field record", () => {
    const fields: FieldMeta[] = [
      f({ name: "name", fieldType: "text", isRequired: true }),
      f({ name: "email", fieldType: "email" }),
      f({ name: "priority", fieldType: "picklist", options: { values: ["high", "low"] } }),
    ];
    const schema = buildLayoutSchema(fields);
    expect(schema.safeParse({ name: "Acme", email: "a@b.vn", priority: "high" }).success).toBe(true);
    expect(schema.safeParse({ name: "", email: "a@b.vn", priority: "high" }).success).toBe(false);
    expect(schema.safeParse({ name: "Acme", email: "bad", priority: "high" }).success).toBe(false);
  });

  it("buildLayoutSchema: maps the failing field name in issues path", () => {
    const schema = buildLayoutSchema([f({ name: "subject", fieldType: "text", isRequired: true })]);
    const r = schema.safeParse({ subject: "" });
    expect(r.success).toBe(false);
    if (!r.success) expect(r.error.issues[0].path).toEqual(["subject"]);
  });

  // specs/060 (#270): the form has no input for file/image (the "Tệp" tab owns them), so the schema
  // must never block a save on them — a required image that already holds its [{id,name}] refs was
  // refused as "bắt buộc" under a static note the user cannot fix.
  it("file/image: không kiểm — ảnh/tệp bắt buộc đã có tham chiếu không chặn lưu", () => {
    const refs = [{ id: "f1", name: "anh.png" }];
    expect(check(f({ name: "anh", fieldType: "image", isRequired: true }), refs).ok).toBe(true);
    expect(check(f({ name: "chu_ky", fieldType: "file", isRequired: true }), refs).ok).toBe(true);
  });
});

describe("required number — empty is not 0 (#275)", () => {
  it.each(["number", "currency"])("%s required rejects '' and null, accepts 0", (fieldType) => {
    const fld = f({ name: "n", fieldType, isRequired: true });
    expect(check(fld, "").ok).toBe(false);
    expect(check(fld, null).ok).toBe(false);
    expect(check(fld, 0).ok).toBe(true);
    expect(check(fld, "5").ok).toBe(true);
  });
});
