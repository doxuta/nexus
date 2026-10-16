import { describe, it, expect } from "vitest";
import { recordLabel, type LabelableRecord } from "./recordLabel";
import type { FieldMeta } from "@/lib/api";

// Helper: build a FieldMeta with just the bits recordLabel cares about.
const f = (name: string, fieldType = "text"): FieldMeta => ({ name, fieldType });

const rec = (id: string, data: Record<string, unknown>): LabelableRecord => ({ id, data });

describe("recordLabel — VIP heuristic (§5bis)", () => {
  // ── Tier 1: preferred name fields, ranked ──
  it("tier1: uses `name` when present", () => {
    expect(
      recordLabel(rec("abc12345-xxxx", { name: "VietBuild JSC", email: "a@b.vn" }), [
        f("name"),
        f("email", "email"),
      ]),
    ).toBe("VietBuild JSC");
  });

  it("tier1: respects priority order (title beats subject/code/email)", () => {
    expect(
      recordLabel(rec("id1", { subject: "Sub", title: "The Title", code: "C1" }), [
        f("subject"),
        f("title"),
        f("code"),
      ]),
    ).toBe("The Title");
  });

  it("tier1: case-insensitive field-name match (Full_Name)", () => {
    expect(
      recordLabel(rec("id1", { Full_Name: "Nguyễn Văn A" }), [f("Full_Name")]),
    ).toBe("Nguyễn Văn A");
  });

  it("tier1: skips an empty preferred field and falls to the next", () => {
    expect(
      recordLabel(rec("id1", { name: "   ", title: "Backup Title" }), [
        f("name"),
        f("title"),
      ]),
    ).toBe("Backup Title");
  });

  it("tier1: subject (ticket entity) wins when no higher-rank field", () => {
    expect(
      recordLabel(rec("t1", { subject: "Không đăng nhập được", priority: "high" }), [
        f("subject"),
        f("priority", "picklist"),
      ]),
    ).toBe("Không đăng nhập được");
  });

  // ── Tier 2: first text field by name ASC ──
  it("tier2: first text field by name ASC when no preferred field", () => {
    expect(
      recordLabel(rec("id1", { zeta: "Z", alpha: "A-value", note: 5 }), [
        f("zeta"),
        f("alpha"),
        f("note", "number"),
      ]),
    ).toBe("A-value");
  });

  it("tier2: ignores non-text fields", () => {
    expect(
      recordLabel(rec("id1", { amount: 1000, descr: "Hello" }), [
        f("amount", "number"),
        f("descr"),
      ]),
    ).toBe("Hello");
  });

  // ── Tier 3: join two short fields ──
  // To exercise tier3 the name fields must NOT be plain `text` (otherwise tier2 grabs the first
  // one). Real-world trigger: composite sub-parts or non-text fields with no preferred-name field.
  it("tier3: joins first_name + last_name (non-text → tier2 skips, tier3 joins)", () => {
    expect(
      recordLabel(rec("id1", { first_name: "An", last_name: "Nguyễn" }), [
        f("first_name", "composite"),
        f("last_name", "composite"),
      ]),
    ).toBe("An Nguyễn");
  });

  it("tier3: does NOT join when one half is empty → falls through to #<id>", () => {
    expect(
      recordLabel(rec("abcd1234-rest", { first_name: "An", last_name: "" }), [
        f("first_name", "composite"),
        f("last_name", "composite"),
      ]),
    ).toBe("#abcd1234");
  });

  // ── Tier 4: fallback to #<short id> ──
  it("tier4: falls back to #<8-char id> when no usable field", () => {
    expect(
      recordLabel(rec("ce3fcd24-1234-5678-9abc-def012345678", { ignored: 42 }), [
        f("ignored", "number"),
      ]),
    ).toBe("#ce3fcd24");
  });

  it("tier4: empty-safe when no id and no fields", () => {
    expect(recordLabel(rec("", {}), [])).toBe("(chưa đặt tên)");
  });

  // ── Formatting ──
  it("format: strips HTML tags", () => {
    expect(
      recordLabel(rec("id1", { name: "<b>Bold</b> name" }), [f("name")]),
    ).toBe("Bold name");
  });

  it("format: truncates long labels with an ellipsis (<= maxLength)", () => {
    const long = "x".repeat(80);
    const out = recordLabel(rec("id1", { name: long }), [f("name")], { maxLength: 20 });
    expect(out.length).toBeLessThanOrEqual(20);
    expect(out.endsWith("…")).toBe(true);
  });

  // ── Tier 0: explicit labelField override ──
  it("tier0: explicit labelField wins over the heuristic", () => {
    expect(
      recordLabel(rec("id1", { name: "Heuristic", custom_label: "Explicit" }), [
        f("name"),
        f("custom_label"),
      ], { labelField: "custom_label" }),
    ).toBe("Explicit");
  });

  // ── Robustness: no metadata at all (data-only inference) ──
  it("works with empty fields[] by reading preferred keys straight off data", () => {
    expect(recordLabel(rec("id1", { name: "Just Data" }), [])).toBe("Just Data");
  });

  // ── Relation ref value ({id,label}) ──
  it("reads a label from an expanded relation ref value", () => {
    expect(
      recordLabel(rec("id1", { name: { id: "x", label: "Acme Corp" } }), [f("name", "relation")]),
    ).toBe("Acme Corp");
  });

  // ── DP-2.1 ref-guard: id-shaped values are references, never labels ──
  it("refuses a uuid stored in a text field (falls through to #shortid)", () => {
    expect(
      recordLabel(
        rec("deadbeef-1111-2222-3333-444455556666", {
          assigned_advisor: "b1551a1a-5b97-4640-91b5-f49d13bb118f",
        }),
        [f("assigned_advisor")],
      ),
    ).toBe("#deadbeef");
  });

  it("refuses a uuid even via the preferred-name tier", () => {
    expect(
      recordLabel(rec("aaaabbbb-0000-0000-0000-000000000000", {
        name: "b1551a1a-5b97-4640-91b5-f49d13bb118f",
      }), [f("name")]),
    ).toBe("#aaaabbbb");
  });

  // ── DP-2.1 tier 2.5: join-entity labels itself by its expanded endpoints ──
  it("joins two expanded relation labels for a pure join entity", () => {
    expect(
      recordLabel(
        rec("id1", {
          student: { id: "s1", label: "Trần Thị Mai" },
          course: { id: "c1", label: "Digital Marketing" },
          grade: 8.7,
        }),
        [f("student", "relation"), f("course", "relation"), f("grade", "number")],
      ),
    ).toBe("Trần Thị Mai — Digital Marketing");
  });

  it("uses a single expanded relation label when only one is expanded", () => {
    expect(
      recordLabel(
        rec("id1", {
          student: { id: "s1", label: "Trần Thị Mai" },
          course: "c1-raw-id-not-expanded",
        }),
        [f("student", "relation"), f("course", "relation")],
      ),
    ).toBe("Trần Thị Mai");
  });

  it("unexpanded relations (raw id strings) still fall through to #shortid", () => {
    expect(
      recordLabel(
        rec("cafebabe-0000-0000-0000-000000000000", {
          student: "a3871ec5-d272-4e79-a657-58269959052c",
          course: "97cd7ed6-206e-464c-95cb-4e250da25faf",
        }),
        [f("student", "relation"), f("course", "relation")],
      ),
    ).toBe("#cafebabe");
  });

  it("a real text name still beats the relation-label tier", () => {
    expect(
      recordLabel(
        rec("id1", {
          title: "Ghi danh khoá Marketing",
          student: { id: "s1", label: "Trần Thị Mai" },
        }),
        [f("title"), f("student", "relation")],
      ),
    ).toBe("Ghi danh khoá Marketing");
  });
});

