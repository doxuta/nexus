"use client";

// QueuePage (specs/074 lát f2-automation-approval-queue): `dung/tu-dong/hang-doi[/<id>]` — the work queues of the
// project, filtered by `?app=` / `?bang=`; the open queue follows the URL (`hang-doi/<id>`).
import { studioHref } from "@/lib/studioRoutes";
import { useRouter } from "next/navigation";
import { WorkQueueBuilder } from "@/components/studio/WorkQueueBuilder";
import { AutomationScope } from "./AutomationScope";
import { t } from "@/lib/i18n";
import { useDirectApplyGate } from "@/components/studio/useDirectApplyGate";

export function QueuePage({ slug, id, app, bang }: { slug: string; id?: string; app?: string; bang?: string }) {
  const router = useRouter();
  const applyGate = useDirectApplyGate(slug, "work_queue");
  const follow = (open: string | null) => {
    if ((open ?? undefined) === id) return;
    const query = { app, bang };
    router.replace(open ? studioHref(slug, "dung/tu-dong/hang-doi/:id", { id: open }, query) : studioHref(slug, "dung/tu-dong/hang-doi", {}, query), { scroll: false });
  };
  return (
    <AutomationScope slug={slug} route="dung/tu-dong/hang-doi" app={app} bang={bang} noun={t("studio.build.automation.queue.noun")}>
      {(s) => (
        <WorkQueueBuilder
          entityNames={s.entityNames}
          entity={bang}
          showAllHref={s.showAllHref}
          initialQueueId={id ?? null}
          onSelect={follow}
          applyGate={applyGate}
        />
      )}
    </AutomationScope>
  );
}
