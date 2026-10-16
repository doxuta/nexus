// specs/048 T131 — where a refused save lands. mapSaveError places a 422 on the record form (each
// before-save rule's sentence under its input when that input is drawn and writable, the rest in
// the form-level notification, verbatim); formatSaveError gives the one sentence a toast / result
// strip / batch row can show. Neither ever shows an error code or a field key.
import { describe, it, expect } from "vitest";
import { ApiError, type FieldMeta } from "@/lib/api";
import { formatSaveError, mapSaveError } from "./saveErrors";

const CLOSE: FieldMeta = { id: "1", name: "close_date", label: "Ngày chốt", fieldType: "date" };
const PROB: FieldMeta = { id: "2", name: "probability", label: "Xác suất", fieldType: "number" };
const NAME: FieldMeta = { id: "3", name: "fullname", label: "Họ tên", fieldType: "fullname" };
const AMOUNT: FieldMeta = { id: "4", name: "amount", label: "Giá trị", fieldType: "currency", readOnly: true };
const OWNER: FieldMeta = { id: "5", name: "notes", label: "Ghi chú nội bộ", fieldType: "text" };

const rendered = [CLOSE, PROB, NAME, AMOUNT];
const all = [...rendered, OWNER];

const veto = (violations: Array<{ field?: string; message: string }>, field?: string) =>
  new ApiError(422, "RECORD_HOOK_VETO", violations[0]?.message ?? "", { ...(field ? { field } : {}), violations });

describe("mapSaveError — RECORD_HOOK_VETO", () => {
  it.each([
    {
      name: "two violations on two drawn inputs ⇒ two field errors, no banner",
      err: veto([
        { field: "close_date", message: "Chốt Thắng phải có Ngày chốt" },
        { field: "probability", message: "Xác suất phải từ 10% khi Đàm phán" },
      ], "close_date"),
      fieldErrors: { close_date: "Chốt Thắng phải có Ngày chốt", probability: "Xác suất phải từ 10% khi Đàm phán" },
      banner: [],
    },
    {
      name: "a violation without a field ⇒ the banner, verbatim",
      err: veto([{ message: "Không sửa được cơ hội đã đóng" }]),
      fieldErrors: {},
      banner: ["Không sửa được cơ hội đã đóng"],
    },
    {
      name: "the field is not on this layout ⇒ the rule's sentence, NOT 'required field missing from the form'",
      err: veto([{ field: "notes", message: "Cần ghi chú khi giảm giá" }], "notes"),
      fieldErrors: {},
      banner: ["Cần ghi chú khi giảm giá"],
    },
    {
      name: "the field is not drawn at all (hidden) ⇒ the banner",
      err: veto([{ field: "stage_reason", message: "Thiếu lý do" }], "stage_reason"),
      fieldErrors: {},
      banner: ["Thiếu lý do"],
    },
    {
      name: "the field is drawn read-only ⇒ the rule's sentence, NOT 'locked for your role'",
      err: veto([{ field: "amount", message: "Giá trị phải lớn hơn 0" }], "amount"),
      fieldErrors: {},
      banner: ["Giá trị phải lớn hơn 0"],
    },
    {
      name: "a composite sub-path anchors on its root input",
      err: veto([{ field: "fullname.last", message: "Họ không được trống" }], "fullname.last"),
      fieldErrors: { fullname: "Họ không được trống" },
      banner: [],
    },
    {
      name: "two violations on the same input ⇒ the first under it, the second kept in the banner",
      err: veto([
        { field: "close_date", message: "Chốt Thắng phải có Ngày chốt" },
        { field: "close_date", message: "Ngày chốt không được ở quá khứ" },
      ], "close_date"),
      fieldErrors: { close_date: "Chốt Thắng phải có Ngày chốt" },
      banner: ["Ngày chốt không được ở quá khứ"],
    },
    {
      name: "a veto without the violations list (older server) ⇒ details.field + message",
      err: new ApiError(422, "RECORD_HOOK_VETO", "Chốt Thắng phải có Ngày chốt", { field: "close_date" }),
      fieldErrors: { close_date: "Chốt Thắng phải có Ngày chốt" },
      banner: [],
    },
  ])("$name", ({ err, fieldErrors, banner }) => {
    expect(mapSaveError(err, rendered, all)).toEqual({ fieldErrors, banner });
  });
});

