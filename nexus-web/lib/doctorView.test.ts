// specs/079 f7-kham-ve-lai T009: the pure view logic of the health-check checklist.
import { describe, expect, it } from "vitest";
import type { DoctorFinding, DoctorScanData } from "@/lib/api";
import { cleanRow, deliveredAdvice, doctorLede, findingRows, fixableIds } from "@/lib/doctorView";

const f = (over: Partial<DoctorFinding> & { checkId: string }): DoctorFinding => ({
  severity: "warn", autoFixSafe: true, count: 1, items: [], ...over,
});
// 09:30 in the viewer's zone, whatever it is.
const AT = new Date(2026, 9, 14, 9, 30).toISOString();
const scan = (findings: DoctorFinding[]): DoctorScanData => ({ findings, scannedAt: AT });

describe("doctorLede", () => {
  it("says so when nothing was checked in this session", () => {
    expect(doctorLede(null)).toBe("Chưa khám trong lần mở này.");
  });
  it("clean scan: one sentence, no mismatch", () => {
    expect(doctorLede(scan([f({ checkId: "B1", count: 0 })]))).toBe("Khám lúc 09:30: **không phát hiện lệch nào**.");
  });
  it("drift: counts per severity, zero levels left out", () => {
    const s = scan([f({ checkId: "C1", severity: "critical" }), f({ checkId: "B1" }), f({ checkId: "B2" }), f({ checkId: "C2", severity: "info", count: 0 })]);
    expect(doctorLede(s)).toBe("Khám lúc 09:30: **1 lỗi**, **2 cảnh báo**.");
  });
});

describe("findingRows", () => {
  it("orders critical > warn > info, then by count desc; drops count 0; keeps flags", () => {
    const rows = findingRows([
      f({ checkId: "B1", severity: "warn", count: 2 }),
      f({ checkId: "I1", severity: "info", count: 9 }),
      f({ checkId: "B2", severity: "warn", count: 5, reportOnly: true, truncated: true, autoFixSafe: false }),
      f({ checkId: "C1", severity: "critical", count: 1 }),
      f({ checkId: "Z", severity: "critical", count: 0 }),
    ]);
    expect(rows.map((r) => r.checkId)).toEqual(["C1", "B2", "B1", "I1"]);
    expect(rows[1]).toMatchObject({ reportOnly: true, truncated: true, autoFixSafe: false });
  });
});

describe("fixableIds", () => {
  it("only autoFixSafe, not reportOnly, count > 0", () => {
    expect(fixableIds([
      f({ checkId: "A" }), f({ checkId: "B", autoFixSafe: false }), f({ checkId: "C", reportOnly: true }), f({ checkId: "D", count: 0 }),
    ])).toEqual(["A"]);
  });
});

describe("deliveredAdvice", () => {
  it("fails closed: delivered and unknown warn; not-delivered and no-concept stay quiet", () => {
    expect(deliveredAdvice("delivered")).toMatch(/áp ngay cho người dùng thật/);
    expect(deliveredAdvice("unknown")).toMatch(/áp ngay cho người dùng thật/);
    expect(deliveredAdvice("not-delivered")).toBe("");
    expect(deliveredAdvice("no-concept")).toBe("");
  });
});

describe("cleanRow", () => {
  it("clean ⇒ one sentence with the number of checks; drift ⇒ null", () => {
    expect(cleanRow(scan([]), 18)).toBe("Đã khám 18 khoản, khoản nào cũng khớp.");
    expect(cleanRow(scan([f({ checkId: "B1", count: 0 })]), 18)).toBe("Đã khám 18 khoản, khoản nào cũng khớp.");
    expect(cleanRow(scan([f({ checkId: "B1" })]), 18)).toBeNull();
  });
});
