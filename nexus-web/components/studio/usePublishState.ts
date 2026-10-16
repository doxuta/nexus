"use client";

// usePublishState — GET /admin/publish/state through the token gate (specs/073 T557). getPublishState is in
// TOKEN_SCOPED_FETCHERS; this is the only place besides the legacy DraftBar that may call it. Re-reads when the window regains focus.
import { useEffect } from "react";
import { useQueryClient } from "@tanstack/react-query";
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

export function usePublishState(slug: string) {
  const qc = useQueryClient();
  // An editor saving a draft (or a publish/discard) announces on the draftEvents bus: recount without waiting for focus.
  useEffect(() => onDraftsChanged(() => void qc.invalidateQueries({ queryKey: ["project", slug, ...PUBLISH_STATE_KEY] })), [qc, slug]);
  return useProjectQuery(slug, PUBLISH_STATE_KEY, () => withTimeout(getPublishState()), { refetchOnWindowFocus: true, retry: false });
}
