"use client";

// Reads of the publish history (specs/079 f7-lich-su-dong-thoi-gian T057). Every one goes through useProjectQuery:
// listVersions / getVersion are token-scoped (they would show Nexus's history as the customer's).
import { ApiError, getVersion, listVersions } from "@/lib/api";
import { useProjectQuery } from "../useProjectQuery";

/** The server has no publish history route: 404 that is not the "no such version" answer. */
export const isEngineMissing = (e: unknown) => e instanceof ApiError && e.status === 404 && e.code !== "VERSION_NOT_FOUND";

export const usePublishVersions = (slug: string, delivered: boolean, limit: number) =>
  useProjectQuery(slug, ["versions", delivered, limit], () => (delivered ? listVersions(limit, 0) : Promise.resolve({ versions: [], total: 0 })), { retry: false });

export const usePublishVersion = (slug: string, id: string) => useProjectQuery(slug, ["version", id], () => getVersion(id), { retry: false });
