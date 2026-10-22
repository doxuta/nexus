"use client";

// ProjectFrame — the Studio project frame (specs/073 FR-011..FR-019, FR-025, FR-085; architecture 3.x):
//   project strip (name · three zones) → build-steps bar (Dựng zone only) → ONE work surface (a single card holding
//   the step line and the page). The page arrives as `children`; the frame decides whether it may mount at all.
// What this file guarantees, because every builder inside depends on it:
//   • the surface sits behind ContextGate: a token that does not belong to this project never mounts a builder;
//   • `key={slug}` on the surface: a project change drops every local state (typed text, armed confirms, open dialogs);
//   • while a context switch is in flight the surface is UNMOUNTED (spinner only); a session that ends under the tab
//     freezes it the same way (StudioSessionBanner clears the cache and reloads, never router.push: the heap would keep
//     customer A's data);
//   • focus and the document title follow the page (useStudioRouteFocus), landmarks are named.
// A project that cannot be opened (unknown code, no session) shows only its code as the h1 and the gate's sentence:
// zones and steps would promise places there is nowhere to go.
import { usePathname, useRouter } from "next/navigation";
import { useMemo, useRef, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { listEntities } from "@/lib/api";
import type { BuildStepKey } from "@/lib/buildSteps";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { PageHeader } from "@/components/slds/PageHeader";
import { Path } from "@/components/slds/Path";
import { Spinner } from "@/components/slds/Spinner";
import { useFacePaths } from "@/components/shell/ModeSwitch";
import { FramePublishState } from "@/components/shell/useDirectApply";
import { buildSteps, STEP_ROUTE, stepOfRoute } from "@/lib/buildSteps";
import { t, tf } from "@/lib/i18n";
import {
  useReloadOnIdentityStorage,
  useStudioIdentity,
  useStudioTransition,
  useStudioTransitionFailed,
} from "@/lib/studioContext";
import { parseStudioPath, SLUG_RE, studioHref, zoneOfRoute } from "@/lib/studioRoutes";
import { AssistantMount } from "./assistant/AssistantMount";
import { AssistantProvider } from "./assistant/AssistantProvider";
import { ProposalOverlayProvider } from "./assistant/ProposalOverlay";
import { ContextGate } from "./ContextGate";
import { DraftBar } from "./DraftBar";
import { ProjectHeader } from "./ProjectHeader";
import { usePublishState } from "./usePublishState";
import { StepHeader } from "./StepHeader";
import { SwitchFailedPanel } from "./SwitchFailedPanel";
import { useStudioDocumentTitle, useStudioRouteFocus } from "./useStudioRouteFocus";
import { ZoneTabs } from "./ZoneTabs";

// The data step is inferred from the project's real tables (same cache entry as the table list). Rendered inside
// ContextGate, so the call happens only once the token is known to belong to this project. The workshop (home) knows
// nothing: null. Other steps stay null until the project row (so1a) feeds them. Loading/error: null, never a guessed "Chưa đủ".
function FrameStepHeader({ step, home }: { step: BuildStepKey; home: boolean }) {
  const on = step === "data" && !home;
  const q = useQuery({ queryKey: ["studio", "count", "entities"], queryFn: listEntities, staleTime: 60_000, enabled: on });
  const n = on && q.data ? q.data.length : null;
  if (n === null) return <StepHeader step={{ key: step, complete: null }} />;
  const sublabel = n >= 1 ? tf("studio.step.data.sub.some", { n }) : t("studio.step.data.sub.none");
  return <StepHeader step={{ key: step, complete: n >= 1, sublabel }} />;
}

export function ProjectFrame({ slug, children }: { slug: string; children: ReactNode }) {
  const pathname = usePathname();
  const router = useRouter();
  const root = useRef<HTMLDivElement>(null);
  const { appHref } = useFacePaths("builder");
  const frozen = useStudioTransition();
  const switchFailed = useStudioTransitionFailed();
  const slugOk = SLUG_RE.test(slug);
  const identity = useStudioIdentity({ enabled: slugOk });
  useReloadOnIdentityStorage();

  const parsed = useMemo(() => parseStudioPath(pathname), [pathname]);
  const route = parsed?.route ?? null;
  const open = slugOk && identity.matches(slug);
  const W = identity.me?.workspace ?? null;
  const home = open && !W;
  const name = !open ? slug : home ? t("studio.project.home") : W?.tenantName || W?.tenantSlug || slug;
  const { data: publishState, error: publishError } = usePublishState(slug);
  // useDirectApply on the page answers from this read instead of one of its own per announcement (#53).
  const framePublish = useMemo(() => ({ data: publishState, error: publishError }), [publishState, publishError]);
  const step = stepOfRoute(route);
  const zone = zoneOfRoute(route);

  useStudioRouteFocus(root, slug, parsed?.root ? "" : (route?.id ?? pathname), identity.status !== "loading");
  useStudioDocumentTitle(route ? t(route.labelKey) : t("studio.notFound.title"), name);

  // A session that ends under the tab is handled once, by StudioSessionBanner (StudioShell): it freezes the frame through
  // beginTransition() (this component unmounts the surface on that), clears the cache and loads /studio afresh.

  const steps = useMemo(
    () =>
      buildSteps(null, { home }).map((s) => ({
        key: s.key,
        label: t(`studio.step.${s.key}.label` as const),
        complete: s.complete,
        href: studioHref(slug, STEP_ROUTE[s.key]),
      })),
    [home, slug],
  );

  const surface = (
    <FramePublishState value={framePublish}>
      <ZoneTabs slug={slug} route={route} />
      {step && <FrameStepHeader step={step} home={home} />}
      <EmbeddedSurface>{children}</EmbeddedSurface>
    </FramePublishState>
  );

  return (
    <div ref={root} className="flex min-h-0 flex-1 flex-col overflow-y-auto" data-testid="project-frame">
      <section aria-label={name} className="shrink-0">
        {open ? (
          <ProjectHeader
            slug={slug}
            identity={{ name }}
            zone={zone}
            home={home}
            openApp={() => router.push(appHref)}
            publish={publishState && { delivered: publishState.delivered, pendingCount: publishState.pendingCount, versionNo: publishState.headVersion?.versionNo, schemaRetry: !!publishState.schemaRetry }}
            actions={home ? [] : [<DraftBar key="draft" slug={slug} />]} // no first-publish button in the workshop (FR-064); once delivered (the demo seed does it) ProjectHeader shows its publish status
          />
        ) : (
          <div className="slds-theme_default slds-border_bottom">
            <PageHeader title={name} wrapTitle className="slds-page-header_joined slds-theme_default border-0" />
          </div>
        )}
      </section>
      {open && zone === "dung" && step && !frozen && (
        <div className="shrink-0 slds-m-horizontal_medium slds-m-top_small">
          <Path mode="steps" label={t("studio.a11y.nav.steps")} steps={steps} current={step} />
        </div>
      )}
      <main id="nx-main" tabIndex={-1} className="min-h-0 flex-1 p-3">
        <article className="slds-card">
          <div className="slds-card__body" key={slug}>
            {switchFailed ? (
              // The old surface is never mounted again (its session is closed); both ways out load the page afresh.
              <SwitchFailedPanel />
            ) : frozen ? (
              <div className="slds-is-relative slds-p-around_xx-large">
                <Spinner size="medium" label={t("studio.frame.switching")} />
              </div>
            ) : (
              <ContextGate slug={slug} ctx={route?.ctx ?? "phien"}>
                {/* `nha` pages call operator routes: no builder (the assistant included) may mount there. */}
                {route && route.ctx === "phien" ? (
                  <ProposalOverlayProvider slug={slug}>
                    <AssistantProvider slug={slug} projectName={name} route={route} params={parsed?.params ?? {}} panel={<AssistantMount />}>{surface}</AssistantProvider>
                  </ProposalOverlayProvider>
                ) : surface}
              </ContextGate>
            )}
          </div>
        </article>
      </main>
    </div>
  );
}
