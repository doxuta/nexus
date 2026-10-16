"use client";

// "Xem bản nháp" for ONE customer-app screen (specs/063-draft-publish S7-2, T061). The builder opens a
// report / dashboard of a delivered workspace with `?draft=1`; lib/previewMode then marks that screen's
// GETs with X-Nexus-Draft, so the server overlays the builder's unpublished changes. Per screen, never
// per tab (the whole-tab flag + banner + read-only mode belong to S7-3), and never on a write — the
// editing panel's save stays a plain write.
import { useCallback } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { useQuery } from "@tanstack/react-query";
import { getPublishState, hasDraftChange, type DraftState } from "@/lib/api";
import { useAuth } from "@/lib/auth/AuthProvider";
import { t } from "@/lib/i18n";
import { Button } from "@/components/slds/Button";
import { ScopedNotification } from "@/components/slds/ScopedNotification";

export interface DraftView {
  /** This screen was opened on the draft (`?draft=1`). */
  on: boolean;
  /** The caller is the builder of a delivered workspace — the only one who has a draft to look at. */
  canPreview: boolean;
  /** Switch this screen to the draft / back to the live version (the URL changes, the queries re-key). */
  set: (on: boolean) => void;
  /** Append `?draft=1` (or `&draft=1`) to an in-app link while the draft view is on. */
  keep: (href: string) => string;
}

export function useDraftView(): DraftView {
  const router = useRouter();
  const pathname = usePathname();
  const params = useSearchParams();
  const { user } = useAuth();
  const on = params?.get("draft") === "1";
  // 403 (not the builder) or backend off ⇒ no state ⇒ no button. Same key as AppsManager (same queryFn): cheap, cached.
  const { data: state } = useQuery({
    queryKey: ["publishState", "draftView"],
    queryFn: () => getPublishState().catch(() => null),
    enabled: Boolean(user?.isAdmin),
    staleTime: 30_000,
  });
  const set = useCallback((next: boolean) => {
    const q = new URLSearchParams(params?.toString() ?? "");
    if (next) q.set("draft", "1"); else q.delete("draft");
    const qs = q.toString();
    router.replace(`${pathname}${qs ? `?${qs}` : ""}`);
  }, [params, pathname, router]);
  const keep = useCallback((href: string) => (on ? `${href}${href.includes("?") ? "&" : "?"}draft=1` : href), [on]);
  return { on, canPreview: Boolean(state?.delivered), set, keep };
}

/** The toggle button of a page header: "Xem bản nháp" ⇄ "Xem bản đang chạy". Renders nothing for a non-builder. */
export function DraftViewButton({ view }: { view: DraftView }) {
  if (!view.on && !view.canPreview) return null;
  return (
    <Button onClick={() => view.set(!view.on)}>
      {t(view.on ? "draft.view.live" : "draft.view.open")}
    </Button>
  );
}

/** Banner above a draft-view screen: says the customer does not see this yet, offers the way back. */
export function DraftViewNotice({ view, state }: { view: DraftView; state?: DraftState }) {
  if (!view.on || !hasDraftChange(state)) return null;
  return (
    <ScopedNotification theme="warning" className="mt-3">
      {t(state === "deleted" ? "draft.view.bannerDeleted" : "draft.view.banner")}{" "}
      <Button variant="base" onClick={() => view.set(false)}>{t("draft.view.live")}</Button>
    </ScopedNotification>
  );
}
