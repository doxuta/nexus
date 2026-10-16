"use client";

// AppHomeDashboard — the dashboard an app's builder picked as the app's home page (sys_apps
// homeDashboardId). Home leads with it, so a receptionist opening the clinic app sees the clinic's
// numbers first. Compact summary only: scalar widgets as tiles, grouped widgets as their top rows;
// the full viewer (filters, charts, drill) is one click away. A dashboard that no longer runs
// (deleted, no access) renders nothing — Home keeps working without it.
import { useQuery } from "@tanstack/react-query";
import { LayoutGrid } from "lucide-react";
import Link from "next/link";
import { runDashboard, type DashboardWidgetDef, type WidgetRun } from "@/lib/api";
import { formatWidgetValue } from "@/lib/analytics";
import { qk } from "@/lib/query/keys";
import { t } from "@/lib/i18n";
import { Card } from "@/components/slds/Card";
import { EmptyState } from "@/components/slds/EmptyState";
import { Tile } from "@/components/slds/Tile";

const MAX_WIDGETS = 6;
const MAX_GROUP_ROWS = 3;

function WidgetTile({ run, def }: { run: WidgetRun; def?: DashboardWidgetDef }) {
  const res = run.result;
  if (run.error || !res) return null;
  const metric = def?.metric;
  return (
    <li>
      <Tile title={run.title}>
        {res.grouped ? (
          <ul className="slds-text-body_small">
            {(res.groups ?? []).slice(0, MAX_GROUP_ROWS).map((g) => (
              <li key={g.key} className="slds-truncate">
                {g.label ?? g.key}: <span className="tabular-nums">{formatWidgetValue(g.value, metric, res.currencyCode)}</span>
              </li>
            ))}
          </ul>
        ) : (
          <p className="slds-text-heading_large tabular-nums">{formatWidgetValue(res.value, metric, res.currencyCode)}</p>
        )}
      </Tile>
    </li>
  );
}

export function AppHomeDashboard({ dashboardId }: { dashboardId: string }) {
  const { data, isLoading, isError } = useQuery({
    queryKey: qk.dashboardRun(dashboardId),
    queryFn: () => runDashboard(dashboardId),
    staleTime: 60_000,
    retry: false,
  });
  if (isError) return null;

  const defs = new Map((data?.dashboard.widgets ?? []).map((w) => [w.id, w]));
  const shown = (data?.widgets ?? []).filter((w) => !w.error && w.result);
  const tiles = shown.slice(0, MAX_WIDGETS);
  const more = shown.length - tiles.length;
  const href = `/dashboards/${dashboardId}`;

  return (
    <Card
      title={data?.dashboard.name ?? t("page.home.dashboards")}
      icon={{ glyph: LayoutGrid, name: "dashboard" }}
      href={href}
      loading={isLoading}
      footer={
        <Link className="slds-card__footer-action" href={href}>
          {more > 0 ? `${t("page.home.appDashboardOpen")} · ${t("page.home.appDashboardMore").replace("{n}", String(more))}` : t("page.home.appDashboardOpen")}
        </Link>
      }
    >
      {!isLoading && tiles.length === 0 ? (
        <EmptyState title={t("page.home.appDashboardEmpty")} />
      ) : (
        <ul className="grid grid-cols-2 gap-4 sm:grid-cols-3" data-testid="app-home-dashboard">
          {tiles.map((w) => <WidgetTile key={w.widgetId} run={w} def={defs.get(w.widgetId)} />)}
        </ul>
      )}
    </Card>
  );
}
