// specs/076 FR-127: "Mở bản nháp" reaches ProjectHeader only through this bus; it must report whether anything listened.
import { describe, expect, it, vi } from "vitest";
import { hasPublishReviewOpener, onOpenPublishReview, openPublishReview } from "./draftEvents";

describe("openPublishReview", () => {
  it("returns false and fires nothing when no opener is registered", () => {
    expect(hasPublishReviewOpener()).toBe(false);
    expect(openPublishReview()).toBe(false);
  });
  it("calls every registered opener and stops after unsubscribe", () => {
    const cb = vi.fn();
    const off = onOpenPublishReview(cb);
    expect(hasPublishReviewOpener()).toBe(true);
    expect(openPublishReview()).toBe(true);
    expect(cb).toHaveBeenCalledTimes(1);
    off();
    expect(hasPublishReviewOpener()).toBe(false);
    expect(openPublishReview()).toBe(false);
    expect(cb).toHaveBeenCalledTimes(1);
  });
});
