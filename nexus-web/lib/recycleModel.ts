// lib/recycleModel.ts — pure helpers for the W9c recycle bin (soft-delete list + restore).
// ../specs/026-recycle-bin/spec.md + api-conventions §13.1. Kept React/DOM-free so the restore-flow state
// transitions and the error→message mapping are unit-testable (lib/recycleModel.test.ts).

import { ApiError } from "@/lib/api";

// Drop a record by id — used after a successful restore to remove it from the deleted list, and
// (defensively) after a 404 (already restored / hard-gone elsewhere) so the row doesn't linger.
export function removeById<T extends { id: string }>(rows: T[], id: string): T[] {
  return rows.filter((r) => r.id !== id);
}

// Should the row be removed from the deleted list for this error? 404 means it is no longer a
// soft-deleted record (restored elsewhere / purged) → drop it so the list stays honest.
export function shouldDropOnError(err: unknown): boolean {
  return err instanceof ApiError && (err.status === 404 || err.code === "RECORD_NOT_FOUND");
}

// Map a restore error to an honest Vietnamese toast. `labelFor` resolves a field NAME → its display
// label so the 409 unique-collision names the human field, not the raw column (api-conventions §13.1:
// 409 RECORD_DUPLICATE carries details.field).
export function restoreErrorMessage(err: unknown, labelFor: (field: string) => string): string {
  if (err instanceof ApiError) {
    if (err.status === 409 || err.code === "RECORD_DUPLICATE") {
      const field = typeof err.details?.field === "string" ? err.details.field : "";
      const label = field ? labelFor(field) : "";
      return label
        ? `Trường "${label}" đang bị một bản ghi khác giữ — sửa bản ghi kia trước rồi khôi phục lại.`
        : "Một giá trị duy nhất đang bị bản ghi khác giữ — sửa bản ghi kia trước rồi khôi phục lại.";
    }
    if (err.status === 403 || err.code === "FORBIDDEN") {
      return "Bạn không có quyền khôi phục bản ghi này.";
    }
    if (err.status === 404 || err.code === "RECORD_NOT_FOUND") {
      return "Bản ghi không còn trong thùng rác (có thể đã được khôi phục hoặc xoá vĩnh viễn).";
    }
    return err.message || "Không thể khôi phục bản ghi.";
  }
  return "Không thể khôi phục bản ghi.";
}
