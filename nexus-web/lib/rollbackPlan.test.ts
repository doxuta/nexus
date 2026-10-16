// specs/079 f7-lich-su-quay-lai T067: every refusal code of "Quay lại bản này" has its own sentence (SC-013).
import { describe, expect, it } from "vitest";
import vi from "./i18n/vi";
import { rollbackErrorKey, ROLLBACK_ERROR_CODES } from "./rollbackPlan";

describe("rollbackErrorKey", () => {
  it.each(ROLLBACK_ERROR_CODES)("%s has a studio.maintain.err key with a sentence", (code) => {
    const key = rollbackErrorKey(code);
    expect(key).toBe(code === "ROLLBACK_NO_SNAPSHOT" ? "studio.maintain.rollback.err.NO_SNAPSHOT" : `studio.maintain.err.${code}`);
    expect((vi as Record<string, string>)[key as string]).toBeTruthy();
  });
  it("covers the seven codes of the spec and the server's PLAN_CHANGED", () => {
    expect([...ROLLBACK_ERROR_CODES].sort()).toEqual(
      ["DRAFTS_PENDING", "HEAD_MOVED", "PLAN_CHANGED", "PUBLISH_BUSY", "ROLLBACK_NAME_TAKEN", "ROLLBACK_NO_SNAPSHOT", "ROLLBACK_UNIQUE_CONFLICT", "ROLLBACK_USE_REVERT"],
    );
  });
  it("an unknown code has no key (the caller falls back to the generic sentence)", () => {
    expect(rollbackErrorKey("SOMETHING_ELSE")).toBeNull();
  });
});
