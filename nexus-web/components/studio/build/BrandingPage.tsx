"use client";

// BrandingPage (specs/074 lát f2-branding-preview, T283): dung/thuong-hieu. The tabs drawn in the customer frame are the
// first tables of the REAL customer app (GET /apps/:name/nav, already ACL-filtered), not invented labels.
import { appNav, appsCatalog } from "@/lib/api";
import { BrandingScreen } from "../BrandingScreen";
import { useProjectQuery } from "../useProjectQuery";

const MAX_TABS = 4;

export function BrandingPage({ slug }: { slug: string }) {
  const tabs = useProjectQuery(slug, ["branding", "nav"], async () => {
    const first = (await appsCatalog())[0];
    if (!first) return [];
    const nav = await appNav(first.name);
    return nav.items.filter((i) => i.type === "entity").slice(0, MAX_TABS).map((i) => i.label);
  });
  return <BrandingScreen tabs={tabs.data ?? []} />;
}
