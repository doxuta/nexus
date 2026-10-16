"use client";

// BrandingBoot — AO-THUONG-HIEU p1 (../specs/013-tenant-branding/spec.md §5). Root-mounted, renders nothing.
// On mount: apply the CACHED branding immediately (no flash of Nexus teal on a branded tenant),
// then fetch the fresh document by the stored workspace slug and re-apply + re-cache. Tenants
// without branding get {} → applyBranding clears any stale vars from a previous session.
import { useEffect, useSyncExternalStore } from "react";

import { getPublicBranding, getTenantBranding, tenantSlug, type Branding } from "@/lib/api";
import { applyBranding, BRANDING_EVENT, readCachedBranding } from "@/lib/branding";
import { inDraftPreviewTab } from "@/lib/previewMode";

export function BrandingBoot() {
  useEffect(() => {
    // specs/063 S7-3: a draft-preview tab paints the builder's DRAFT branding (the admin route overlays it
    // for the builder) and never writes it to the shared boot cache — the customer's login page and the
    // builder's other tabs keep the live look. Any failure (not signed in yet, not the builder) falls
    // back to the public document below.
    const draft = inDraftPreviewTab();
    applyBranding(readCachedBranding(), draft ? { persist: false } : undefined);
    const slug = tenantSlug();
    let alive = true;
    const loadPublic = () => {
      if (!slug) return;
      getPublicBranding(slug)
        .then((b) => { if (alive) applyBranding(b, draft ? { persist: false } : undefined); })
        .catch(() => {}); // offline boot keeps the cached look
    };
    if (draft) {
      getTenantBranding()
        .then((b) => { if (alive) applyBranding(b, { persist: false }); })
        .catch(loadPublic);
    } else {
      loadPublic();
    }
    return () => { alive = false; };
  }, []);
  return null;
}

// ── useBranding — subscribe chrome components (AppSwitcher, Login) to the live document ──────

let snapshot: Branding = {};
let snapshotInit = false;
const EMPTY: Branding = {};

function subscribe(cb: () => void): () => void {
  const handler = (e: Event) => {
    snapshot = (e as CustomEvent<Branding>).detail ?? {};
    cb();
  };
  window.addEventListener(BRANDING_EVENT, handler);
  return () => window.removeEventListener(BRANDING_EVENT, handler);
}

function getSnapshot(): Branding {
  if (!snapshotInit) {
    snapshot = readCachedBranding();
    snapshotInit = true;
  }
  return snapshot;
}

/** Current branding document ({} = default Nexus). SSR-safe (empty on the server). */
export function useBranding(): Branding {
  return useSyncExternalStore(subscribe, getSnapshot, () => EMPTY);
}
