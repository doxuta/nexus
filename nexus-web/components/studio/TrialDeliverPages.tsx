"use client";

// Báo cáo, Chạy thử and Giao cho khách (specs/073 kh3 T235, T237, T238). Each is a thin page over screens that already
// exist; what is not built says so in one sentence (FR-132) — no dead button, no fake customer frame.
import Link from "next/link";
import { useEffect, useState } from "react";
import { TrialBar } from "./trial/TrialBar";
import { usePublishState } from "./usePublishState";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { Select } from "@/components/slds/Select";
import { adminApps, type AdminApp } from "@/lib/api";
import { t } from "@/lib/i18n";
import { QueryState } from "./QueryState";
import { SampleDataByTable } from "./trial/SampleDataByTable";
import { TrialRolesTable } from "./trial/TrialRolesTable";
import { TrialStageSwitch } from "./trial/TrialStageSwitch";
import { useProjectQuery } from "./useProjectQuery";

function TrialApp({ app }: { app: AdminApp }) {
  return (
    <p className="slds-m-top_small">
      {/* ?app= makes the sidebar start on this app (lib/apps.tsx: the query wins once). */}
      <Link href={`/home?app=${encodeURIComponent(app.name)}`} aria-label={`${t("studio.build.trial.openApp")} ${app.label || app.name}`}>
        {t("studio.build.trial.openApp")}
      </Link>
    </p>
  );
}

// F5 (queue #571/#570) replaces this page with the real trial and handover steps.
export function TrialPage({ slug }: { slug: string }) {
  const apps = useProjectQuery(slug, ["apps"], adminApps);
  // The toggle exists only when delivered with a draft; default = the server's own (draft).
  const [draft, setDraft] = useState(true);
  const pub = usePublishState(slug).data;
  const showToggle = !!pub?.delivered && pub.pendingCount > 0;
  const router = useRouter();
  const pathname = usePathname();
  const params = useSearchParams();
  const wanted = params.get("app");
  const list = apps.data ?? [];
  // The app comes from the project's own list, never from the raw URL string (a stale or foreign ?app= falls back).
  const app = list.find((a) => a.name === wanted) ?? list.find((a) => a.isDefault) ?? list[0] ?? null;
  // A foreign / stale ?app= is dropped so the URL never names an app other than the one shown.
  const unknownWanted = !!wanted && !!app && app.name !== wanted;
  useEffect(() => {
    if (!unknownWanted) return;
    const rest = new URLSearchParams(params.toString());
    rest.delete("app");
    const q = rest.toString();
    router.replace(q ? `${pathname}?${q}` : pathname);
  }, [unknownWanted, router, pathname, params]);
  return (
    <div className="slds-p-around_medium">
      {/* The step frame above already carries the one-line lede (studio.step.trial.desc). */}
      <TrialStageSwitch slug={slug} />
      <QueryState query={apps} empty={<p className="slds-text-color_weak slds-m-top_x-small">{t("studio.maintain.check.noApps")}</p>}>
        {list.length > 1 && app && (
          <Select
            label={t("studio.build.trial.app")}
            value={app.name}
            options={list.map((a) => ({ value: a.name, label: a.label || a.name }))}
            placeholder={false}
            onChange={(e) => router.replace(`${pathname}?app=${encodeURIComponent(e.target.value)}`)}
          />
        )}
        <TrialBar slug={slug} app={app} draft={draft} onDraftChange={setDraft} />
        <TrialRolesTable slug={slug} app={app} draft={showToggle ? draft : undefined} />
        {app && <TrialApp key={app.id} app={app} />}
        <SampleDataByTable slug={slug} />
      </QueryState>
    </div>
  );
}