describe("recordLabel — the table's own label field (#272)", () => {
  it("isLabel beats the English-name heuristic: a VN table titled by its chosen column", () => {
    const fields: FieldMeta[] = [
      { name: "title", fieldType: "text" }, // would win tier 1
      { name: "ho_ten", fieldType: "text", isLabel: true },
    ];
    expect(recordLabel(rec("171347a0-aaaa", { title: "Khám tổng quát", ho_ten: "Nguyễn Thị Hoa" }), fields)).toBe("Nguyễn Thị Hoa");
  });

  it("#479: a relation's own labelField beats an INFERRED isLabel, but the table's explicit choice beats the relation", () => {
    const data = rec("id1", { ma: "BN-01", ho_ten: "Nguyễn Văn An" });
    const inferred: FieldMeta[] = [{ name: "ma", fieldType: "text" }, { name: "ho_ten", fieldType: "text", isLabel: true }];
    expect(recordLabel(data, inferred, { labelField: "ma" })).toBe("BN-01");
    const explicit: FieldMeta[] = [{ name: "ma", fieldType: "text" }, { name: "ho_ten", fieldType: "text", isLabel: true, isLabelExplicit: true }];
    expect(recordLabel(data, explicit, { labelField: "ma" })).toBe("Nguyễn Văn An");
  });

  it("an empty isLabel value falls through to the heuristic instead of showing nothing", () => {
    const fields: FieldMeta[] = [{ name: "so_gd", fieldType: "auto_number", isLabel: true }, { name: "title", fieldType: "text" }];
    expect(recordLabel(rec("id1", { so_gd: "", title: "Giao dịch A" }), fields)).toBe("Giao dịch A");
  });
});
