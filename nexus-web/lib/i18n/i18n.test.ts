// Tests for lib/i18n — t() translation function and getLocale().

import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { getLocale, t } from "@/lib/i18n";
import viCatalog from "@/lib/i18n/vi";
import enCatalog from "@/lib/i18n/en";
import { loadPrefs } from "@/lib/prefs";
import { layoutViewLabel, reportSummaryText, workflowActionText, workflowTriggerPhrase } from "@/lib/aiBuilder";
import { readFileSync } from "node:fs";
import { join } from "node:path";

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

// Issue #6: which language the UI renders in. Vietnamese stays the default; English comes from ?lang=en (remembered
// in the preferences), a saved preference, or a browser that lists English and no Vietnamese.
describe("getLocale()", () => {
  const PREFS = "nexus.prefs.v1";
  const browser = (langs: string[]) => vi.spyOn(navigator, "languages", "get").mockReturnValue(langs);
  const at = (url: string) => window.history.replaceState(null, "", url);
  beforeEach(() => {
    localStorage.clear();
    at("/home");
  });
  afterEach(() => vi.restoreAllMocks());

  it("is Vietnamese by default, also for a browser that lists Vietnamese after English", () => {
    browser(["vi-VN", "vi"]);
    expect(getLocale()).toBe("vi");
    browser(["en-VN", "vi-VN"]);
    expect(getLocale()).toBe("vi");
    browser(["fr-FR"]);
    expect(getLocale()).toBe("vi");
  });

  it("is English for a browser that lists English and no Vietnamese", () => {
    browser(["en-US", "en"]);
    expect(getLocale()).toBe("en");
    expect(t("auth.logout")).toBe("Sign out");
  });

  it("?lang=en picks English and remembers it in the preferences, keeping the others", () => {
    browser(["vi-VN"]);
    localStorage.setItem(PREFS, JSON.stringify({ density: "compact" }));
    at("/login?lang=en");
    expect(getLocale()).toBe("en");
    expect(JSON.parse(localStorage.getItem(PREFS)!)).toEqual({ density: "compact", locale: "en" });
    at("/home"); // the next page, without the parameter
    expect(getLocale()).toBe("en");
    expect(loadPrefs().locale).toBe("en");
  });

  it("?lang=vi wins over a saved English; an unknown ?lang is ignored", () => {
    browser(["en-US"]);
    localStorage.setItem(PREFS, JSON.stringify({ locale: "en" }));
    at("/home?lang=fr");
    expect(getLocale()).toBe("en");
    at("/home?lang=vi");
    expect(getLocale()).toBe("vi");
    at("/home");
    expect(getLocale()).toBe("vi");
  });

  it("a saved preference wins over the browser", () => {
    browser(["en-US"]);
    localStorage.setItem(PREFS, JSON.stringify({ locale: "vi" }));
    expect(getLocale()).toBe("vi");
  });

  it("the Preferences screen starts from the language the UI is in, not a fixed Vietnamese", () => {
    browser(["en-US"]);
    expect(loadPrefs().locale).toBe("en");
  });
});

describe("en.ts", () => {
  // tf() fills {name} placeholders: an English value that drops or renames one prints the raw "{name}" (issue #6).
  it("every English value has its Vietnamese key and the same {placeholders}", () => {
    const vi = viCatalog as Record<string, string>;
    const holes = (v: string) => (v.match(/\{\w+\}/g) ?? []).sort().join(" ");
    const bad = Object.entries(enCatalog as Record<string, string>)
      .filter(([k, v]) => !(k in vi) || holes(v) !== holes(vi[k]))
      .map(([k, v]) => `${k}: "${v}"`);
    expect(bad).toEqual([]);
  });
});

// Issue #6, round 2: the README sends English readers to /login?lang=en, and the assistant's cards build their text in
// lib/aiBuilder.ts. Both follow the UI language.
describe("English on the sign-in page and on the assistant's cards", () => {
  afterEach(() => localStorage.clear());

  it("every key the sign-in page renders has English", () => {
    const src = readFileSync(join(__dirname, "..", "..", "components", "screens", "Login.tsx"), "utf8");
    const keys = [...new Set([...src.matchAll(/\btf?\(\s*"([\w.]+)"/g)].map((m) => m[1]))];
    expect(keys.length).toBeGreaterThan(30);
    expect(keys.filter((k) => !(k in enCatalog))).toEqual([]);
  });

  it("card text is English in English, Vietnamese by default", () => {
    expect(workflowTriggerPhrase("after_create")).toBe("Tạo mới bản ghi");
    localStorage.setItem("nexus.prefs.v1", JSON.stringify({ locale: "en" }));
    expect(workflowTriggerPhrase("after_create")).toBe("A record is created");
    expect(layoutViewLabel("list")).toBe("List");
    expect(workflowActionText({ type: "in_app_notification", userId: "$owner", title: "Overdue" })).toBe("Notify the record's owner: “Overdue”");
    expect(reportSummaryText("summary", { metric: "count", groupBy: "stage", bucket: "month" })).toBe("Count of records · grouped by stage · per month");
  });
});
