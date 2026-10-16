"use client";

// "Độ sẵn sàng của từng app" (specs/073 kh4 T274, architecture 9.7 lát 1): the server's three conditions an app needs
// before it can be handed over (services.AppService.Readiness), one checklist per app, each row an icon AND words —
// never colour alone. A fix link is drawn only when the page it opens is mounted (FR-132); "Bù quyền còn thiếu" is the
// existing dry-run-then-confirm control. specs/079 f7-kham-san-sang: lives under the health check; the reads run at most 4 at a time.
import clsx from "clsx";
import { Check, CircleAlert } from "lucide-react";
import Link from "next/link";
import type { ReactNode } from "react";
import { Icon } from "@/components/slds/Icon";
import { adminApp, adminApps, type AdminApp, type AdminAppDetail } from "@/lib/api";
import { t } from "@/lib/i18n";
import { isMounted, studioHref } from "@/lib/studioRoutes";
import { AclBackfillControl } from "../AclBackfillControl";
import { QueryState } from "../QueryState";
import { useProjectQuery } from "../useProjectQuery";

function Row({ ok, label, children }: { ok: boolean; label: string; children?: ReactNode }) {
  return (
    <li className="slds-item">
      <div className="flex items-center gap-2">
        {ok ? (
          <Icon icon={Check} size="x-small" tone="success" label={t("page.appOverview.readiness.pass")} />
        ) : (
          <Icon icon={CircleAlert} size="x-small" tone="warning" label={t("page.appOverview.readiness.fail")} />
        )}
        <span className={clsx(ok && "slds-text-color_weak")}>{label}</span>
      </div>
      {children && <div className="flex flex-wrap items-center gap-x-3 gap-y-1 slds-m-top_xx-small">{children}</div>}
    </li>
  );
}

export const READ_CONCURRENCY = 4;
export interface AppReading { app: AdminApp; detail: AdminAppDetail | null }

// One list read, then one detail read per app, at most READ_CONCURRENCY in flight. A detail that fails is null ("not read"), never green.
export async function readAppsReadiness(): Promise<AppReading[]> {
  const apps = await adminApps();
  const out: AppReading[] = apps.map((app) => ({ app, detail: null }));
  let next = 0;
  const worker = async () => {
    while (next < apps.length) {
      const i = next++;
      out[i].detail = await adminApp(apps[i].id).catch(() => null);
    }
  };
  await Promise.all(Array.from({ length: Math.min(READ_CONCURRENCY, apps.length) }, worker));
  return out;
}

function AppChecklist({ slug, reading, onApplied }: { slug: string; reading: AppReading; onApplied: () => void }) {
  const { app } = reading;
  const r = reading.detail?.readiness;
  return (
    <section className="slds-m-top_medium">
      <h3 className="slds-text-heading_small">{app.label || app.name}</h3>
      {r ? (
        <>
          <p className="slds-m-top_xx-small">{t(r.ready ? "page.appOverview.readiness.ready" : "page.appOverview.readiness.notReady")}</p>
          <ul className="slds-has-dividers_top-space slds-m-top_x-small">
            <Row ok={r.allEntitiesHaveFields} label={t("page.appOverview.readiness.fields")}>
              {!r.allEntitiesHaveFields && (
                <>
                  <span>{(r.entitiesMissingFields ?? []).join(", ")}</span>
                  <Link href={studioHref(slug, "dung/du-lieu", {}, { app: app.name })}>{t("studio.maintain.check.open")}</Link>
                </>
              )}
            </Row>
            <Row ok={r.hasNonAdminViewRole} label={t("page.appOverview.readiness.role")}>
              {!r.hasNonAdminViewRole && isMounted("dung/nguoi-dung-quyen") && (
                <Link href={studioHref(slug, "dung/nguoi-dung-quyen")}>{t("page.appOverview.readiness.grant")}</Link>
              )}
              <AclBackfillControl appId={app.id} appLabel={app.label || app.name} onApplied={onApplied} />
            </Row>
            <Row ok={r.hasNonAdminUserAssigned} label={t("page.appOverview.readiness.user")}>
              {!r.hasNonAdminUserAssigned && <Link href={studioHref(slug, "van-hanh/nguoi-dung")}>{t("page.appOverview.readiness.addUser")}</Link>}
            </Row>
          </ul>
        </>
      ) : (
        <p className="slds-text-color_weak">{t("studio.maintain.readiness.unread")}</p>
      )}
    </section>
  );
}

export function ReadinessSection({ slug }: { slug: string }) {
  const q = useProjectQuery(slug, ["readiness"], readAppsReadiness);
  return (
    <div className="slds-p-top_medium slds-m-top_large slds-border_top">
      <h2 className="slds-text-heading_medium">{t("studio.maintain.readiness.title")}</h2>
      <QueryState
        query={q}
        empty={
          <p className="slds-text-color_weak slds-m-top_x-small">
            {t("studio.maintain.readiness.noApps")}{" "}
            {isMounted("dung/man-hinh/app") && <Link href={studioHref(slug, "dung/man-hinh/app")}>{t("studio.maintain.readiness.addApp")}</Link>}
          </p>
        }
      >
        {(q.data ?? []).map((x) => (
          <AppChecklist key={x.app.id} slug={slug} reading={x} onApplied={() => void q.refetch()} />
        ))}
        {isMounted("van-hanh/suc-khoe") && (
          <p className="slds-m-top_medium">
            <Link href={studioHref(slug, "van-hanh/suc-khoe")}>{t("studio.maintain.kham.healthLink")}</Link>
          </p>
        )}
      </QueryState>
    </div>
  );
}
