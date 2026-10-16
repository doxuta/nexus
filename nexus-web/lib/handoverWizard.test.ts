// handoverWizard — pure step-state logic of the "Bàn giao app này" wizard (spec §4/§6):
// cumulative advance gating, nav-based preselection (orphan-safe), and the graceful-degrade
// classifier that must NEVER report a live MS-C backend (TENANT_NOT_FOUND/FORBIDDEN) as
// "backend chưa hỗ trợ", nor an unsupported backend (bare 400/404) as anything else.
import { describe, it, expect } from "vitest";
import {
  HANDOVER_STEPS, canAdvance, applySelection, preselectEntities, packFileName, explainTargetError,
} from "./handoverWizard";
import { ApiError } from "./api";

describe("handoverWizard — steps & gating", () => {
  it("declares the 4 spec steps in order: export → workspace → plan → apply", () => {
    expect(HANDOVER_STEPS.map((s) => s.key)).toEqual(["export", "workspace", "plan", "apply"]);
  });

  it("advance gating is strictly cumulative", () => {
    const none = { hasPack: false, targetSlug: null, planReady: false };
    const packed = { ...none, hasPack: true };
    const targeted = { ...packed, targetSlug: "acme" };
    const planned = { ...targeted, planReady: true };

    // Step 0 needs the pack.
    expect(canAdvance(0, none)).toBe(false);
    expect(canAdvance(0, packed)).toBe(true);

    // Step 1 needs pack + target.
    expect(canAdvance(1, packed)).toBe(false);
    expect(canAdvance(1, targeted)).toBe(true);
    // A target without a pack (user went back and cleared it) must NOT advance.
    expect(canAdvance(1, { ...targeted, hasPack: false })).toBe(false);

    // Step 2 needs pack + target + successful dry-run.
    expect(canAdvance(2, targeted)).toBe(false);
    expect(canAdvance(2, planned)).toBe(true);

    // Apply is terminal — never "next".
    expect(canAdvance(3, planned)).toBe(false);
  });
});

describe("handoverWizard — export selection helpers", () => {
  it("applySelection keeps existing order, appends new picks, drops unchecked, never mutates", () => {
    const base = ["b", "a"];
    expect(applySelection(base, { a: true, b: true, c: true })).toEqual(["b", "a", "c"]);
    expect(applySelection(base, { a: false, b: true })).toEqual(["b"]);
    expect(applySelection(base, {})).toEqual([]);
    expect(base).toEqual(["b", "a"]);
  });

  it("preselectEntities keeps only nav entities that exist in the tenant, deduped", () => {
    const available = [{ name: "patients" }, { name: "visits" }, { name: "invoices" }];
    // "ghost" was deleted after being added to the nav (orphan ref) — must not leak into export.
    expect(preselectEntities(["patients", "ghost", "visits", "patients"], available))
      .toEqual(["patients", "visits"]);
    expect(preselectEntities([], available)).toEqual([]);
  });

  it("packFileName derives from the app slug + data flag", () => {
    expect(packFileName("phong_kham", false)).toBe("pack-phong_kham.json");
    expect(packFileName("phong_kham", true)).toBe("pack-phong_kham-with-data.json");
    expect(packFileName(null, false)).toBe("vertical-pack.json");
  });
});

describe("handoverWizard — explainTargetError (feature-detect, no version sniffing)", () => {
  it("TENANT_NOT_FOUND (live MS-C backend rejecting the slug) → target-gone, NOT unsupported", () => {
    const info = explainTargetError(new ApiError(404, "TENANT_NOT_FOUND", "không tìm thấy tenant"));
    expect(info.kind).toBe("target-gone");
    expect(info.message).not.toMatch(/chưa hỗ trợ/);
  });

  it("FORBIDDEN (operator gate) → forbidden, NOT unsupported", () => {
    const info = explainTargetError(new ApiError(403, "FORBIDDEN", "no"));
    expect(info.kind).toBe("forbidden");
    expect(info.message).not.toMatch(/chưa hỗ trợ/);
  });

  it("bare 400/404 (older backend without ?targetTenant=) → the honest 'chưa hỗ trợ' note", () => {
    for (const e of [
      new ApiError(400, "UNKNOWN", "bad request"),
      new ApiError(404, "UNKNOWN", "not found"),
    ]) {
      const info = explainTargetError(e);
      expect(info.kind).toBe("unsupported");
      expect(info.message).toMatch(/chưa hỗ trợ cài chéo workspace/);
      expect(info.message).toMatch(/cập nhật server/);
    }
  });

  it("other API errors (409 dup, 500) and non-API errors stay generic", () => {
    expect(explainTargetError(new ApiError(409, "RECORD_DUPLICATE", "trùng")).kind).toBe("other");
    expect(explainTargetError(new ApiError(500, "UNKNOWN", "boom")).kind).toBe("other");
    expect(explainTargetError(new Error("network down")).kind).toBe("other");
    expect(explainTargetError(new Error("network down")).message).toBe("network down");
  });
});
