"use client";

// TeamsPage (specs/074 lát f2-access-team-share, T220): "Đội" on its own URL (dung/nguoi-dung-quyen/doi). The list,
// the detail and the four states (loading, error, empty, rows) are TeamsList's; `inline` makes "+ Đội mới" a form in
// the page and the two deletes ask in the page, so no dialog opens here. The customer's /admin/teams keeps the Modal.
import { TeamsList } from "@/components/screens/admin/TeamsList";
import { t } from "@/lib/i18n";
import { studioHref } from "@/lib/studioRoutes";
import { SubPage } from "../AutomationPage";

export function TeamsPage({ slug }: { slug: string }) {
  return (
    <SubPage back={{ label: t("studio.route.access"), href: studioHref(slug, "dung/nguoi-dung-quyen") }}>
      <TeamsList inline />
    </SubPage>
  );
}
