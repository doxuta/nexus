"use client";

// "App đã giao cho khách" (specs/077 f5-handoff-deliver): the result block that replaces the checklist once the first
// version is out. No re-deliver button. The owner warning only appears when the owner source was READ and holds no
// usable account; a failed read says "could not check" with a retry, never the false warning.
import Link from "next/link";
import { useEffect, useRef } from "react";
import { Button, buttonClassName } from "@/components/slds/Button";
import type { PublishState } from "@/lib/api";
import { usableOwners } from "@/lib/handoffChecklist";
import type { AdminUser } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import { studioHref } from "@/lib/studioRoutes";

export type OwnerRead = { users: AdminUser[] } | "error" | undefined;

export function DeliveredNotice({ slug, state, owner, onRetryOwner }: { slug: string; state: PublishState; owner: OwnerRead; onRetryOwner: () => void }) {
  const head = state.headVersion;
  const at = head?.publishedAt ? new Date(head.publishedAt).toLocaleString("vi-VN") : "";
  const known = owner && owner !== "error" && owner.users.some((u) => typeof u.canManageUsers === "boolean");
  // The confirm button that held focus is gone: land on the result so keyboard and screen-reader users hear it.
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => ref.current?.focus({ preventScroll: true }), []);
  return (
    <div ref={ref} tabIndex={-1}>
      <p className="slds-text-heading_small">{tf("studio.build.handover.done.title", { n: head?.versionNo ?? 1, at })}</p>
      <p className="slds-m-top_x-small">{t("studio.build.handover.done.hint")}</p>
      {owner === "error" && (
        <p className="slds-m-top_small">
          {t("studio.build.handover.row.owner.unknown")}{" "}
          <Button variant="neutral" onClick={onRetryOwner}>
            {t("studio.build.handover.action.retry")}
          </Button>
        </p>
      )}
      {known && usableOwners((owner as { users: AdminUser[] }).users).length === 0 && (
        <p className="slds-m-top_small">
          {t("studio.build.handover.done.noOwner")}{" "}
          <Link className={buttonClassName("neutral")} href={studioHref(slug, "van-hanh/nguoi-dung")}>
            {t("studio.build.handover.done.openUsers")}
          </Link>
        </p>
      )}
    </div>
  );
}
