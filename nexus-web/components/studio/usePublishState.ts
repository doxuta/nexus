"use client";

// usePublishState — GET /admin/publish/state through the token gate (specs/073 T557). getPublishState is in
// TOKEN_SCOPED_FETCHERS; this is the only place besides the legacy DraftBar that may call it. Re-reads when the window regains focus.
import { useEffect } from "react";
import { useQueryClient, type QueryClient } from "@tanstack/react-query";
import { getPublishState } from "@/lib/api";
import { onDraftsChanged } from "@/lib/draftEvents";
import { useProjectQuery } from "./useProjectQuery";

export const PUBLISH_STATE_KEY = ["publish", "state"] as const;

// A read that hangs must turn into an error, not a spinner: the assistant card treats "no answer" as unknown delivery (specs/076 FR-130).
// retry:false ⇒ the card turns to "unknown" after 8 s, not after 8 + 1 + 8 s.
export const READ_TIMEOUT_MS = 8000;
function withTimeout<T>(read: Promise<T>): Promise<T> {
  let timer: ReturnType<typeof setTimeout>;
  const late = new Promise<never>((_, reject) => { timer = setTimeout(() => reject(new Error("publish state timed out")), READ_TIMEOUT_MS); });
  return Promise.race([read, late]).finally(() => clearTimeout(timer));
}

// An editor saving a draft (or a publish/discard) announces on the draftEvents bus: recount without waiting for focus.
// ONE listener per (client, project), however many components read the state (#46): every invalidate cancels the read in
// flight and sends another, and getPublishState takes no signal, so a listener per reader turned one save into N reads.
const listeners = new WeakMap<QueryClient, Map<string, { readers: number; off: () => void }>>();
function shareDraftsListener(qc: QueryClient, slug: string): () => void {
  const bySlug = listeners.get(qc) ?? new Map<string, { readers: number; off: () => void }>();
  listeners.set(qc, bySlug);
  let l = bySlug.get(slug);
  if (!l) {
    l = { readers: 0, off: onDraftsChanged(() => void qc.invalidateQueries({ queryKey: ["project", slug, ...PUBLISH_STATE_KEY] })) };
    bySlug.set(slug, l);
  }
  const mine = l;
  mine.readers++;
  return () => {
    if (--mine.readers > 0) return;
    mine.off();
    bySlug.delete(slug);
  };
}

// rereadOnMount: the project frame's read only (#62). A draft written where nothing announces (a report created on the
// customer face /reports) must show the moment the builder opens the project, inside the staleTime too. An invalidate,
// not refetchOnMount "always": it also re-reads when the token is known only after mount (enabled flips on a fresh cached
// answer), and cancelRefetch:false joins a read already in flight, so a mount still costs one read. Pages' readers don't
// ask: each navigation would add a read (#52: one on mount, one per announcement).
export function usePublishState(slug: string, { rereadOnMount = false } = {}) {
  const qc = useQueryClient();
  useEffect(() => shareDraftsListener(qc, slug), [qc, slug]);
  useEffect(() => {
    if (rereadOnMount) void qc.invalidateQueries({ queryKey: ["project", slug, ...PUBLISH_STATE_KEY] }, { cancelRefetch: false });
  }, [qc, slug, rereadOnMount]);
  return useProjectQuery(slug, PUBLISH_STATE_KEY, () => withTimeout(getPublishState()), { refetchOnWindowFocus: true, retry: false });
}
