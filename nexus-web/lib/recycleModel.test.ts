// recycleModel — pure-logic unit tests for the W9c recycle-bin restore flow.
// Covers: row removal after restore, the drop-on-404 rule, and the honest error→message mapping
// (409 unique collision names the human field label; 403/404 mapped distinctly).

import { describe, it, expect } from "vitest";
import { ApiError } from "./api";
import { removeById, shouldDropOnError, restoreErrorMessage } from "./recycleModel";

const label = (name: string) => (name === "email" ? "Email" : name);

describe("removeById", () => {
  it("removes the matching row, leaves the rest in order", () => {
    const rows = [{ id: "a" }, { id: "b" }, { id: "c" }];
    expect(removeById(rows, "b")).toEqual([{ id: "a" }, { id: "c" }]);
  });
  it("is a no-op when the id is absent", () => {
    const rows = [{ id: "a" }];
    expect(removeById(rows, "z")).toEqual([{ id: "a" }]);
  });
});

describe("shouldDropOnError", () => {
  it("true for 404 / RECORD_NOT_FOUND (row is no longer soft-deleted)", () => {
    expect(shouldDropOnError(new ApiError(404, "RECORD_NOT_FOUND", "x"))).toBe(true);
  });
  it("false for a 409 collision (row stays — user can fix the conflict)", () => {
    expect(shouldDropOnError(new ApiError(409, "RECORD_DUPLICATE", "x", { field: "email" }))).toBe(false);
  });
  it("false for a non-ApiError", () => {
    expect(shouldDropOnError(new Error("network"))).toBe(false);
  });
});

describe("restoreErrorMessage", () => {
  it("409 collision names the field LABEL, not the raw column", () => {
    const msg = restoreErrorMessage(new ApiError(409, "RECORD_DUPLICATE", "dup", { field: "email" }), label);
    expect(msg).toContain("Email");
    expect(msg).toContain("bản ghi khác");
  });

  it("409 without details.field falls back to a generic unique message", () => {
    const msg = restoreErrorMessage(new ApiError(409, "RECORD_DUPLICATE", "dup"), label);
    expect(msg).toContain("giá trị duy nhất");
  });

  it("403 → permission message", () => {
    expect(restoreErrorMessage(new ApiError(403, "FORBIDDEN", "no"), label)).toContain("không có quyền");
  });

  it("404 → no-longer-in-bin message", () => {
    expect(restoreErrorMessage(new ApiError(404, "RECORD_NOT_FOUND", "gone"), label)).toContain("không còn trong thùng rác");
  });

  it("unknown error → generic fallback", () => {
    expect(restoreErrorMessage(new Error("boom"), label)).toBe("Không thể khôi phục bản ghi.");
  });
});
