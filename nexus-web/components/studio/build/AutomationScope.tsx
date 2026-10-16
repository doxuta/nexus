"use client";

// AutomationScope (specs/074 lát f2-automation-approval-queue): the part ApprovalPage and QueuePage share — read the
// page's `?app=`, say so plainly when the app is unknown / failed to load / is still loading, and only then hand the
// builder the tables of that app. The app filter sits on top; the scope is the URL, never the shared "active app" lens.
import Link from "next/link";
import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { appsCatalog } from "@/lib/api";
import { useAppEntityNames } from "@/lib/appScope";
import { qk } from "@/lib/query/keys";
import { t, tf } from "@/lib/i18n";
import { studioHref, type RouteId } from "@/lib/studioRoutes";
import { AppFilter } from "@/components/studio/AppFilter";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { EmptyState } from "@/components/slds/EmptyState";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";

export interface ScopeProps {
  entityNames: Set<string> | null;
  appLabel: string | null;
  showAllHref: string;
}

export function AutomationScope({ slug, route, app, bang, noun, children }: {
  slug: string;
  route: RouteId;
  app?: string;
  bang?: string;
  noun: string;
  children: (s: ScopeProps) => ReactNode;
}) {
  const appCode = app ?? null;
  const appsQ = useQuery({ queryKey: qk.apps(), queryFn: appsCatalog, staleTime: 60_000 });
  const scope = useAppEntityNames(appCode);
  const allHref = studioHref(slug, route, {}, { bang });

  let body: ReactNode;
  if (scope.unknown) {
    body = (
      <EmptyState plain className="slds-p-around_medium" title={tf("studio.build.data.unknownApp", { app: appCode ?? "" })} description={t("studio.build.data.unknownAppDesc")}
        action={<Link href={allHref}>{t("studio.build.data.showAll")}</Link>} />
    );
  } else if (scope.isLoading) {
    body = <div className="slds-is-relative slds-p-around_xx-large"><Spinner size="medium" label={t("studio.build.automation.scope.loading")} /></div>;
  } else if (scope.error) {
    body = (
      <div className="slds-p-around_medium">
        <ScopedNotification theme="error">{t("studio.build.data.error.app")}</ScopedNotification>
        <p className="slds-m-top_small"><Link href={allHref}>{t("studio.build.data.showAll")}</Link></p>
      </div>
    );
  } else {
    body = children({ entityNames: scope.entityNames, appLabel: scope.app?.label ?? null, showAllHref: allHref });
  }

  return (
    <EmbeddedSurface>
      <div className="flex min-h-0 flex-1 flex-col">
        <div className="slds-p-horizontal_medium slds-p-vertical_small">
          <AppFilter slug={slug} route={route} apps={appsQ.data ?? []} value={appCode} hiddenCount={0} hiddenNoun={noun} query={{ bang }} />
        </div>
        {body}
      </div>
    </EmbeddedSurface>
  );
}
