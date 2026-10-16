"use client";

// Stage line of the Chạy thử step (specs/077 US8): shows the project's stage and, while it is not delivered, one text
// button to start / leave the trial. Reads the same ledger as Cài đặt dự án (PATCH /admin/projects/:tenantId {trial}).
// The workshop project has no ledger row and a delivered project cannot go back (422), so neither draws a button.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Button } from "@/components/slds/Button";
import { ApiError, listProjects, patchProject, type ProjectRow } from "@/lib/api";
import { errorMessage } from "@/lib/errorMessages";
import { t } from "@/lib/i18n";
import { PROJECTS_QUERY_KEY } from "../ProjectSettings";

const STAGE_NAME = { building: "building", trial: "trial", live: "live", needs_attention: "needsAttention" } as const;

export function TrialStageSwitch({ slug }: { slug: string }) {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: PROJECTS_QUERY_KEY, queryFn: listProjects, staleTime: 30_000 });
  const row = q.data?.find((r) => r.slug === slug);
  const patch = useMutation({
    mutationFn: (trial: boolean) => patchProject(row!.tenantId, { trial }),
    onSuccess: (r: ProjectRow) => {
      qc.setQueryData<ProjectRow[]>(PROJECTS_QUERY_KEY, (rows) => rows?.map((x) => (x.tenantId === r.tenantId ? r : x)));
      void qc.invalidateQueries({ queryKey: ["project", "current"] });
    },
    // Delivered meanwhile (another tab): reload the ledger so the button disappears.
    onError: (e) => {
      if (e instanceof ApiError && e.code === "PROJECT_TRIAL_AFTER_DELIVERY") void qc.invalidateQueries({ queryKey: PROJECTS_QUERY_KEY });
    },
  });
  if (!row) return null;
  const canSwitch = row.deliveredAt == null && (row.stage === "building" || row.stage === "trial");
  return (
    <div className="flex flex-wrap items-center gap-2 slds-m-bottom_small">
      <span className="slds-text-color_weak">{t("studio.build.trial.stage.label")}</span>
      <strong>{t(`studio.stage.${STAGE_NAME[row.stage]}.name`)}</strong>
      {canSwitch && (
        <Button variant="neutral" disabled={patch.isPending} onClick={() => patch.mutate(row.stage === "building")}>
          {t(row.stage === "building" ? "studio.build.trial.stage.start" : "studio.build.trial.stage.back")}
        </Button>
      )}
      {patch.error && <span className="slds-text-color_error" role="alert">{errorMessage(patch.error)}</span>}
    </div>
  );
}
