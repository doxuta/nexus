"use client";

// ScopeHiddenNote — "n <noun> của app khác đang ẩn · Xem tất cả" for the screens mounted in an app workshop
// (queue #302). A text link, not a wall: the filter is soft (INV-2) and the other apps' rows are one click away.

import { Button } from "@/components/slds/Button";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";

export function ScopeHiddenNote({
  hidden, noun, showAll, onToggle,
}: { hidden: number; noun: "roles" | "users" | "reports" | "dashboards"; showAll: boolean; onToggle: () => void }) {
  if (hidden === 0 && !showAll) return null;
  return (
    <p className="slds-text-body_small slds-text-color_weak slds-p-vertical_xx-small" data-testid="scope-hidden-note">
      {!showAll && <>{tf("workshop.scope.hidden", { n: hidden, noun: t(`workshop.scope.noun.${noun}` as I18nKey) })}{" "}</>}
      <Button variant="base" onClick={onToggle}>{t(showAll ? "workshop.scope.showApp" : "workshop.scope.showAll")}</Button>
    </p>
  );
}
