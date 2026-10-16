"use client";

// Vận hành pages that are more than one mounted screen (specs/073 kh4 T273, T275): a screen with the related column of its
// sibling pages (the same 300px column as Tự động in Dựng), the rule picker of "Lần chạy luật", and "Dữ liệu nhận vào",
// (Dữ liệu nhận vào now has its own page, operate/InboundDeliveriesPage). A related link is drawn only for a page that is
// mounted (FR-132), so the column grows by itself as the pending pages land.
import Link from "next/link";
import { useRouter } from "next/navigation";
import type { ReactNode } from "react";
import { listWorkflows } from "@/lib/api";
import { t } from "@/lib/i18n";
import { isMounted, studioHref, type RouteId } from "@/lib/studioRoutes";
import { Select } from "@/components/slds/Select";
import { SubPage } from "./AutomationPage";
import { QueryState } from "./QueryState";
import { useProjectQuery } from "./useProjectQuery";
import { WorkflowRunHistory } from "./WorkflowRunHistory";

export const AUTO_PAGES = [
  "van-hanh/tu-dong/lan-chay",
  "van-hanh/tu-dong/email",
  "van-hanh/tu-dong/viec-nen",
  "van-hanh/tu-dong/cong-nhan",
  "van-hanh/tu-dong/khoa-ket-noi",
  "van-hanh/tu-dong/khoa-truy-cap",
] as const;
export const USER_PAGES = ["van-hanh/nguoi-dung/dang-nhap-mot-lan", "van-hanh/nguoi-dung/nhat-ky"] as const;

const LABEL = {
  "van-hanh/nguoi-dung/dang-nhap-mot-lan": "studio.route.operSso",
  "van-hanh/nguoi-dung/nhat-ky": "studio.route.operAudit",
  "van-hanh/tu-dong/lan-chay": "studio.route.operRuns",
  "van-hanh/tu-dong/email": "studio.route.operEmail",
  "van-hanh/tu-dong/viec-nen": "studio.route.operJobs",
  "van-hanh/tu-dong/cong-nhan": "studio.route.operInbound",
  "van-hanh/tu-dong/khoa-ket-noi": "studio.route.operCredentials",
  "van-hanh/tu-dong/khoa-truy-cap": "studio.route.operKeys",
} as const;

export function WithRelated({ slug, pages, children }: { slug: string; pages: readonly (keyof typeof LABEL)[]; children: ReactNode }) {
  const live = pages.filter((id) => isMounted(id));
  return (
    <div className="flex flex-col items-stretch gap-4 lg:flex-row lg:items-start">
      <div className="min-w-0 flex-1">{children}</div>
      {live.length > 0 && (
        <aside className="w-full shrink-0 px-4 pt-4 lg:w-[300px] lg:pl-0" aria-label={t("studio.operate.related")}>
          <h2 className="slds-text-heading_small slds-m-bottom_x-small">{t("studio.operate.related")}</h2>
          <ul className="slds-has-dividers_bottom">
            {live.map((id) => (
              <li key={id} className="slds-item">
                <Link href={studioHref(slug, id as RouteId)}>{t(LABEL[id])}</Link>
              </li>
            ))}
          </ul>
        </aside>
      )}
    </div>
  );
}

export const backTo = (slug: string, id: RouteId, labelKey: "studio.route.operUsers" | "studio.route.operAuto") => ({
  label: t(labelKey),
  href: studioHref(slug, id),
});

/** `?luat=` picks the rule; without it the page lists the rules (one link each), never an empty panel. */
export function RunsPage({ slug, ruleId }: { slug: string; ruleId?: string }) {
  const rules = useProjectQuery(slug, ["rules"], listWorkflows);
  const here = studioHref(slug, "van-hanh/tu-dong/lan-chay");
  const router = useRouter();
  const rule = ruleId ? rules.data?.find((w) => w.id === ruleId) : undefined;
  return (
    <SubPage back={backTo(slug, "van-hanh/tu-dong", "studio.route.operAuto")}>
      <QueryState query={rules} empty={<p className="slds-p-horizontal_medium slds-text-color_weak">{t("studio.operate.runs.empty")}</p>}>
        {ruleId && rule ? (
          <>
            <p className="slds-p-horizontal_medium slds-m-bottom_small">
              <Link href={here}>{`← ${t("studio.route.operRuns")}`}</Link>
            </p>
            <div className="slds-p-horizontal_medium slds-m-bottom_small" style={{ maxWidth: "24rem" }}>
              <Select
                label={t("studio.operate.runs.rule")}
                placeholder={false}
                value={rule.id}
                options={(rules.data ?? []).map((w) => ({ value: w.id, label: w.name }))}
                onChange={(e) => router.push(studioHref(slug, "van-hanh/tu-dong/lan-chay", {}, { luat: e.target.value }))}
              />
            </div>
            <WorkflowRunHistory key={rule.id} workflowId={rule.id} entityName={rule.entityName} />
          </>
        ) : (
          <div className="slds-p-horizontal_medium">
            <p className="nx-lede">{t("studio.operate.runs.pick")}</p>
            <ul className="slds-has-dividers_bottom slds-m-top_small">
              {(rules.data ?? []).map((w) => (
                <li key={w.id} className="slds-item">
                  <Link href={studioHref(slug, "van-hanh/tu-dong/lan-chay", {}, { luat: w.id })}>{w.name}</Link>
                </li>
              ))}
            </ul>
          </div>
        )}
      </QueryState>
    </SubPage>
  );
}
