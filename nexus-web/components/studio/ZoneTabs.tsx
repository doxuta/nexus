"use client";

// The four sub tabs of Vận hành and Bảo trì (specs/073 kh4 T273, FR-013, architecture 3.4): the first row of the work
// surface, page links in a `nav` named "Mục trong khu". Below 768px the labels are the short ones and the row scrolls
// inside itself (Tabs links mode) — it never widens the page.
import { Tabs } from "@/components/slds/Tabs";
import { t } from "@/lib/i18n";
import { useOperateAttention } from "@/lib/operateFeed";
import { studioHref, tabOfRoute, ZONE_TABS, type RouteId, type StudioRoute } from "@/lib/studioRoutes";

// Only the Vận hành strip asks for the failures of the last 24h (it shares the overview's queries, FR-079).
function OperateTabs(props: { slug: string; route: StudioRoute | null }) {
  const dot = useOperateAttention(props.slug);
  return <ZoneTabsBody {...props} autoDot={dot} />;
}

export function ZoneTabs(props: { slug: string; route: StudioRoute | null }) {
  return props.route?.segments[0] === "van-hanh" ? <OperateTabs {...props} /> : <ZoneTabsBody {...props} autoDot={false} />;
}

function ZoneTabsBody({ slug, route, autoDot }: { slug: string; route: StudioRoute | null; autoDot: boolean }) {
  const current = tabOfRoute(route);
  const zone = route?.segments[0];
  if (!current || (zone !== "van-hanh" && zone !== "bao-tri")) return null;
  return (
    <Tabs
      mode="links"
      className="nx-tabs-zone"
      label={t("studio.a11y.nav.sub")}
      current={current}
      items={ZONE_TABS[zone].map((x) => ({
        value: x.route,
        label: t(x.label),
        shortLabel: t(x.short),
        href: studioHref(slug, x.route as RouteId),
        dot: x.route === "van-hanh/tu-dong" && autoDot,
      }))}
    />
  );
}
