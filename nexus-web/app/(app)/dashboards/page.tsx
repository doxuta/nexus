import { Suspense } from "react";
import { DashboardsScreen } from "@/components/screens/DashboardsScreen";

// /dashboards — dashboard catalogue (screen lives in components/screens/DashboardsScreen, also mounted in the workshop, #302).
// Suspense: the screen reads ?draft=1 (useSearchParams) — Next 16 build rule.
export default function DashboardsPage() {
  return (
    <Suspense fallback={null}>
      <DashboardsScreen />
    </Suspense>
  );
}
