"use client";

// The context gate (specs/073 FR-020, architecture 1.2): decides whether the page may mount for project S, given what
// the SERVER says the token in this tab is (GET /auth/me) — never what the tab's storage claims (FR-026).
//   phien page   S = home, no session      show           | S = home, session W     "close the session and open the workshop"
//                session is S              show           | session is another     "switch to S"
//                no session                "open S"       |
//   nha page     session open              "switch to the workshop"                 | no session   show
// Rules: nothing is opened or closed on page load (opening a session writes a row and an audit line, so it takes one
// click); `next` only ever passes through safeNext; a slug that is not a valid project code stops here, before any call;
// not sure who the token is ⇒ closed. A builder component never mounts on a `nha` page of a customer (the route table
// enforces that; this gate only decides context).
import { useQueryClient } from "@tanstack/react-query";
import Link from "next/link";
import { useEffect, useState, type ReactNode } from "react";
import { Button } from "@/components/slds/Button";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { ApiError } from "@/lib/api";
import { isPreviewExiting } from "@/lib/previewSession";
import { errorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { useConfirmLeave } from "@/lib/navigation-blocker";
import { endPreviewIfAny, enterProject, exitProject, switchProject, useStudioIdentity } from "@/lib/studioContext";
import { SLUG_RE, safeNext, type RouteCtx } from "@/lib/studioRoutes";
import { isWorkspaceLeaving } from "@/lib/workspace";

interface Block {
  message: string;
  /** One primary action; absent = a sentence and a way back only. */
  action?: { label: string; run: (next: string) => Promise<void> };
}

export function ContextGate({ slug, ctx, next, children }: { slug: string; ctx: RouteCtx; next?: string; children: ReactNode }) {
  // Case 0 (FR-086): a "view as role" tab is ended before any decision. An effect, not render: it does real work
  // (ends the preview on the server and reloads), and it is declared before useStudioIdentity so it runs before that
  // query's observer subscribes. StudioPreviewGate, outermost in the layout, already keeps this page from mounting
  // in such a tab; this is the second lock for any route that reaches the gate without it.
  useEffect(() => {
    endPreviewIfAny();
  }, []);
  const slugOk = SLUG_RE.test(slug);
  const identity = useStudioIdentity({ enabled: slugOk });
  const deps = { qc: useQueryClient(), confirmLeave: useConfirmLeave() };
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<{ notFound: boolean; text: string } | null>(null);

  const back = (
    <p className="slds-m-top_small">
      <Link href="/studio">{t("studio.gate.backToProjects")}</Link>
    </p>
  );
  const frame = (body: ReactNode) => <div className="slds-p-around_medium">{body}</div>;

  if (!slugOk) return frame(<><ScopedNotification theme="info">{t("studio.gate.notFoundAny")}</ScopedNotification>{back}</>);
  if (identity.status === "loading") {
    return frame(
      <div className="slds-is-relative slds-p-around_large">
        <Spinner size="medium" label={t("studio.gate.checking")} />
      </div>,
    );
  }
  if (identity.status === "error") {
    return frame(
      <>
        <ScopedNotification theme="error">{t("studio.gate.cannotConfirm")}</ScopedNotification>
        <div className="slds-m-top_small">
          <Button variant="neutral" onClick={identity.retry}>
            {t("studio.gate.retry")}
          </Button>
        </div>
      </>,
    );
  }
  if (!identity.sessionReadable) return frame(<><ScopedNotification theme="info">{t("studio.gate.cannotConfirm")}</ScopedNotification>{back}</>);

  const W = identity.me?.workspace ?? null;
  const target = () => safeNext(next ?? (typeof window === "undefined" ? "" : window.location.pathname + window.location.search), slug);
  const wName = W ? W.tenantName || W.tenantSlug : "";

  let block: Block | null = null;
  if (!W && identity.me?.isPlatformOperator !== true) {
    block = { message: t("studio.gate.notOpenable") };
  } else if (ctx === "nha") {
    if (W) block = { message: tf("studio.gate.homeOnly", { name: wName }), action: { label: t("studio.gate.homeAction"), run: (n) => exitProject(n, deps) } };
  } else if (W?.tenantSlug === slug) {
    // the tab is building for this very project
  } else if (slug === identity.home) {
    if (W) block = { message: tf("studio.gate.homeWithSession", { name: wName }), action: { label: t("studio.gate.homeWithSessionAction"), run: (n) => exitProject(n, deps) } };
  } else if (W) {
    block = {
      message: tf("studio.gate.sessionOther", { name: wName }),
      action: { label: tf("studio.gate.switchAction", { name: slug }), run: (n) => switchProject(slug, n, deps) },
    };
  } else {
    block = { message: tf("studio.gate.openPrompt", { name: slug }), action: { label: t("studio.gate.openAction"), run: (n) => enterProject(slug, n, deps) } };
  }

  if (!block) return <>{children}</>;

  const act = async () => {
    if (!block?.action) return;
    setBusy(true);
    setProblem(null);
    try {
      await block.action.run(target());
      // Nothing left the page (the user kept unsaved changes): the button must work again. When a latch is up the page
      // is on its way out (or a preview reload is), and a second click would only repeat the call.
      if (!isWorkspaceLeaving() && !isPreviewExiting()) setBusy(false);
    } catch (e) {
      setProblem({ notFound: e instanceof ApiError && e.status === 404, text: errorMessage(e) });
      setBusy(false);
    }
  };

  return frame(
    <>
      <ScopedNotification theme="info">{problem?.notFound ? tf("studio.gate.notFound", { name: slug }) : block.message}</ScopedNotification>
      {problem && !problem.notFound && (
        <div className="slds-m-top_small">
          <ScopedNotification theme="error">{problem.text}</ScopedNotification>
        </div>
      )}
      {block.action && !problem?.notFound && (
        <div className="slds-m-top_small">
          <Button variant="brand" onClick={() => void act()} disabled={busy}>
            {block.action.label}
          </Button>
        </div>
      )}
      {(!block.action || problem?.notFound) && back}
    </>,
  );
}
