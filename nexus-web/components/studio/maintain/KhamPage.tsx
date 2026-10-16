"use client";

// bao-tri/kham: the health check, told whether the project is delivered so a fix that reaches real users at once says so
// (specs/079 FR-026). Anything but a clear "not delivered" is read as delivered: fail closed.
import { DoctorScreen } from "@/components/screens/admin/DoctorScreen";
import { isEngineMissing } from "@/lib/engineGap";
import type { DeliveredState } from "@/lib/doctorView";
import { PackUpgradeNotice } from "./PackUpgradeNotice";
import { homeSlug } from "@/lib/studioContext";
import { t } from "@/lib/i18n";
import { ReadinessSection } from "./ReadinessSection";
import { usePublishState } from "../usePublishState";

export function KhamPage({ slug }: { slug: string }) {
  const publish = usePublishState(slug);
  let state: DeliveredState = "unknown";
  if (publish.data && typeof publish.data.delivered === "boolean") state = publish.data.delivered ? "delivered" : "not-delivered";
  else if (publish.isError && isEngineMissing(publish.error)) state = "no-concept";
  return (
    <div>
      <DoctorScreen deliveredState={state} />
      <PackUpgradeNotice slug={slug} />
      {slug !== homeSlug() && <p className="slds-text-body_small slds-text-color_weak slds-m-top_small">{t("studio.maintain.kham.snapshotNote")}</p>}
      <ReadinessSection slug={slug} />
    </div>
  );
}
