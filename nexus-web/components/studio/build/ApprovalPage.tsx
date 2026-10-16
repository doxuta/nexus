"use client";

// ApprovalPage (specs/074 lát f2-automation-approval-queue): `dung/tu-dong/duyet[/<id>]` — the list of approval
// processes on the left and the editor on the right, in one surface. The scope is `?app=` / `?bang=`; the open process
// follows the URL (`duyet/<id>`), and an id the scope hides or that does not exist opens nothing of another app's.
import { studioHref } from "@/lib/studioRoutes";
import { useRouter } from "next/navigation";
import { ApprovalBuilder } from "@/components/studio/ApprovalBuilder";
import { AutomationScope } from "./AutomationScope";
import { t } from "@/lib/i18n";
import { useDirectApplyGate } from "@/components/studio/useDirectApplyGate";

export function ApprovalPage({ slug, id, app, bang }: { slug: string; id?: string; app?: string; bang?: string }) {
  const router = useRouter();
  const applyGate = useDirectApplyGate(slug, "approval");
  // The URL follows the open process; replace, so Back does not step through every click.
  const follow = (open: string | null) => {
    if ((open ?? undefined) === id) return;
    const query = { app, bang };
    router.replace(open ? studioHref(slug, "dung/tu-dong/duyet/:id", { id: open }, query) : studioHref(slug, "dung/tu-dong/duyet", {}, query), { scroll: false });
  };
  return (
    <AutomationScope slug={slug} route="dung/tu-dong/duyet" app={app} bang={bang} noun={t("studio.build.automation.approval.noun")}>
      {(s) => (
        <ApprovalBuilder
          entityNames={s.entityNames}
          entity={bang}
          appLabel={s.appLabel}
          showAllHref={s.showAllHref}
          initialProcessId={id ?? null}
          onSelect={follow}
          applyGate={applyGate}
        />
      )}
    </AutomationScope>
  );
}
