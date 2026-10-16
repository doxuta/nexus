"use client";

// WorkspaceBanner — "Đang dựng cho: <khách> (<mã>)" + "Về Nexus" (specs/062-customer-workspace L2, FR-013/015).
// Mounted by ShellGlobalHeader, so every page of the Studio AND of the customer app shows it while THIS
// tab builds inside a customer's workspace — it is the only thing telling the builder that what they
// edit is not Nexus's own. Renders nothing otherwise.
// It also keeps the session alive (D-11): the session token lives ≤ 15 minutes, and screens that go
// through adminReq have no 401 loop of their own (#290), so waiting for a 401 would break the Admin
// screens after 15 minutes. Renew 60 s before expiry, and when the tab becomes visible again (a sleeping
// machine skips timers). A session that ends under the tab (closed in another tab, revoked, expired)
// fires `nexus:workspace-ended`: leave for the library and say why on the page we land on.
import { useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { Building2 } from "lucide-react";
import { Alert } from "@/components/slds/Alert";
import { Button } from "@/components/slds/Button";
import { toast } from "@/components/ui/Toaster";
import { exitWorkspace, resumeWorkspace } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import {
  WORKSPACE_ENDED_EVENT, WORKSPACE_STORAGE_KEY, getWorkspace, isWorkspaceLeaving, setWorkspaceEndedNotice, subscribeWorkspace, takeWorkspaceEndedNotice,
} from "@/lib/workspace";

/** Renew this long before the token expires. */
const RENEW_LEAD_MS = 60_000;
/** After a transient renewal failure (network, 5xx) try again after this long. */
const RETRY_MS = 15_000;

// null on the server and on the first client render (no hydration mismatch), the real state right after.
const readRaw = () => window.sessionStorage.getItem(WORKSPACE_STORAGE_KEY);
const serverRaw = () => null;

export interface WorkspaceBannerProps {
  /** Label of the leave button. Default "Về Nexus" (the customer app); the Studio says "Về danh sách dự án". */
  exitLabel?: string;
  /** What the leave button does. Default `exitWorkspace()`; the Studio passes the frozen exit of lib/studioContext. */
  onExit?: () => Promise<void> | void;
  /** The session ended under the tab (the one-shot notice is already left for the landing page). Default: load /studio. */
  onEnded?: () => void;
}

export function WorkspaceBanner({ exitLabel, onExit, onEnded }: WorkspaceBannerProps = {}) {
  const raw = useSyncExternalStore(subscribeWorkspace, readRaw, serverRaw);
  // `raw` is the dependency: getWorkspace() re-reads that same string.
  const ws = useMemo(() => (raw ? getWorkspace() : null), [raw]);
  const [leaving, setLeaving] = useState(false);
  // The ended event fires AFTER lib/api cleared the state, so the name has to be remembered.
  const lastName = useRef("");
  const expiresAt = ws ? Date.parse(ws.expiresAt) : null;
  // The listener below is registered once; it must call the caller's latest handler, not the first render's.
  const onEndedRef = useRef(onEnded);
  useEffect(() => {
    onEndedRef.current = onEnded;
  });

  useEffect(() => {
    if (ws) lastName.current = ws.session.tenantName;
  }, [ws]);

  // The page we landed on after a session ended: say so once.
  useEffect(() => {
    const name = takeWorkspaceEndedNotice();
    if (name !== null) toast.warning(tf("workspace.ended", { name: name || t("workspace.customerFallback") }));
  }, []);

  // Always listening (even once the state is gone): the event is fired after the state is cleared.
  useEffect(() => {
    const handle = () => {
      setWorkspaceEndedNotice(lastName.current);
      if (onEndedRef.current) onEndedRef.current();
      else window.location.assign("/studio");
    };
    window.addEventListener(WORKSPACE_ENDED_EVENT, handle);
    return () => window.removeEventListener(WORKSPACE_ENDED_EVENT, handle);
  }, []);

  // Proactive renewal. Re-armed whenever the token (expiresAt) changes — i.e. after every renewal.
  useEffect(() => {
    if (expiresAt === null) return;
    let cancelled = false;
    let timer: number | undefined;
    const renew = () => {
      resumeWorkspace().catch(() => {
        // A definite "ended" cleared the state (cleanup runs → cancelled); anything else is transient.
        if (cancelled) return;
        window.clearTimeout(timer); // one retry chain at a time (visibility renew + armed timer can both fail)
        timer = window.setTimeout(renew, RETRY_MS);
      });
    };
    timer = window.setTimeout(renew, Math.max(0, expiresAt - RENEW_LEAD_MS - Date.now()));
    const onVisible = () => {
      if (document.visibilityState === "visible" && expiresAt - Date.now() < RENEW_LEAD_MS) renew();
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [expiresAt]);

  if (!ws) return null;

  return (
    <Alert
      variant="warning"
      icon={Building2}
      iconName="warning"
      className="shrink-0 print:hidden"
      action={
        <Button
          variant="neutral"
          className="slds-m-left_small shrink-0 whitespace-nowrap" // layout only: a long label ("Về danh sách dự án") must not fold inside the pill at 390px
          disabled={leaving}
          onClick={() => {
            setLeaving(true);
            // A "no" at the unsaved-changes question leaves the page as it was: the button must work again.
            Promise.resolve(onExit ? onExit() : exitWorkspace())
              .catch(() => undefined) // the Studio's frozen exit shows its own failure screen
              .finally(() => {
                if (!isWorkspaceLeaving()) setLeaving(false);
              });
          }}
        >
          {exitLabel ?? t("workspace.banner.exit")}
        </Button>
      }
    >
      {tf("workspace.banner.label", { name: ws.session.tenantName, slug: ws.session.tenantSlug })}
    </Alert>
  );
}
