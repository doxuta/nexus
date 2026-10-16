import { describe, it, expect } from "vitest";
import { ALL_ITEMS, resolveActiveApp, slugifyAppName, isValidAppSlug } from "./apps";
import type { AppSummary } from "./api";

function app(name: string, isDefault = false): AppSummary {
  return { id: `id-${name}`, name, label: name, icon: null, color: null, isDefault, sortOrder: 0 };
}

describe("resolveActiveApp — stored slug vs apps list", () => {
  const apps = [app("sales"), app("support", true), app("lms")];

  it("nothing stored → the tenant's isDefault app", () => {
    expect(resolveActiveApp(null, apps)).toBe("support");
  });

  it("nothing stored and no default app (2+ apps) → All Items (never guess between lenses)", () => {
    expect(resolveActiveApp(null, [app("sales"), app("lms")])).toBe(ALL_ITEMS);
  });

  it("nothing stored, no default, EXACTLY ONE app → that app (F7-3 single-app tenant)", () => {
    expect(resolveActiveApp(null, [app("sales")])).toBe("sales");
  });

  it("stored slug of a DELETED app is NOT rescued by the single-app fallback", () => {
    // "Never silently jump" beats the exactly-one rule: only the nothing-stored path falls
    // back to the lone app.
    expect(resolveActiveApp("gone", [app("sales")])).toBe(ALL_ITEMS);
  });

  it("nothing stored and 0 apps → All Items (INV-1: sidebar behaves exactly as today)", () => {
    expect(resolveActiveApp(null, [])).toBe(ALL_ITEMS);
  });

  it("stored '*' → All Items, even when a default app exists", () => {
    expect(resolveActiveApp(ALL_ITEMS, apps)).toBe(ALL_ITEMS);
  });

  it("stored slug that still exists → that app (wins over the default)", () => {
    expect(resolveActiveApp("sales", apps)).toBe("sales");
  });

  it("stored slug of a DELETED app → All Items, NOT the default app", () => {
    expect(resolveActiveApp("gone", apps)).toBe(ALL_ITEMS);
  });
});

describe("slugifyAppName — Vietnamese label → server slug rule ^[a-z][a-z0-9_]{0,59}$", () => {
  it("strips diacritics incl. đ/Đ and joins words with _", () => {
    expect(slugifyAppName("Quản lý Bán hàng")).toBe("quan_ly_ban_hang");
    expect(slugifyAppName("Đào tạo")).toBe("dao_tao");
  });

  it("drops characters outside [a-z0-9_] and collapses runs of _", () => {
    expect(slugifyAppName("CRM  —  Sales & Support!")).toBe("crm_sales_support");
  });

  it("trims to a leading letter (leading digits/underscores removed)", () => {
    expect(slugifyAppName("2026 Kế hoạch")).toBe("ke_hoach");
    expect(slugifyAppName("__x")).toBe("x");
  });

  it("caps at 60 chars", () => {
    expect(slugifyAppName("a".repeat(80)).length).toBe(60);
  });

  it("empty / all-invalid input → empty string (caller blocks submit)", () => {
    expect(slugifyAppName("!!!")).toBe("");
    expect(slugifyAppName("")).toBe("");
  });

  it("every non-empty suggestion passes the live validator", () => {
    for (const label of ["Quản lý Bán hàng", "Hỗ trợ 24/7", "LMS — Đào tạo nội bộ"]) {
      expect(isValidAppSlug(slugifyAppName(label))).toBe(true);
    }
  });
});

describe("isValidAppSlug", () => {
  it("accepts the server pattern", () => {
    expect(isValidAppSlug("sales")).toBe(true);
    expect(isValidAppSlug("a")).toBe(true);
    expect(isValidAppSlug("crm_2026")).toBe(true);
    expect(isValidAppSlug("a".repeat(60))).toBe(true);
  });

  it("rejects everything else", () => {
    expect(isValidAppSlug("")).toBe(false);
    expect(isValidAppSlug("2sales")).toBe(false);
    expect(isValidAppSlug("_sales")).toBe(false);
    expect(isValidAppSlug("Sales")).toBe(false);
    expect(isValidAppSlug("bán_hàng")).toBe(false);
    expect(isValidAppSlug("a".repeat(61))).toBe(false);
  });
});

describe("resolveActiveApp — per-role default", () => {
  const a = (name: string, extra: Partial<AppSummary> = {}): AppSummary => ({ ...app(name), ...extra });
  it("app mặc định của vai người đăng nhập thắng isDefault của workspace", () => {
    const apps = [a("crm", { isDefault: true }), a("nha_khoa", { defaultForCaller: true })];
    expect(resolveActiveApp(null, apps)).toBe("nha_khoa");
  });
  it("không vai nào khai → vẫn theo isDefault", () => {
    expect(resolveActiveApp(null, [a("crm", { isDefault: true }), a("x")])).toBe("crm");
  });
  it("slug đã lưu vẫn thắng mặc định theo vai", () => {
    expect(resolveActiveApp("crm", [a("crm"), a("nha_khoa", { defaultForCaller: true })])).toBe("crm");
  });
});
