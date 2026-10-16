"use client";

// Cài đặt dự án (specs/073 so2 T481). "Form cài đặt" pattern: group heads (13px/600 + rule), no card per group, the last
// group is the danger zone.
//   Nexus workshop  → read only (kh4): the workshop is not a customer row.
//   customer       → reads the ONE project ledger (GET /admin/projects). The page closes until that answers (FR-027):
//                    loading / error / no row for the slug ⇒ the settings component is never mounted, so no call carries
//                    an empty or guessed tenant id. `tenantId` always comes from the ledger row, never from the URL.
// Name and code are read-only text: PATCH accepts only `industry` and `trial`, so no edit box is drawn.
// Hard delete is NOT here: the server demands a retired project (FR-095), it lives under "Đã ngưng" (T485).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useRouter } from "next/navigation";
import { useState, type ReactNode } from "react";
import { Button } from "@/components/slds/Button";
import { EmptyState } from "@/components/slds/EmptyState";
import { Input } from "@/components/slds/Input";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Toggle } from "@/components/slds/Toggle";
import { healthCheckProject, listProjects, patchProject, retireTenant, type ProjectHealth, type ProjectRow } from "@/lib/api";
import { errorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { homeSlug } from "@/lib/studioContext";
import { studioHref } from "@/lib/studioRoutes";
import { InlineConfirm } from "./InlineConfirm";
import { OwnerTierSection } from "./OwnerTierSection";
import { QueryState } from "./QueryState";

// Shared with the board (so1b): one ledger entry, one request.
export const PROJECTS_QUERY_KEY = ["studio", "projects"];

function Group({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="slds-m-top_large">
      <h3 className="slds-text-title_caps slds-border_bottom slds-p-bottom_x-small" style={{ fontSize: "13px", fontWeight: 600, textTransform: "none" }}>
        {title}
      </h3>
      <div className="slds-m-top_small">{children}</div>
    </section>
  );
}

function Failure({ error }: { error: unknown }) {
  return error ? <ScopedNotification theme="error">{errorMessage(error)}</ScopedNotification> : null;
}

function CustomerSettings({ row }: { row: ProjectRow }) {
  const qc = useQueryClient();
  const router = useRouter();
  const [industry, setIndustry] = useState(row.industry ?? "");
  const [health, setHealth] = useState<ProjectHealth>(row.health);
  const [retiring, setRetiring] = useState(false);
  const refresh = (r: ProjectRow) => qc.setQueryData<ProjectRow[]>(PROJECTS_QUERY_KEY, (rows) => rows?.map((x) => (x.tenantId === r.tenantId ? r : x)));
  const patch = useMutation({ mutationFn: (p: { industry?: string | null; trial?: boolean }) => patchProject(row.tenantId, p), onSuccess: refresh });
  const check = useMutation({ mutationFn: () => healthCheckProject(row.tenantId), onSuccess: setHealth });
  const retire = useMutation({
    mutationFn: () => retireTenant(row.tenantId),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: PROJECTS_QUERY_KEY });
      router.replace("/studio");
    },
  });
  const rows: [string, string][] = [
    [t("studio.settings.name"), row.name],
    [t("studio.settings.code"), row.slug],
  ];
  return (
    <div className="slds-p-around_large" style={{ maxWidth: "48rem" }}>
      <h2 className="slds-text-heading_medium">{t("studio.route.settings")}</h2>
      <dl className="slds-m-top_small grid grid-cols-[8rem_minmax(0,1fr)] gap-y-2">
        {rows.map(([label, value]) => (
          <div key={label} className="contents">
            <dt className="slds-text-color_weak">{label}</dt>
            <dd>{value}</dd>
          </div>
        ))}
      </dl>
      <p className="slds-text-color_weak slds-text-body_small slds-m-top_x-small">{t("studio.settings.fixedNote")}</p>

      <Group title={t("studio.settings.groupGeneral")}>
        <div className="flex items-end gap-2">
          <div className="flex-1">
            <Input label={t("studio.settings.industry")} value={industry} onChange={(e) => setIndustry(e.target.value)} maxLength={120} />
          </div>
          <Button variant="neutral" disabled={patch.isPending || industry.trim() === (row.industry ?? "")} onClick={() => patch.mutate({ industry: industry.trim() || null })}>
            {t("studio.settings.saveIndustry")}
          </Button>
        </div>
        <div className="slds-m-top_medium">
          <Toggle
            label={t("studio.settings.trial")}
            help={t("studio.settings.trialHelp")}
            checked={row.trialSince != null}
            disabled={patch.isPending || row.deliveredAt != null}
            onChange={(e) => patch.mutate({ trial: e.target.checked })}
          />
        </div>
        <div className="slds-m-top_small"><Failure error={patch.error} /></div>
      </Group>

      <Group title={t("studio.settings.groupHealth")}>
        <Button variant="neutral" disabled={check.isPending} onClick={() => check.mutate()}>
          {t("studio.settings.recheck")}
        </Button>
        {health.checks.length === 0 ? (
          <p className="slds-text-color_weak slds-m-top_small">{t("studio.settings.noChecks")}</p>
        ) : (
          <ul className="slds-m-top_small">
            {health.checks.map((c) => (
              <li key={c.checkId}>{`${c.checkId} · ${c.severity} · ${c.count}`}</li>
            ))}
          </ul>
        )}
        <div className="slds-m-top_small"><Failure error={check.error} /></div>
      </Group>

      <Group title={t("studio.settings.groupBackup")}>
        <a href={studioHref(row.slug, "bao-tri/sao-luu")}>{t("studio.settings.backupLink")}</a>
      </Group>

      <OwnerTierSection tenantId={row.tenantId} name={row.name} />

      <Group title={t("studio.settings.groupDanger")}>
        {retiring ? (
          <InlineConfirm
            tone="error"
            message={tf("studio.settings.retireMessage", { name: row.name })}
            confirmLabel={t("studio.settings.retireConfirm")}
            busy={retire.isPending}
            onConfirm={() => retire.mutate()}
            onCancel={() => setRetiring(false)}
          />
        ) : (
          <Button variant="text-destructive" onClick={() => setRetiring(true)}>{t("studio.settings.retire")}</Button>
        )}
        <p className="slds-text-color_weak slds-m-top_x-small">{t("studio.settings.purgeWhere")}</p>
        <div className="slds-m-top_small"><Failure error={retire.error} /></div>
      </Group>
    </div>
  );
}

