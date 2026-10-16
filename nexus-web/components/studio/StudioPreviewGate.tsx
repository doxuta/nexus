"use client";

// StudioPreviewGate — the Studio never runs inside a preview tab (specs/064-preview-as-role-sample-data S9-2).
// A tab that is previewing a role holds only the preview token; the Studio's admin routes would 403
// and, worse, a stray "Xem như khách" click could mix the two. So when the Studio mounts and the tab
// still carries a preview, end it ("left"), clear it and reload — and never render the Studio (nor fire
// one of its requests) in between. Also the one place that tells the builder, once, that a preview
// ended by itself (expiry / revoked) before they were sent back here.
import { useEffect, useSyncExternalStore, type ReactNode } from "react";
import { Spinner } from "@/components/slds/Spinner";
import { toast } from "@/components/ui/Toaster";
import { t } from "@/lib/i18n";
import { clearDraftPreview } from "@/lib/previewMode";
import { clearPreview, endPreviewExit, PREVIEW_STORAGE_KEY, takePreviewNotice } from "@/lib/previewSession";
import { endPreviewIfAny } from "@/lib/studioContext";

const PENDING = "\u0000pending";
const noopSubscribe = () => () => {};
const readRaw = () => window.sessionStorage.getItem(PREVIEW_STORAGE_KEY);
// Server + hydration pass: unknown yet → render nothing of the Studio (RequireAuth is client-only anyway).
const serverRaw = () => PENDING;

export function StudioPreviewGate({ children }: { children: ReactNode }) {
  const raw = useSyncExternalStore(noopSubscribe, readRaw, serverRaw);
  const previewing = raw !== null && raw !== PENDING;

  useEffect(() => {
    if (raw === PENDING) return;
    if (previewing) {
      // A record without a token is no preview: endPreviewIfAny does nothing for it, and the tab would sit on the
      // spinner forever. Wipe it and load afresh.
      if (!endPreviewIfAny()) {
        clearPreview();
        window.location.reload();
      }
      return;
    }
    // Studio is up and this page is not previewing: a stale exit latch (soft navigation) must not
    // strip the builder's token, and a preview that ended on its own gets its one Toast.
    endPreviewExit();
    clearDraftPreview(); // a draft-preview tab flag must not follow the builder into Studio
    if (takePreviewNotice()) toast.info(t("preview.ended"));
  }, [raw, previewing]);

  if (raw === PENDING || previewing) return <div className="relative h-screen"><Spinner /></div>;
  return <>{children}</>;
}
