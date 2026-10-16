"use client";

import { Suspense, use } from "react";
import { ReportViewer } from "@/components/screens/ReportViewer";
import { Spinner } from "@/components/slds/Spinner";

// /reports/[id] — report viewer + admin config panel (SLDS; specs/038 T030).
export default function ReportViewerPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = use(params);
  // useSearchParams (inside useRunFilters) needs a Suspense boundary — Next 16 build rule.
  return (
    <Suspense
      fallback={
        <div className="relative min-h-0 flex-1">
          <Spinner size="medium" />
        </div>
      }
    >
      <ReportViewer id={id} />
    </Suspense>
  );
}
