"use client";

// DashboardPage (specs/074 lát f2-dashboard-viewer-extract): `dung/bao-cao/tong-quan/<id>` — the customer's own
// dashboard viewer (filters, widget grid, admin authoring) shown inside the workshop; deleting returns to Báo cáo.
import { Suspense } from "react";
import { useRouter } from "next/navigation";
import { DashboardViewer } from "@/components/analytics/DashboardViewer";
import { Spinner } from "@/components/slds/Spinner";
import { SubPage } from "@/components/studio/AutomationPage";
import { t } from "@/lib/i18n";
import { studioHref } from "@/lib/studioRoutes";

export function DashboardPage({ slug, id }: { slug: string; id: string }) {
  const router = useRouter();
  const back = studioHref(slug, "dung/bao-cao");
  return (
    <SubPage back={{ label: t("studio.build.reports.dashboard.back"), href: back }}>
      {/* useSearchParams (run filters) needs a Suspense boundary */}
      <Suspense fallback={<div className="relative min-h-40 flex-1"><Spinner /></div>}>
        <DashboardViewer id={id} embedded onDeleted={() => router.push(back)} />
      </Suspense>
    </SubPage>
  );
}
