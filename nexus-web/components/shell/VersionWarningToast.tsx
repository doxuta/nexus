"use client";

// VersionWarningToast — specs/063 S7-5 (FR-033): a change that applied at once but whose version-history
// entry failed to write. req() raises the event on any 2xx carrying `versionWarning`; this shows ONE toast.
import { useEffect } from "react";
import { VERSION_WARNING_EVENT } from "@/lib/api";
import { t } from "@/lib/i18n";
import { toast } from "@/components/ui/Toaster";

export function VersionWarningToast() {
  useEffect(() => {
    const onWarn = () => toast.warning(t("direct.warning"), 9000);
    window.addEventListener(VERSION_WARNING_EVENT, onWarn);
    return () => window.removeEventListener(VERSION_WARNING_EVENT, onWarn);
  }, []);
  return null;
}
