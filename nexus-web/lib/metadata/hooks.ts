// Metadata hooks — useEntities, useFields, useLayout with long TTL
// Metadata changes rarely; stale 10 min / gc 30 min per fe-architecture spec.
"use client";

import { useQuery } from "@tanstack/react-query";
import { listEntities, listFields, getLayout } from "@/lib/api";
import { qk } from "@/lib/query/keys";
import { withDraftRead } from "@/lib/previewMode";
import { useAuth } from "@/lib/auth/AuthProvider";

const METADATA_STALE = 10 * 60 * 1000;  // 10 min
const METADATA_GC = 30 * 60 * 1000;      // 30 min

/** `draftRead` = the builder's draft overlay of a delivered project (specs/074 FR-064): own cache key + draft header. */
export interface DraftReadOpt { draftRead?: boolean }
const keyed = <K extends readonly unknown[]>(key: K, draftRead?: boolean) => (draftRead ? ([...key, "draft"] as const) : key);

export function useEntities({ draftRead }: DraftReadOpt = {}) {
  const { user } = useAuth();
  return useQuery({
    queryKey: keyed(qk.entities(), draftRead),
    queryFn: draftRead ? () => withDraftRead(listEntities) : listEntities,
    staleTime: METADATA_STALE,
    gcTime: METADATA_GC,
    enabled: !!user,
  });
}

// Real entity LABEL (from /metadata/entities) for headings/breadcrumbs — never the machine name.
// Falls back to the machine name only until the entities list has loaded.
export function useEntityLabel(entity: string): string {
  const { data: entities = [] } = useEntities();
  return entities.find((e) => e.name === entity)?.label ?? entity;
}

export function useFields(entity: string, { draftRead }: DraftReadOpt = {}) {
  const { user } = useAuth();
  return useQuery({
    queryKey: keyed(qk.fields(entity), draftRead),
    queryFn: () => (draftRead ? withDraftRead(() => listFields(entity)) : listFields(entity)),
    staleTime: METADATA_STALE,
    gcTime: METADATA_GC,
    enabled: !!user && !!entity,
  });
}

export function useLayout(entity: string, { draftRead }: DraftReadOpt = {}) {
  const { user } = useAuth();
  return useQuery({
    queryKey: keyed(qk.layout(entity), draftRead),
    queryFn: () => (draftRead ? withDraftRead(() => getLayout(entity)) : getLayout(entity)),
    staleTime: METADATA_STALE,
    gcTime: METADATA_GC,
    enabled: !!user && !!entity,
  });
}
