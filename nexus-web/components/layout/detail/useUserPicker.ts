"use client";

// Shared people-picker data for ShareDialog + OwnerDialog (DAILY-QUEUE #252).
// Opening loads ONE directory page (what you see before typing); typing asks the server
// (`GET /users/directory?q=`, case- and diacritic-insensitive) after a short pause, so a person
// beyond the first page is still reachable in an organisation with more than a page of users.
// While the answer is in flight the already-loaded page is filtered client-side (diacritics-insensitive).
import { useEffect, useState } from "react";
import { ensureAuth, listUserDirectory, ApiError, type DirectoryUser } from "@/lib/api";
import { matches } from "@/components/slds/Combobox";

export const DIRECTORY_PAGE = 100; // the endpoint's max page
const SEARCH_LIMIT = 20;
const DEBOUNCE_MS = 250;

export function useUserPicker(open: boolean, filter: string, loadErrText: string) {
  const [base, setBase] = useState<DirectoryUser[] | null>(null);
  const [found, setFound] = useState<{ q: string; users: DirectoryUser[] } | null>(null);
  const [seen, setSeen] = useState<Record<string, DirectoryUser>>({});
  const [loadErr, setLoadErr] = useState("");
  const q = filter.trim();

  useEffect(() => {
    if (!open) return;
    let alive = true;
    (async () => {
      try {
        await ensureAuth();
        const us = await listUserDirectory("", DIRECTORY_PAGE);
        if (alive) {
          setLoadErr("");
          setBase(us);
          setSeen((prev) => ({ ...prev, ...Object.fromEntries(us.map((u) => [u.id, u])) }));
        }
      } catch (e) {
        // A refused read shows its real error — never an empty list that reads as "nobody".
        if (alive) setLoadErr(e instanceof ApiError ? e.message : loadErrText);
      }
    })();
    return () => { alive = false; };
  }, [open, loadErrText]);

  useEffect(() => {
    if (!open || !q) return;
    let alive = true;
    const timer = setTimeout(async () => {
      try {
        const us = await listUserDirectory(q, SEARCH_LIMIT);
        if (alive) {
          setFound({ q, users: us });
          setSeen((prev) => ({ ...prev, ...Object.fromEntries(us.map((u) => [u.id, u])) }));
        }
      } catch {
        // Search failed: keep the client-side filter of the loaded page (the dialog stays usable).
      }
    }, DEBOUNCE_MS);
    return () => { alive = false; clearTimeout(timer); };
  }, [open, q]);

  const visible = !q
    ? (base ?? [])
    : found?.q === q
      ? found.users
      : (base ?? []).filter((u) => matches(u.fullName, q) || matches(u.email, q));

  return { ready: base !== null, loadErr, visible, find: (id: string): DirectoryUser | undefined => seen[id] };
}
