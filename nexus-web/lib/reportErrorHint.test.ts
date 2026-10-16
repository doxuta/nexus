// Tests for lib/reportErrorHint — the sentence a failed report run / export shows (specs/049 F0,
// FR-024). One hint per error code; the "đánh index" advice only for the real index error; staff
// (non-admin) get a plain Vietnamese sentence and never a machine code or a config-key token.
import { describe, expect, it } from "vitest";
import { reportErrorHint } from "./reportErrorHint";

const ENGINE_TOKENS = /REPORT_|FIELD_FORBIDDEN|AGGREGATE_|subGroups|columnGroup|groupBy|having/;

describe("reportErrorHint", () => {
  it("FORBIDDEN: the permission sentence, no index advice", () => {
    const h = reportErrorHint("FORBIDDEN", {}, false);
    expect(h.body).toMatch(/không có quyền xem/);
    expect(h.body).toMatch(/đề nghị quản trị viên cấp quyền xem/);
    expect(h.body).not.toMatch(/index/);
    expect(h.showCode).toBe(false);
  });

  it("FIELD_FORBIDDEN: a hidden field, said without naming the axis to staff", () => {
    const h = reportErrorHint("FIELD_FORBIDDEN", { axis: "subGroups[0]" }, false);
    expect(h.body).toMatch(/trường bạn không được xem/);
    expect(h.body).not.toMatch(/cấp nhóm/);
    expect(h.body).not.toMatch(ENGINE_TOKENS);
  });

  it("LIST_UNKNOWN_FILTER / SORT_NOT_ALLOWED (hidden-field filter, queue #317) read as the hidden-field sentence", () => {
    for (const code of ["LIST_UNKNOWN_FILTER", "SORT_NOT_ALLOWED"]) {
      const h = reportErrorHint(code, {}, false);
      expect(h.body).toMatch(/trường bạn không được xem/);
      expect(h.body).not.toMatch(ENGINE_TOKENS);
    }
  });

  it("REPORT_MATRIX_TOO_MANY_CELLS with rows × columns", () => {
    const h = reportErrorHint("REPORT_MATRIX_TOO_MANY_CELLS", { cap: 1000, cells: 1100, rows: 11, columns: 100, lowerBound: false }, false);
    expect(h.title).toBe("Bảng chéo vượt giới hạn ô");
    expect(h.body).toContain("Bảng chéo có 11 hàng × 100 cột = 1.100 ô, vượt giới hạn 1.000 ô.");
    expect(h.body).toMatch(/thu hẹp/);
    expect(h.body).not.toMatch(/index/);
  });

  it("REPORT_MATRIX_TOO_MANY_CELLS without rows/columns: a lower bound reads 'ít nhất'", () => {
    expect(reportErrorHint("REPORT_MATRIX_TOO_MANY_CELLS", { cap: 1000, cells: 1001, lowerBound: true }, false).body)
      .toContain("Bảng chéo có ít nhất 1.001 ô, vượt giới hạn 1.000 ô.");
    expect(reportErrorHint("REPORT_MATRIX_TOO_MANY_CELLS", { cap: 1000, cells: 1200, lowerBound: false }, false).body)
      .toContain("Bảng chéo có 1.200 ô, vượt giới hạn 1.000 ô.");
    // No numbers at all → still the right sentence, no "undefined".
    const bare = reportErrorHint("REPORT_MATRIX_TOO_MANY_CELLS", {}, false).body;
    expect(bare).toContain("Bảng chéo có quá nhiều ô để hiển thị.");
    expect(bare).not.toMatch(/undefined|NaN/);
  });

  it("REPORT_TOO_MANY_GROUPS", () => {
    const h = reportErrorHint("REPORT_TOO_MANY_GROUPS", { cap: 1000, groups: 1001, lowerBound: true }, false);
    expect(h.title).toBe("Báo cáo vượt giới hạn nhóm");
    expect(h.body).toContain("Báo cáo có ít nhất 1.001 nhóm, vượt giới hạn 1.000 nhóm.");
    expect(reportErrorHint("REPORT_TOO_MANY_GROUPS", { cap: 1000, groups: 1500 }, false).body)
      .toContain("Báo cáo có 1.500 nhóm, vượt giới hạn 1.000 nhóm.");
  });

  it("REPORT_HAVING_INVALID: admin fixes it in Cấu hình, staff asks the admin", () => {
    const admin = reportErrorHint("REPORT_HAVING_INVALID", { field: "having", reason: "op" }, true);
    const staff = reportErrorHint("REPORT_HAVING_INVALID", { field: "having", reason: "op" }, false);
    expect(admin.title).toBe("Ngưỡng lọc không hợp lệ");
    expect(admin.body).toMatch(/Cấu hình/);
    expect(staff.body).toMatch(/Liên hệ quản trị viên/);
    expect(staff.body).not.toMatch(ENGINE_TOKENS);
  });

  it("REPORT_EXPORT_GRID_UNSUPPORTED: the grid cannot be exported yet — no promise, no index advice", () => {
    const h = reportErrorHint("REPORT_EXPORT_GRID_UNSUPPORTED", {}, false);
    expect(h.body).toMatch(/chưa xuất CSV được/);
    expect(h.body).not.toMatch(/index/);
  });

  it("AGGREGATE_REQUIRES_INDEXED: the index advice, worded per role", () => {
    const admin = reportErrorHint("AGGREGATE_REQUIRES_INDEXED", {}, true);
    const staff = reportErrorHint("AGGREGATE_REQUIRES_INDEXED", {}, false);
    expect(admin.body).toMatch(/cần được đánh index/);
    expect(admin.body).toMatch(/Mô hình dữ liệu/);
    expect(staff.body).toMatch(/cần được đánh index/);
    expect(staff.body).toMatch(/Liên hệ quản trị viên để cập nhật/);
    expect(staff.body).not.toMatch(/Mô hình dữ liệu/);
  });

  it("an unknown code: the generic sentence, NEVER the index advice", () => {
    for (const code of ["RECORD_UNKNOWN_FIELD", "UNKNOWN", "", null, undefined]) {
      const h = reportErrorHint(code, {}, false);
      expect(h.title).toBe("Báo cáo không chạy được");
      expect(h.body).not.toMatch(/index/);
      expect(h.body.length).toBeGreaterThan(0);
    }
  });

  it("details.axis → a Vietnamese label, for the admin only", () => {
    expect(reportErrorHint("FIELD_FORBIDDEN", { axis: "subGroups[1]" }, true).body).toContain("cấp nhóm thứ 3");
    expect(reportErrorHint("FIELD_FORBIDDEN", { axis: "subGroups[0]" }, true).body).toContain("cấp nhóm thứ 2");
    expect(reportErrorHint("AGGREGATE_REQUIRES_INDEXED", { axis: "columnGroup" }, true).body).toContain("trục cột");
    expect(reportErrorHint("AGGREGATE_REQUIRES_INDEXED", { axis: "groupBy" }, true).body).toContain("cấp nhóm thứ 1");
    // An axis token we cannot name is dropped, never echoed.
    const odd = reportErrorHint("FIELD_FORBIDDEN", { axis: "weird[x]" }, true).body;
    expect(odd).not.toContain("weird");
    for (const axis of ["subGroups[1]", "columnGroup", "groupBy"]) {
      const staff = reportErrorHint("AGGREGATE_REQUIRES_INDEXED", { axis }, false).body;
      expect(staff).not.toMatch(/trục cột|cấp nhóm thứ/);
      expect(staff).not.toMatch(ENGINE_TOKENS);
    }
  });

  it("showCode: admin keeps the machine code for diagnosis, staff never", () => {
    expect(reportErrorHint("REPORT_HAVING_INVALID", {}, true).showCode).toBe(true);
    expect(reportErrorHint("REPORT_HAVING_INVALID", {}, false).showCode).toBe(false);
  });
});
