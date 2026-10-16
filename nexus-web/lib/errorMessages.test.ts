// Tests for lib/errorMessages.ts — error-code → Vietnamese message resolution.

import { describe, it, expect } from "vitest";
import { errorMessage, errorCode } from "@/lib/errorMessages";
import { ApiError } from "@/lib/api";

function makeApiError(code: string | undefined, message = "raw server message"): ApiError {
  // Cast to string to satisfy constructor signature (ApiError.code/message are non-optional at TS level
  // but runtime values may be undefined when backend omits them).
  return new ApiError(400, code as string, message as string);
}

describe("errorMessage()", () => {
  it("maps a known code to the Vietnamese string", () => {
    const e = makeApiError("INVALID_CREDENTIALS");
    expect(errorMessage(e)).toBe("Thông tin đăng nhập không đúng.");
  });

  it("maps RECORD_NOT_FOUND correctly", () => {
    const e = makeApiError("RECORD_NOT_FOUND");
    expect(errorMessage(e)).toBe("Bản ghi không tồn tại hoặc đã bị xoá.");
  });

  it("maps FORBIDDEN correctly", () => {
    const e = makeApiError("FORBIDDEN");
    expect(errorMessage(e)).toBe("Bạn không có quyền thực hiện thao tác này.");
  });

  // queue #62: "Tạo nhiều dòng" shows each failed row through errorMessage(). The server's
  // RECORD_REQUIRED_LOCKED sentence names the field (it has no column on that grid) and the admin's
  // type-aware way out; a generic mapping here would collapse it back to "Thiếu thông tin bắt buộc."
  it("leaves RECORD_REQUIRED_LOCKED unmapped so the row shows the server's field + way out", () => {
    const e = makeApiError("RECORD_REQUIRED_LOCKED", 'Trường "Giá trị" bắt buộc nhưng vai trò của bạn không được ghi trường này');
    expect(errorMessage(e)).toBe('Trường "Giá trị" bắt buộc nhưng vai trò của bạn không được ghi trường này');
  });

  // queue #317: a hidden column and a missing column are the same refusal, so the screen must say
  // one sentence for both — never the server's text, which echoes the probed name.
  it("answers a filter/sort on a hidden-or-missing column with one sentence that names neither", () => {
    for (const code of ["LIST_UNKNOWN_FILTER", "SORT_NOT_ALLOWED"]) {
      const msg = errorMessage(makeApiError(code, 'không thể lọc theo field "amount" (không có trong metadata)'));
      expect(msg).not.toContain("amount");
      expect(msg).not.toContain("metadata");
    }
  });

  it("falls back to err.message when code is unknown", () => {
    const e = makeApiError("SOME_UNKNOWN_CODE", "server says something");
    expect(errorMessage(e)).toBe("server says something");
  });

  it("falls back to generic message when ApiError has no code and no message", () => {
    // ApiError constructor is typed as (status, code: string, message: string) but
    // runtime values may be undefined when backend omits fields. Cast to test the branch.
    const e = new ApiError(500, undefined as unknown as string, undefined as unknown as string);
    expect(errorMessage(e)).toBe("Đã xảy ra lỗi. Vui lòng thử lại.");
  });

  it("handles a plain Error (not ApiError)", () => {
    expect(errorMessage(new Error("network error"))).toBe("network error");
  });

  it("handles a string thrown (not an Error instance)", () => {
    expect(errorMessage("something weird")).toBe("Đã xảy ra lỗi. Vui lòng thử lại.");
  });

  it("handles null/undefined", () => {
    expect(errorMessage(null)).toBe("Đã xảy ra lỗi. Vui lòng thử lại.");
    expect(errorMessage(undefined)).toBe("Đã xảy ra lỗi. Vui lòng thử lại.");
  });
});

describe("errorCode()", () => {
  it("extracts the code string from ApiError", () => {
    const e = makeApiError("RECORD_DUPLICATE");
    expect(errorCode(e)).toBe("RECORD_DUPLICATE");
  });

  it("returns null when ApiError has no code", () => {
    const e = new ApiError(500, undefined as unknown as string, "oops");
    expect(errorCode(e)).toBeNull();
  });

  it("returns null for plain Error", () => {
    expect(errorCode(new Error("plain"))).toBeNull();
  });
});
