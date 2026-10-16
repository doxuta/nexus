"use client";

// SharingPage (specs/074 lát f2-access-team-share, T220): "Chia sẻ" on its own URL (dung/nguoi-dung-quyen/chia-se).
// SharingAdmin (how role ACL and team-sets combine + the live team-sets table) in the Studio frame; read-only.
import { SharingAdmin } from "@/components/screens/admin/SharingAdmin";
import { t } from "@/lib/i18n";
import { studioHref } from "@/lib/studioRoutes";
import { SubPage } from "../AutomationPage";

export function SharingPage({ slug }: { slug: string }) {
  return (
    <SubPage back={{ label: t("studio.route.access"), href: studioHref(slug, "dung/nguoi-dung-quyen") }}>
      <SharingAdmin inline />
    </SubPage>
  );
}
