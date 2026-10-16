// lib/pins.ts — per-user entity pins (W3 A — sidebar favorites), localStorage only.
// No backend (sys_user_prefs is P2); keys are namespaced per user so two accounts on the
// same browser don't share pins. Pure functions take an explicit userKey so unit tests
// don't need auth; the usePins() hook resolves the key from the cached /auth/me.
//
// Storage shape: string[] of entity names, in PIN ORDER (first pinned = first shown).

import { useCallback, useEffect, useState } from "react";
import { getStorageScope } from "@/lib/api";
import { readForUser } from "@/lib/scopedStorage";

const KEY_PREFIX = "nexus.pins.v1";
// Cross-component sync (Sidebar ↔ Home ↔ ⌘K) — same-tab localStorage writes don't fire
// the native "storage" event, so we dispatch our own.
const PINS_EVENT = "nexus:pins-changed";

/**
 * Namespace suffix for the current user: `${tenantId}:${userId}` (specs/062 L2b — Nexus and each customer
 * workspace keep their own pins), "anon" before login (pins made there stay local).
 */
export function currentUserKey(): string {
  return getStorageScope().key;
}

function storageKey(userKey: string): string {
  return `${KEY_PREFIX}:${userKey}`;
}

export function loadPins(userKey?: string): string[] {
  if (typeof window === "undefined") return [];
  try {
    // No explicit key = the current scope: only then may a pre-L2b value be adopted (home tenant).
    const raw = userKey === undefined ? readForUser(storageKey) : window.localStorage.getItem(storageKey(userKey));
    if (!raw) return [];
    const parsed = JSON.parse(raw);
    return Array.isArray(parsed) ? parsed.filter((x): x is string => typeof x === "string") : [];
  } catch {
    return [];
  }
}

export function savePins(pins: string[], userKey: string = currentUserKey()): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(storageKey(userKey), JSON.stringify(pins));
    window.dispatchEvent(new CustomEvent(PINS_EVENT));
  } catch {
    /* quota/private mode — pins are a convenience, fail silent */
  }
}

/** Toggle one entity; returns the new pin list (append at end when pinning). */
export function togglePin(pins: string[], entityName: string): string[] {
  return pins.includes(entityName)
    ? pins.filter((p) => p !== entityName)
    : [...pins, entityName];
}

export function isPinned(pins: string[], entityName: string): boolean {
  return pins.includes(entityName);
}

/**
 * React hook — pins for the current user, kept in sync across components in the same tab
 * (custom event) and across tabs (native storage event).
 */
export function usePins(): { pins: string[]; toggle: (entityName: string) => void } {
  const [pins, setPins] = useState<string[]>([]);

  useEffect(() => {
    const reload = () => setPins(loadPins());
    reload();
    window.addEventListener(PINS_EVENT, reload);
    window.addEventListener("storage", reload);
    return () => {
      window.removeEventListener(PINS_EVENT, reload);
      window.removeEventListener("storage", reload);
    };
  }, []);

  const toggle = useCallback((entityName: string) => {
    const next = togglePin(loadPins(), entityName);
    savePins(next);
    setPins(next);
  }, []);

  return { pins, toggle };
}
