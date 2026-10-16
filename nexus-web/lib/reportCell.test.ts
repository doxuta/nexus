// lib/reportCell — the text a tabular-report cell shows (screen + client CSV fallback).
// BE contract (api-conventions §4.4d): a projected relation/user column arrives resolved by the
// list engine's expand — {id,label} · {id,label,email} · {id} (field-ACL withheld the label) · a
// bare id string (target gone / no fetcher). The cell must show the label a human reads, never
// "[object Object]" and never a 36-char UUID when a name was served.
import { describe, expect, it } from "vitest";
import { csvGuard, reportCellText } from "./reportCell";
import { formatMoney } from "./format";

describe("reportCellText", () => {
  it("shows the resolved label of a relation column, not the UUID", () => {
    expect(reportCellText({ id: "9f3c1e2a-0000-4000-8000-000000000001", label: "Công ty ABC" })).toBe("Công ty ABC");
  });
  it("shows the person's name for a user column, then email, then id", () => {
    expect(reportCellText({ id: "u-1", label: "Nguyễn Văn A", email: "a@example.com" })).toBe("Nguyễn Văn A");
    expect(reportCellText({ id: "u-1", label: "", email: "a@example.com" })).toBe("a@example.com");
    expect(reportCellText({ id: "u-1", label: "", email: "" })).toBe("u-1");
  });
  it("falls back to the id when field-ACL withheld the label ({id} only)", () => {
    expect(reportCellText({ id: "9f3c1e2a-0000-4000-8000-000000000001" })).toBe("9f3c1e2a-0000-4000-8000-000000000001");
  });
  it("keeps a bare id string verbatim (unresolved FK) and scalars as text", () => {
    expect(reportCellText("9f3c1e2a-0000-4000-8000-000000000001")).toBe("9f3c1e2a-0000-4000-8000-000000000001");
    expect(reportCellText("Nháp")).toBe("Nháp");
    expect(reportCellText(12)).toBe("12");
    expect(reportCellText(false)).toBe("false");
  });
  it("renders an empty cell as the dash placeholder", () => {
    expect(reportCellText(null)).toBe("—");
    expect(reportCellText(undefined)).toBe("—");
    expect(reportCellText("")).toBe("—");
  });
  it("formats a currency object instead of printing [object Object]", () => {
    expect(reportCellText({ amount: 5000000, currencyCode: "VND" })).toBe(formatMoney(5000000, "VND"));
  });
  it("joins a multi-select array and never prints [object Object] for other objects", () => {
    expect(reportCellText(["A", "B"])).toBe("A, B");
    expect(reportCellText({ type: "crm_accounts", id: "x" })).not.toContain("[object Object]");
  });
});

// The client-side CSV fallback (server export failed) writes tenant-authored labels straight into
// a file a manager opens in Excel; the server path neutralizes formulas with csvGuard, so the
// fallback must too — RFC 4180 quoting alone does not stop "=HYPERLINK(...)" from executing.
describe("csvGuard", () => {
  it("prefixes a formula-looking cell with a quote", () => {
    expect(csvGuard('=HYPERLINK("http://evil.example","Công ty ABC")')).toBe("'=HYPERLINK(\"http://evil.example\",\"Công ty ABC\")");
    expect(csvGuard("@SUM(A1)")).toBe("'@SUM(A1)");
    expect(csvGuard("\tx")).toBe("'\tx");
    expect(csvGuard("\rx")).toBe("'\rx");
    expect(csvGuard("+cmd|' /c calc'!A1")).toBe("'+cmd|' /c calc'!A1");
    expect(csvGuard("-")).toBe("'-");
  });
  it("leaves plain text, numbers and signed numbers untouched", () => {
    expect(csvGuard("Công ty ABC")).toBe("Công ty ABC");
    expect(csvGuard("")).toBe("");
    expect(csvGuard("-100")).toBe("-100");
    expect(csvGuard("+3.5")).toBe("+3.5");
  });
});