function CustomerGate({ slug }: { slug: string }) {
  const q = useQuery({ queryKey: PROJECTS_QUERY_KEY, queryFn: listProjects, staleTime: 30_000 });
  const row = q.data?.find((r) => r.slug === slug);
  return (
    <QueryState query={q} empty={null}>
      {row ? (
        <CustomerSettings key={row.tenantId} row={row} />
      ) : (
        <div className="slds-p-around_large">
          <EmptyState title={t("studio.settings.notFound")} description={t("studio.settings.notFoundHint")} />
        </div>
      )}
    </QueryState>
  );
}

export function ProjectSettings({ slug }: { slug: string }) {
  if (slug !== homeSlug()) return <CustomerGate key={slug} slug={slug} />;
  const rows: [string, string][] = [
    [t("studio.settings.name"), t("studio.project.home")],
    [t("studio.settings.code"), slug],
    [t("studio.settings.audience"), t("studio.settings.audienceNo")],
  ];
  return (
    <div className="slds-p-around_large">
      <h2 className="slds-text-heading_medium">{t("studio.route.settings")}</h2>
      <dl className="slds-m-top_small grid grid-cols-[8rem_minmax(0,1fr)] gap-y-2">
        {rows.map(([label, value]) => (
          <div key={label} className="contents">
            <dt className="slds-text-color_weak">{label}</dt>
            <dd>{value}</dd>
          </div>
        ))}
      </dl>
      <p className="slds-text-color_weak slds-m-top_medium">{t("studio.settings.readonly")}</p>
    </div>
  );
}
