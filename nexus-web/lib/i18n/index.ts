"use client";

// lib/i18n/index.ts — i18n hook + provider for Nexus.
// Reading locale from localStorage (nexus.prefs.v1).
// Scope: shell + auth + most-visible screens. Full screen-by-screen conversion is incremental.
// i18n-TODO: mark untranslated strings with // i18n-TODO comment for future passes.

import { useCallback } from "react";
import vi, { type I18nKey } from "./vi";
import en from "./en";

type Locale = "vi" | "en";

const catalogs: Record<Locale, Record<string, string>> = {
  vi: vi as unknown as Record<string, string>,
  en: en as unknown as Record<string, string>,
};

/**
 * Get the current locale from prefs (synchronous — reads localStorage once).
 * Safe for both SSR (returns "vi") and client.
 */
export function getLocale(): Locale {
  if (typeof window === "undefined") return "vi";
  try {
    const raw = window.localStorage.getItem("nexus.prefs.v1");
    if (raw) {
      const p = JSON.parse(raw);
      if (p.locale === "en") return "en";
    }
  } catch {
    // ignore
  }
  return "vi";
}

/**
 * Translate a key to the current locale.
 * Falls back to Vietnamese if key missing in target locale.
 * Falls back to the raw key if missing in both.
 */
export function t(key: I18nKey, locale?: Locale): string {
  const loc = locale ?? getLocale();
  const catalog = catalogs[loc];
  const result = catalog[key];
  if (result !== undefined) return result;
  // Fallback to vi
  if (loc !== "vi") return catalogs.vi[key] ?? key;
  return key;
}

/** t() with `{name}` placeholders filled, e.g. tf("page.batch.submit", { n: 3 }) → "Gửi 3 dòng". */
export function tf(key: I18nKey, vars: Record<string, string | number>): string {
  return t(key).replace(/\{(\w+)\}/g, (m, k: string) => (k in vars ? String(vars[k]) : m));
}

/**
 * React hook — returns a bound translator that always reads current locale.
 * Usage: const { t } = useT(); t("shell.nav.home")
 */
export function useT() {
  const locale = getLocale();
  const translate = useCallback((key: I18nKey) => t(key, locale), [locale]);
  return { t: translate, locale };
}
