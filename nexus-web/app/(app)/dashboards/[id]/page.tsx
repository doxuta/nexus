"use client";

import { Suspense, use } from "react";
import { DashboardViewer } from "@/components/analytics/DashboardViewer";
import { Spinner } from "@/components/slds/Spinner";

// /dashboards/:id — the viewer + admin authoring live in components/analytics/DashboardViewer (also mounted
// inside an app workshop, queue #302).
export default function DashboardViewerPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = use(params);
  // useSearchParams (trong useRunFilters) cần Suspense boundary — Next 16 build rule.
  return (
    <Suspense
      fallback={
        <div className="relative min-h-40 flex-1">
          <Spinner />
        </div>
      }
    >
      <DashboardViewer id={id} />
    </Suspense>
  );
}
