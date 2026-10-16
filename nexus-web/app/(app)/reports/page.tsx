import { Suspense } from "react";
import { ReportsScreen } from "@/components/screens/ReportsScreen";

// /reports — report catalog + admin create (SLDS object home; specs/038 T030).
// Suspense: the screen reads ?draft=1 (useSearchParams) — Next 16 build rule.
export default function ReportsPage() {
  return (
    <Suspense fallback={null}>
      <ReportsScreen />
    </Suspense>
  );
}
