import { describe, expect, it } from "vitest";
import { ApiError } from "@/lib/api";
import { errorMessage, strictErrorMessage } from "@/lib/errorMessages";

const FALLBACK = "Không lưu được. Thử lại, hoặc báo người quản trị xưởng.";

describe("strictErrorMessage (specs/075 T008, part used by the screen editor)", () => {
  it.each(["LAYOUT_INVALID", "LAYOUT_NOT_FOUND", "TENANT_MAINTENANCE", "WORKSPACE_SESSION_NOT_ALLOWED"])(
    "%s gives a business sentence without engine words",
    (code) => {
      const m = strictErrorMessage(new ApiError(422, code, "raw server text with entity"));
      expect(m).not.toBe(FALLBACK);
      expect(m).not.toMatch(/entity|layout|tenant|workspace|session|inverse|unique/i);
    },
  );
  it("unknown code or non-ApiError gives the fixed sentence and never err.message", () => {
    expect(strictErrorMessage(new ApiError(500, "SOMETHING_ODD", "leaky entity message"))).toBe(FALLBACK);
    expect(strictErrorMessage(new Error("boom"))).toBe(FALLBACK);
    expect(strictErrorMessage("x")).toBe(FALLBACK);
  });
  it("LAYOUT_INVALID stays the server's own specific text for every other screen (errorMessage)", () => {
    expect(errorMessage(new ApiError(422, "LAYOUT_INVALID", "specific reason"))).toBe("specific reason");
  });
});
