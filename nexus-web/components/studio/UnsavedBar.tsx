"use client";

// UnsavedBar (specs/074 T461, FR-072): the Huỷ / Lưu bar of a work page. It shows ONLY while the page holds unsaved
// changes, says so in an aria-live region, and arms the unsaved-changes guard (NavigationBlocker) for the same span.
// DockedFormFooter is `position:fixed` over the whole viewport; here it is made sticky inside the work column so it never
// slides under the assistant pane and never covers the last field (it takes its own height in the flow).
import { useEffect, type ReactNode } from "react";
import { DockedFormFooter } from "@/components/slds/DockedFormFooter";
import { useSetBlocked } from "@/lib/navigation-blocker";
import { t } from "@/lib/i18n";

export interface UnsavedBarProps {
  dirty: boolean;
  /** Show the bar although nothing changed yet (a page that has never been saved). Default: only when dirty. */
  alwaysShow?: boolean;
  saving?: boolean;
  onSave: () => void;
  onCancel: () => void;
  saveLabel?: string;
  /** The real error of the last save: the bar stays and shows it. */
  error?: ReactNode;
}

export function UnsavedBar({ dirty, alwaysShow, saving, onSave, onCancel, saveLabel, error }: UnsavedBarProps) {
  const setBlocked = useSetBlocked();
  useEffect(() => {
    setBlocked(dirty);
    return () => setBlocked(false);
  }, [dirty, setBlocked]);
  return (
    <div className="slds-theme_default slds-border_top sticky bottom-0 z-10 [&_.slds-docked-form-footer]:static" data-testid="unsaved-bar">
      <div aria-live="polite" role="status" className={dirty ? "slds-p-horizontal_medium slds-p-vertical_xx-small slds-text-color_weak" : "slds-assistive-text"}>
        {dirty ? t("studio.unsaved.text") : ""}
      </div>
      {error ? <p role="alert" className="slds-text-color_error slds-p-horizontal_medium" title={typeof error === "string" ? error : undefined}>{error}</p> : null}
      {(dirty || alwaysShow) && <DockedFormFooter onCancel={onCancel} onSave={onSave} saving={saving} saveLabel={saveLabel} />}
    </div>
  );
}
