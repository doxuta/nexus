"use client";

// lib/i18n/index.ts — i18n hook + provider for Nexus.
// The locale lives in the preferences (localStorage nexus.prefs.v1, lib/prefs.ts); see getLocale().
// Scope: shell + auth + most-visible screens + the demo path (issue #6). Full screen-by-screen conversion is incremental.
// i18n-TODO: mark untranslated strings with // i18n-TODO comment for future passes.

import { useCallback, useEffect, useSyncExternalStore } from "react";
import vi, { type I18nKey } from "./vi";
import en from "./en";

type Locale = "vi" | "en";

const catalogs: Record<Locale, Record<string, string>> = {
  vi: vi as unknown as Record<string, string>,
  en: en as unknown as Record<string, string>,
};

const PREFS = "nexus.prefs.v1";
const isLocale = (v: unknown): v is Locale => v === "vi" || v === "en";

/**
 * The language the UI renders in. On the server: Vietnamese. In the browser, the first that applies:
 * `?lang=vi|en` in the address (saved into the preferences, so the next page keeps it); the saved preference;
 * English when the browser lists English and no Vietnamese; Vietnamese.
 */
export function getLocale(): Locale {
  if (typeof window === "undefined") return "vi";
  try {
    const prefs = JSON.parse(window.localStorage.getItem(PREFS) ?? "{}");
    const asked = new URLSearchParams(window.location.search).get("lang");
    if (isLocale(asked)) {
      if (prefs.locale !== asked) window.localStorage.setItem(PREFS, JSON.stringify({ ...prefs, locale: asked }));
      return asked;
    }
    if (isLocale(prefs.locale)) return prefs.locale;
    const langs = (navigator.languages?.length ? navigator.languages : [navigator.language]).map((l) => l.toLowerCase());
    if (!langs.some((l) => l.startsWith("vi")) && langs.some((l) => l.startsWith("en"))) return "en";
  } catch {
    // storage blocked or a malformed preference: the default
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

/** Root-mounted (app/layout.tsx): `<html lang>` names the language the UI renders in, for screen readers. */
export function HtmlLang() {
  useEffect(() => {
    document.documentElement.lang = getLocale();
  }, []);
  return null;
}

const noSubscription = () => () => {};
/**
 * False on the server and while the page hydrates, true afterwards. The server cannot know the language (it renders
 * Vietnamese, see getLocale), so a server-rendered page whose text goes through t() renders that text only once this
 * is true: the server's Vietnamese never meets the client's English (React #418).
 */
export function useLocaleReady(): boolean {
  return useSyncExternalStore(noSubscription, () => true, () => false);
}

/**
 * The language for a server-rendered page that must show its text before hydration (the landing page): Vietnamese
 * on the server and while the page hydrates, so the hydrated text matches the server's (no React #418), then the
 * reader's language (getLocale) right after. useLocaleReady instead renders nothing until hydration.
 */
export function useHydratedLocale(): Locale {
  return useSyncExternalStore(noSubscription, getLocale, () => "vi");
}
