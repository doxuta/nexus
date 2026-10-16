"use client";

// The way out of a context switch that failed after the old session was closed (specs/073 FR-085(3)): the old work surface
// cannot be re-mounted (its token is gone), so the page says so and offers two full loads. Drawn by the project frame
// inside a project and by the Studio shell on every other route — the header's switcher works on both.
import { useEffect, useRef } from "react";
import { Button } from "@/components/slds/Button";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { t } from "@/lib/i18n";

export function SwitchFailedPanel() {
  const row = useRef<HTMLDivElement>(null);
  // The button that was clicked is gone with the old surface, so focus would fall to <body>: put it on "Thử lại".
  useEffect(() => {
    row.current?.querySelector("button")?.focus();
  }, []);
  return (
    <div className="slds-p-around_large">
      <ScopedNotification theme="error">{t("studio.frame.switchFailed")}</ScopedNotification>
      <div ref={row} className="slds-m-top_small slds-grid slds-grid_vertical-align-center">
        <Button variant="brand" onClick={() => window.location.reload()}>
          {t("studio.gate.retry")}
        </Button>
        <Button variant="neutral" className="slds-m-left_small" onClick={() => window.location.assign("/studio")}>
          {t("studio.gate.backToProjects")}
        </Button>
      </div>
    </div>
  );
}
