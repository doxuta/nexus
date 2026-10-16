// picklistRows — unit tests for the W5 picklist row-editor model (pure logic, no DOM).
import { describe, it, expect } from "vitest";
import {
  rowsFromValues, rowsToValues, addRow, removeRow, renameRow, moveRow, duplicateIds, rowsChanged,
} from "./picklistRows";

describe("picklistRows", () => {
  it("round-trips stored values in order with stable unique ids", () => {
    const rows = rowsFromValues(["Mới", "Đang xử lý", "Hoàn tất"]);
    expect(rows.map((r) => r.value)).toEqual(["Mới", "Đang xử lý", "Hoàn tất"]);
    expect(new Set(rows.map((r) => r.id)).size).toBe(3);
    expect(rowsToValues(rows)).toEqual(["Mới", "Đang xử lý", "Hoàn tất"]);
  });

  it("serialize trims whitespace and drops empty rows", () => {
    let rows = rowsFromValues(["A"]);
    rows = addRow(rows, "  B  ");
    rows = addRow(rows); // empty row (just added, not yet typed)
    expect(rowsToValues(rows)).toEqual(["A", "B"]);
  });

  it("rename keeps the row id (input identity survives keystrokes)", () => {
    const rows = rowsFromValues(["A", "B"]);
    const renamed = renameRow(rows, rows[0].id, "A2");
    expect(renamed[0].id).toBe(rows[0].id);
    expect(renamed[0].value).toBe("A2");
    expect(renamed[1]).toBe(rows[1]); // untouched row is the same reference
  });

  it("moveRow lands on the target row's slot (dnd-kit over semantics), both directions", () => {
    const rows = rowsFromValues(["A", "B", "C"]);
    const down = moveRow(rows, rows[0].id, rows[2].id);
    expect(down.map((r) => r.value)).toEqual(["B", "C", "A"]);
    const up = moveRow(rows, rows[2].id, rows[0].id);
    expect(up.map((r) => r.value)).toEqual(["C", "A", "B"]);
  });

  it("moveRow is a no-op for unknown ids or same slot", () => {
    const rows = rowsFromValues(["A", "B"]);
    expect(moveRow(rows, "nope", rows[0].id)).toBe(rows);
    expect(moveRow(rows, rows[0].id, rows[0].id)).toBe(rows);
  });

  it("removeRow deletes exactly one row", () => {
    const rows = rowsFromValues(["A", "B", "C"]);
    const next = removeRow(rows, rows[1].id);
    expect(next.map((r) => r.value)).toEqual(["A", "C"]);
  });

  it("duplicateIds flags later duplicates (trimmed), never the first occurrence", () => {
    let rows = rowsFromValues(["Mới", "Cũ"]);
    rows = addRow(rows, " Mới "); // dupe after trim
    rows = addRow(rows, ""); // empty ignored
    const dupes = duplicateIds(rows);
    expect(dupes.size).toBe(1);
    expect(dupes.has(rows[2].id)).toBe(true);
    expect(dupes.has(rows[0].id)).toBe(false);
  });

  it("rowsChanged compares against the stored contract", () => {
    const rows = rowsFromValues(["A", "B"]);
    expect(rowsChanged(rows, ["A", "B"])).toBe(false);
    expect(rowsChanged(renameRow(rows, rows[0].id, "A2"), ["A", "B"])).toBe(true);
    expect(rowsChanged(addRow(rows), ["A", "B"])).toBe(false); // empty new row = not dirty yet
    expect(rowsChanged(rows, ["B", "A"])).toBe(true); // order matters
  });
});

// ── Tầng nhãn (B-PICKLIST-VN B2) — ô nhập valueLabels trong Studio ───────────────────────────────
//
// Không có bề mặt này thì chỉ PACK có nhãn tiếng Việt (khai trong builtin.go) — admin tự dựng
// entity không có đường đặt nhãn, vi phạm luật FE-phơi-bày-BE. Bất biến của cả thiết kế: sửa NHÃN
// không bao giờ đụng MÃ (duplicateIds/rowsChanged nửa-values vẫn so trên value).
import { rowsToValueLabels, relabelRow } from "./picklistRows";

describe("picklist label layer", () => {
  it("rowsFromValues nhận map nhãn — mỗi row mang nhãn của mã mình", () => {
    const rows = rowsFromValues(["New", "Lost"], { New: "Mới" });
    expect(rows[0].label).toBe("Mới");
    expect(rows[1].label).toBe(""); // mã không nhãn
  });

  it("rowsToValueLabels: chỉ giữ nhãn không rỗng, key theo MÃ đã trim; không nhãn nào → undefined (bỏ hẳn khoá)", () => {
    const rows = rowsFromValues(["New", "Lost"], { New: "Mới" });
    expect(rowsToValueLabels(rows)).toEqual({ New: "Mới" });
    expect(rowsToValueLabels(rowsFromValues(["A", "B"]))).toBeUndefined();
  });

  it("relabelRow đổi nhãn, mã đứng nguyên — và xoá row là nhãn đi theo (authoring không mint nhãn mồ côi)", () => {
    let rows = rowsFromValues(["New"], { New: "Mới" });
    rows = relabelRow(rows, rows[0].id, "Mới toanh");
    expect(rows[0].value).toBe("New");
    expect(rowsToValueLabels(rows)).toEqual({ New: "Mới toanh" });
    expect(rowsToValueLabels(removeRow(rows, rows[0].id))).toBeUndefined();
  });

  it("rowsChanged thấy thay đổi CHỈ-nhãn — thiếu vế này thì sửa nhãn xong nút Lưu vẫn xám", () => {
    const rows = rowsFromValues(["New"], { New: "Mới" });
    expect(rowsChanged(rows, ["New"], { New: "Mới" })).toBe(false);
    expect(rowsChanged(relabelRow(rows, rows[0].id, "Khác"), ["New"], { New: "Mới" })).toBe(true);
    expect(rowsChanged(rows, ["New"])).toBe(true); // stored chưa có nhãn, rows có → dirty
  });
});
