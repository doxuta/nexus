// lib/scopedStorage.ts — localStorage reads for the per-workspace browser memory (specs/062 L2b).
// Keys are namespaced by getStorageScope().key (`${tenantId}:${userId}`). Before L2b the namespace was the
// bare user id (or nothing at all), so a value found under the OLD key belongs to the home tenant: it is
// adopted once — moved to the new key, old key removed — and only when the tab is at home.
import { getStorageScope } from "@/lib/api";

/** Value of `newKey`; if absent and `legacyKey` holds one, move it to `newKey` and return it. Throws like localStorage. */
export function readMigrating(newKey: string, legacyKey: string | null): string | null {
  const v = window.localStorage.getItem(newKey);
  if (v !== null || !legacyKey) return v;
  const old = window.localStorage.getItem(legacyKey);
  if (old === null) return null;
  window.localStorage.setItem(newKey, old);
  window.localStorage.removeItem(legacyKey);
  return old;
}

/** Scoped read of a per-user key: `build(scope)` is the key; `build(bareUserId)` is its pre-L2b form. */
export function readForUser(build: (userKey: string) => string): string | null {
  const { key, legacyUserId } = getStorageScope();
  return readMigrating(build(key), legacyUserId ? build(legacyUserId) : null);
}

/** Scoped read of a key that had NO user part before L2b (`legacyKey` is the whole old key). */
export function readForScope(build: (userKey: string) => string, legacyKey: string): string | null {
  const { key, legacyUserId } = getStorageScope();
  return readMigrating(build(key), legacyUserId ? legacyKey : null);
}