describe("mapSaveError — everything else keeps the form's old behaviour", () => {
  it("503 RECORD_HOOKS_UNAVAILABLE ⇒ 'busy, retry' in the banner", () => {
    const err = new ApiError(503, "RECORD_HOOKS_UNAVAILABLE", "Hệ thống bận, thử lại sau ít giây");
    expect(mapSaveError(err, rendered, all)).toEqual({ fieldErrors: {}, banner: ["Hệ thống bận, thử lại sau ít giây"] });
  });

  it("RECORD_REQUIRED_MISSING on a drawn input ⇒ under it", () => {
    const err = new ApiError(422, "RECORD_REQUIRED_MISSING", 'field "close_date" là bắt buộc', { field: "close_date" });
    expect(mapSaveError(err, rendered, all)).toEqual({ fieldErrors: { close_date: 'field "close_date" là bắt buộc' }, banner: [] });
  });

  it("RECORD_REQUIRED_MISSING on a field the layout left out ⇒ 'not on this form' with its label", () => {
    const err = new ApiError(422, "RECORD_REQUIRED_MISSING", 'field "notes" là bắt buộc', { field: "notes" });
    const { banner } = mapSaveError(err, rendered, all);
    expect(banner).toHaveLength(1);
    expect(banner[0]).toContain('"Ghi chú nội bộ" chưa có trên form này');
  });

  it("a 422 blaming a read-only drawn field ⇒ 'locked for your role'", () => {
    const err = new ApiError(422, "RECORD_REQUIRED_MISSING", 'field "amount" là bắt buộc', { field: "amount" });
    expect(mapSaveError(err, rendered, all).banner[0]).toMatch(/"Giá trị" đang bị khoá với vai trò của bạn/);
  });

  // The layout's own read-only toggle (LayoutBuilder cell.readonly) also draws a field as text, but
  // the role may write it: blaming the role would send the user to the wrong admin setting.
  it("a 422 blaming a field the LAYOUT draws read-only ⇒ 'read-only on this form', not 'locked for your role'", () => {
    const layoutReadOnly = [{ ...CLOSE, readOnly: true }, PROB, NAME, AMOUNT];
    const err = new ApiError(422, "RECORD_INVALID_DATE", "Ngày không hợp lệ", { field: "close_date" });
    const { fieldErrors, banner } = mapSaveError(err, layoutReadOnly, all);
    expect(fieldErrors).toEqual({});
    expect(banner).toHaveLength(1);
    expect(banner[0]).toContain('"Ngày chốt" chỉ để xem trên form này');
    expect(banner[0]).toContain("Ngày không hợp lệ");
    expect(banner[0]).not.toMatch(/vai trò/);
  });

  it("RECORD_REQUIRED_LOCKED ⇒ the server's sentence alone, even for a drawn field", () => {
    const err = new ApiError(422, "RECORD_REQUIRED_LOCKED", "Trường Giá trị bắt buộc nhưng bị khoá", { field: "close_date" });
    expect(mapSaveError(err, rendered, all)).toEqual({ fieldErrors: {}, banner: ["Trường Giá trị bắt buộc nhưng bị khoá"] });
  });

  it("a network failure ⇒ its message, or the connection line", () => {
    expect(mapSaveError(new TypeError("Failed to fetch"), rendered, all).banner).toEqual(["Failed to fetch"]);
    expect(mapSaveError("boom", rendered, all).banner).toEqual(["Lỗi kết nối — thử lại"]);
  });
});

describe("formatSaveError — one sentence, never a code", () => {
  it.each([
    {
      name: "a veto ApiError ⇒ its first violation",
      err: veto([{ field: "close_date", message: "Chốt Thắng phải có Ngày chốt" }, { message: "Xác suất phải từ 10%" }]),
      want: "Chốt Thắng phải có Ngày chốt",
    },
    {
      name: "a veto batch row ⇒ its first violation",
      err: { id: "r2", ok: false, code: "RECORD_HOOK_VETO", error: "Không xoá cơ hội đã Thắng", details: { violations: [{ message: "Không xoá cơ hội đã Thắng" }] } },
      want: "Không xoá cơ hội đã Thắng",
    },
    {
      name: "a veto row without details ⇒ its error text",
      err: { code: "RECORD_HOOK_VETO", error: "Không xoá cơ hội đã Thắng" },
      want: "Không xoá cơ hội đã Thắng",
    },
    {
      name: "a veto row with no text at all ⇒ a generic Vietnamese line, not the code",
      err: { code: "RECORD_HOOK_VETO" },
      want: "Đã xảy ra lỗi. Vui lòng thử lại.",
    },
    {
      name: "503 ⇒ busy, retry",
      err: new ApiError(503, "RECORD_HOOKS_UNAVAILABLE", ""),
      want: "Hệ thống bận, thử lại sau ít giây.",
    },
    {
      name: "a known code on a row ⇒ its Vietnamese line",
      err: { code: "RECORD_NOT_FOUND", error: "record not found" },
      want: "Bản ghi không tồn tại hoặc đã bị xoá.",
    },
    {
      name: "an unknown code with a server sentence ⇒ the sentence",
      err: { code: "SOMETHING_NEW", error: "Không được phép lúc này" },
      want: "Không được phép lúc này",
    },
    {
      name: "an unknown code with no sentence ⇒ generic, not the code",
      err: { code: "SOMETHING_NEW" },
      want: "Đã xảy ra lỗi. Vui lòng thử lại.",
    },
    { name: "nothing to go on ⇒ generic", err: undefined, want: "Đã xảy ra lỗi. Vui lòng thử lại." },
  ])("$name", ({ err, want }) => {
    const got = formatSaveError(err);
    expect(got).toBe(want);
    expect(got).not.toMatch(/[A-Z]{3,}_[A-Z_]+/);
  });
});
