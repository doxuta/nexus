"use client";

// CalendarPage (specs/074 lát f2-automation-hook-calendar): `dung/tu-dong/lich-lam-viec` — the working calendars shared by
// the whole project, as a sub page of Tự động. The rule builder's "đặt hạn" action links here through studioHref.
import { useDirectApplyGate } from "@/components/studio/useDirectApplyGate";
import { t } from "@/lib/i18n";
import { studioHref } from "@/lib/studioRoutes";
import { BusinessCalendars } from "@/components/screens/admin/BusinessCalendars";
import { SubPage } from "@/components/studio/AutomationPage";

export function CalendarPage({ slug }: { slug: string }) {
  const applyGate = useDirectApplyGate(slug, "business_calendar");
  return (
    <SubPage back={{ label: t("studio.build.automation.back"), href: studioHref(slug, "dung/tu-dong") }}>
      {applyGate.notice && <p className="slds-m-bottom_small">{applyGate.notice}</p>}
      {applyGate.confirm}
      <BusinessCalendars gate={applyGate.ask} />
    </SubPage>
  );
}
