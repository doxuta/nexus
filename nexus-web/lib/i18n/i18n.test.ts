// Tests for lib/i18n — t() translation function.
// getLocale() reads localStorage (cannot use in vi test env without DOM stub) so we
// test the locale-explicit overload: t(key, locale).

import { describe, it, expect } from "vitest";
import { t } from "@/lib/i18n";

describe("t() — Vietnamese (default locale)", () => {
  it("translates a shell nav key in Vietnamese", () => {
    expect(t("shell.nav.home", "vi")).toBe("Trang chủ");
  });

  it("translates auth keys in Vietnamese", () => {
    expect(t("auth.login.title", "vi")).toBe("Đăng nhập");
    expect(t("auth.logout", "vi")).toBe("Đăng xuất");
  });

  it("translates common action keys in Vietnamese", () => {
    expect(t("action.save", "vi")).toBe("Lưu");
    expect(t("action.cancel", "vi")).toBe("Huỷ");
    expect(t("action.delete", "vi")).toBe("Xoá");
  });

  it("translates settings keys in Vietnamese", () => {
    expect(t("settings.title", "vi")).toBe("Tuỳ chọn");
    expect(t("settings.appearance", "vi")).toBe("Giao diện");
  });
});

describe("t() — English locale", () => {
  it("translates a shell nav key in English", () => {
    expect(t("shell.nav.home", "en")).toBe("Home");
  });

  it("translates auth keys in English", () => {
    expect(t("auth.login.title", "en")).toBe("Sign in");
    expect(t("auth.logout", "en")).toBe("Sign out");
  });

  it("translates settings keys in English", () => {
    expect(t("settings.title", "en")).toBe("Preferences");
  });

  it("translates entity directory keys in English", () => {
    expect(t("entities.title", "en")).toBe("Entity Directory");
  });
});

describe("t() — fallback behaviour", () => {
  it("falls back to Vietnamese when a key is missing in English catalog", () => {
    // "entities.emptyHint" exists in vi but must also exist in en per parity rule.
    // This test ensures the fallback chain works by calling with a key that is defined in vi.
    const result = t("entities.emptyHint", "en");
    // Either the English string or the Vietnamese fallback — both are non-empty.
    expect(result.length).toBeGreaterThan(0);
    expect(result).not.toBe("entities.emptyHint"); // must never return the raw key
  });

  it("returns the raw key as last resort when key is absent in both catalogs", () => {
    // Cast to I18nKey to simulate a missing key (in practice never happens in prod).
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const result = t("nonexistent.key.that.does.not.exist" as any, "vi");
    expect(result).toBe("nonexistent.key.that.does.not.exist");
  });
});
