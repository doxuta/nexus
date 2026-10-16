"use client";

// Giao cho khách (specs/077 f5-handoff-checklist): the checklist built from what the engine already tells us. There
// ends in the one-way "Giao cho khách" action (f5-handoff-deliver); once delivered the page is the result block instead.
import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import type { SampleWipeKept } from "@/lib/api";
import { emitDraftsChanged } from "@/lib/draftEvents";
import { EmptyState } from "@/components/slds/EmptyState";
import { Spinner } from "@/components/slds/Spinner";
import { buildHandoffRows, usableOwners } from "@/lib/handoffChecklist";
import { t } from "@/lib/i18n";
import { homeSlug } from "@/lib/studioContext";
import { invalidateSampleData } from "../trial/useSampleSummary";
import { Button } from "@/components/slds/Button";
import { usePublishState } from "../usePublishState";
import { DeliverConfirm } from "./DeliverConfirm";
import { DeliveredNotice } from "./DeliveredNotice";
import { HandoffChecklist } from "./HandoffChecklist";
import { useEntityLabels, useHandoffApps, useHandoffOwner, useSampleSummary } from "./useHandoffApps";

export function HandoffPage({ slug }: { slug: string }) {
  const apps = useHandoffApps(slug);
  const labels = useEntityLabels(slug);
  const samples = useSampleSummary(slug);
  const owner = useHandoffOwner(slug);
  const publish = usePublishState(slug);
  const qc = useQueryClient();
  // Rows the last wipe kept live in page memory only: the engine does not say which sample rows were kept.
  const [kept, setKept] = useState<SampleWipeKept[]>([]);
  const [keptAck, setKeptAck] = useState(false);
  // Delivered (or found already delivered): every reader of the project state re-reads, the legacy DraftBar included.
  const delivered = () => {
    void qc.invalidateQueries({ queryKey: ["project", slug] });
    void qc.invalidateQueries({ queryKey: ["projects"] });
    void qc.invalidateQueries({ queryKey: ["studio"] });
    emitDraftsChanged();
  };

  if (slug === homeSlug())
    return (
      <div className="slds-p-horizontal_medium">
        <EmptyState title={t("studio.build.handover.hidden")} description={t("studio.build.handover.hiddenWhy")} />
      </div>
    );

  const spinner = (
    <div className="slds-is-relative slds-p-around_large">
      <Spinner size="medium" />
      <span className="slds-assistive-text">{t("studio.build.handover.checking")}</span>
    </div>
  );
  // Unknown whether it is delivered yet: neither the checklist nor the result may flash first, and a failed read is never "not delivered".
  if (publish.isPending) return spinner;
  if (publish.isError || !publish.data)
    return (
      <div className="slds-p-around_medium">
        <p>{t("studio.build.handover.err.state")}</p>
        <Button variant="neutral" className="slds-m-top_small" onClick={() => void publish.refetch()}>
          {t("studio.build.handover.action.retry")}
        </Button>
      </div>
    );
  if (publish.data.delivered)
    return (
      <div className="slds-p-around_medium">
        <DeliveredNotice slug={slug} state={publish.data} owner={owner.isPending ? undefined : owner.isError || !owner.data ? "error" : owner.data} onRetryOwner={() => void owner.refetch()} />
      </div>
    );

  const rows =
    apps.isPending || samples.isPending || owner.isPending
      ? null
      : buildHandoffRows({
          // ponytail: a project session has no current-project row yet, so the build steps are "not known" (never blocking).
          steps: null,
          apps: apps.isError ? "error" : (apps.data ?? []),
          samples: samples.isError || !samples.data ? "error" : { total: samples.data.total, kept: kept.length },
          keptSampleAck: keptAck,
          entityLabels: labels.data ?? {},
          owner: owner.isError || !owner.data ? "error" : owner.data,
        });
  const retry = () => void Promise.all([apps.refetch(), labels.refetch(), samples.refetch(), owner.refetch()]);
  const focusRow = (id: string) => {
    const li = document.querySelector<HTMLElement>(`[data-row="${CSS.escape(id)}"]`);
    (li?.querySelector<HTMLElement>("a,button") ?? li)?.focus();
  };
  return (
    <div className="slds-p-around_medium">
      {rows ? (
        <>
          <HandoffChecklist slug={slug} rows={rows} roles={owner.data?.roles} onRetry={retry} onOwnerChanged={() => void owner.refetch()}
            samples={
              samples.data
                ? {
                    summary: samples.data,
                    entityNames: Object.keys(labels.data ?? {}),
                    label: (e) => labels.data?.[e] ?? e,
                    kept,
                    keptAck,
                    onKeptAck: setKeptAck,
                    onWiped: (res) => {
                      setKept(res.kept);
                      setKeptAck(false);
                      void invalidateSampleData(qc, slug);
                    },
                  }
                : undefined
            }
          />
          <DeliverConfirm
            blocked={rows.filter((r) => r.blocking).map((r) => r.id)}
            keptSamples={samples.data?.total ?? 0}
            ownerEmail={owner.data ? usableOwners(owner.data.users)[0]?.email : undefined}
            onFocusBlocked={focusRow}
            onDelivered={delivered}
          />
        </>
      ) : (
        spinner
      )}
    </div>
  );
}
