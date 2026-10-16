// Permissions hook + <Can> guard
// GET /metadata/permissions/me returns the caller's grant matrix.
// Admin = [{"*","*","all"}] wildcard. Empty = deny-all (fail-closed).
"use client";

import { useQuery } from "@tanstack/react-query";
import { getMyPermissions, type PermGrant, type PermAction } from "@/lib/api";
import { qk } from "@/lib/query/keys";
import { useAuth } from "@/lib/auth/AuthProvider";
import { usePreviewReadOnly } from "@/lib/previewMode";

const METADATA_STALE = 10 * 60 * 1000; // 10 min

export function usePermissionsQuery() {
  const { user } = useAuth();
  // specs/064 FR-014: a tab previewing a role reads the role's grants fresh — the builder edits that
  // role in another tab, and the 10-minute cache would keep the old menu on screen until it expired.
  const previewing = !!user?.preview;
  return useQuery({
    queryKey: qk.permissions(),
    queryFn: getMyPermissions,
    staleTime: previewing ? 0 : METADATA_STALE,
    refetchOnWindowFocus: previewing,
    gcTime: 30 * 60 * 1000,
    enabled: !!user,
  });
}

export function usePermissions() {
  const { data: grants = [], isLoading, isError } = usePermissionsQuery();
  const { user } = useAuth();
  // specs/063 S7-3: a preview tab (draft, or a role) never writes — the server answers every write with
  // 403 PREVIEW_READ_ONLY, so every create / edit / delete button the screens derive from `can()` is
  // withdrawn here instead of being offered and failing. ONE place for list, detail, form, kanban, calendar.
  const readOnly = usePreviewReadOnly();

  function can(action: PermAction, entityName: string): boolean {
    if (!user) return false;
    if (readOnly && action !== "view") return false;
    // Admin bypass
    if (user.isAdmin) return true;
    // Deny-all if no grants
    if (grants.length === 0) return false;
    // Check wildcard grant
    const hasWildcard = grants.some((g) =>
      (g.entityName === "*" || g.entityName === entityName) &&
      (g.action === "*" || g.action === action) &&
      g.accessLevel !== "none",
    );
    return hasWildcard;
  }

  // isError is EXPORTED, not swallowed: `can()` stays fail-closed on a failed fetch (correct — never
  // widen access because a request broke), but a consumer that cannot tell "you are denied" from
  // "we could not ask" renders a permission assertion it did not verify. A screen silently in
  // read-only mode because a fetch failed looks exactly like a screen the admin locked down, and
  // the user has no way to know which — so they file a permissions ticket for a network blip.
  return { can, grants, isLoading, isError };
}

// <Can action="view" entity="accounts"> gate component
export function Can({
  action,
  entity,
  children,
  fallback = null,
}: {
  action: PermAction;
  entity: string;
  children: React.ReactNode;
  fallback?: React.ReactNode;
}) {
  const { can, isLoading } = usePermissions();
  if (isLoading) return null;
  return can(action, entity) ? <>{children}</> : <>{fallback}</>;
}
