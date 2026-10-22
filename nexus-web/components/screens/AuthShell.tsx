"use client";

// AuthShell — ONE shared frame for the whole auth family (login / forgot-password /
// reset-password / SSO complete), mounted once in app/(auth)/layout.tsx.
// SLDS has no login blueprint (RESEARCH-2026-09 §5.1/§5.2), so the frame is composed from
// `brand-band` (page backdrop, large) + `cards` (one centred card per page). The band's gradient
// is SLDS's brand ramp, so it follows tenant branding like every other brand pixel. Each page keeps
// its own logic and renders its content inside <AuthCard>.
// The band needs a stacking context (`z-0` + the band's own position:relative) so its ::before,
// drawn at z-index -1, stays above the page background — same as the blueprint's wrapper.
// CSS glue this frame depends on (app/globals.css, specs/038 T033 c10-auth — ships with this frame):
// `.slds-brand-band:before` keeps only the gradient (SLDS stacks banner-brand-default.png on it, a
// CC BY-ND asset campaign D3 forbids displaying), and `.nx-auth-band` paints the page surface
// brand-95 (the blueprint wrapper's #eef4ff) so the band's ::after fade has no seam.
import type { ReactNode } from "react";
import { Spinner } from "@/components/slds/Spinner";
import { useLocaleReady } from "@/lib/i18n";

// The pages render their text only in the browser: /forgot-password and /reset-password were server-rendered in
// Vietnamese and then hydrated in English (React #418, issue #6). /login already rendered nothing on the server.
export function AuthShell({ children }: { children: ReactNode }) {
  const ready = useLocaleReady();
  return (
    <div className="nx-auth-band slds-brand-band slds-brand-band_large z-0 min-h-screen">
      <main className="mx-auto w-full max-w-[26rem] px-4 pt-12 pb-12 sm:pt-20">{ready ? children : null}</main>
    </div>
  );
}

/**
 * The card every auth page draws its content in (blueprint `cards`, body only: the page's own
 * <h1> is the heading, so there is no card header to repeat it). `busy` lays the SLDS spinner
 * overlay (`slds-spinner_container`) over the card while a request is in flight.
 */
export function AuthCard({ busy, children }: { busy?: boolean; children: ReactNode }) {
  return (
    <article className="slds-card">
      <div className="slds-card__body slds-card__body_inner">
        <div className="flex flex-col gap-4 py-3 sm:px-3">{children}</div>
      </div>
      {busy && <Spinner container size="small" />}
    </article>
  );
}
