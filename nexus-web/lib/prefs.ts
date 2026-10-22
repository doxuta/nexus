// lib/prefs.ts — User preferences persisted to localStorage (nexus.prefs.v1).
// No backend: these are client-only UI preferences.
// Shape: NexusPrefs. Read with loadPrefs(), write with savePrefs().

import { getLocale } from "@/lib/i18n";

export interface NexusPrefs {
  /** UI colour scheme */
  theme: "dark" | "light" | "system";
  /** Layout density */
  density: "compact" | "comfortable" | "spacious";
  /** Interface language (used by i18n provider) */
  locale: "vi" | "en";
  /** In-app notifications toggle (UI hint — no backend integration yet) */
  notifyInApp: boolean;
  /** Email digest toggle (UI hint — no backend integration yet) */
  notifyEmail: boolean;
}

const STORAGE_KEY = "nexus.prefs.v1";

export const DEFAULT_PREFS: NexusPrefs = {
  theme: "dark",
  density: "comfortable",
  locale: "vi",
  notifyInApp: true,
  notifyEmail: false,
};

export function loadPrefs(): NexusPrefs {
  if (typeof window === "undefined") return DEFAULT_PREFS;
  // The language not saved yet is the one the UI picked (?lang, the browser), not a fixed Vietnamese.
  const defaults = { ...DEFAULT_PREFS, locale: getLocale() };
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return defaults;
    return { ...defaults, ...JSON.parse(raw) } as NexusPrefs;
  } catch {
    return defaults;
  }
}

export function savePrefs(prefs: NexusPrefs): void {
  if (typeof window === "undefined") return;
  window.localStorage.setItem(STORAGE_KEY, JSON.stringify(prefs));
}
