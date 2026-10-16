// Refusal codes of POST /admin/publish/versions/:id/rollback (specs/063 S7-7; specs/079 f7-lich-su-quay-lai).
// Each code has its own studio.maintain.err.* sentence; an unknown code returns null and the caller shows the generic one.
import type { I18nKey } from "@/lib/i18n/vi";

export const ROLLBACK_ERROR_CODES = [
  "DRAFTS_PENDING", "HEAD_MOVED", "PLAN_CHANGED", "ROLLBACK_NAME_TAKEN", "ROLLBACK_UNIQUE_CONFLICT",
  "PUBLISH_BUSY", "ROLLBACK_NO_SNAPSHOT", "ROLLBACK_USE_REVERT",
] as const;

export function rollbackErrorKey(code: string): I18nKey | null {
  return (ROLLBACK_ERROR_CODES as readonly string[]).includes(code) ? ((code === "ROLLBACK_NO_SNAPSHOT" ? "studio.maintain.rollback.err.NO_SNAPSHOT" : `studio.maintain.err.${code}`) as I18nKey) : null;
}
