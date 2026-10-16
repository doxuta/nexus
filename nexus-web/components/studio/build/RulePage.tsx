"use client";

// RulePage (specs/074 lát f2-rule-page): `dung/tu-dong/luat/<id | moi>` — ONE rule on its own page. The id comes from
// the path, the app filter from `?app=`; `luat/moi?bang=&khi=&ten=` opens a blank rule. The rule builder gets the scope as
// props (never the shared "active app" lens), so a link to a rule of another app shows the empty sentence, not that rule.
// Layout is still the old editor (hideList); lát 11 flattens it.
import Link from "next/link";
import { useGuardedRouter } from "@/lib/navigation-blocker";
import { useAppEntityNames } from "@/lib/appScope";
import { t } from "@/lib/i18n";
import { studioHref } from "@/lib/studioRoutes";
import { WorkflowBuilder } from "@/components/studio/WorkflowBuilder";
import { useProposedChange } from "@/components/studio/assistant/ProposalOverlay";
import { GhostLive, ProposedGhost } from "@/components/studio/assistant/ProposedGhost";
import { changeSideLines } from "@/lib/aiBuilder";

export function RulePage({ slug, id, app, bang, khi, ten }: { slug: string; id?: string; app?: string; bang?: string; khi?: string; ten?: string }) {
  const router = useGuardedRouter();
  const scope = useAppEntityNames(app ?? null);
  const back = studioHref(slug, "dung/tu-dong", {}, { app });
  const proposed = useProposedChange({ docType: "workflow", docKey: id });
  const ghost = id && id !== "moi" && proposed && proposed.op !== "create" ? proposed : null;
  const before = ghost ? changeSideLines("workflow", ghost.before as Record<string, unknown> | null | undefined) : [];
  const after = ghost && ghost.op === "update" ? changeSideLines("workflow", ghost.after as Record<string, unknown> | null | undefined, ghost.before as Record<string, unknown> | null | undefined) : [];
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <GhostLive change={ghost} />
      <p className="slds-m-vertical_small slds-p-left_medium">
        <Link href={back}>{`← ${t("studio.build.automation.back")}`}</Link>
      </p>
      {ghost && (
        <div className="slds-m-horizontal_medium slds-m-bottom_small">
          <p><ProposedGhost change={ghost} /></p>
          <div className="nx-proposed slds-p-around_small grid gap-4 md:grid-cols-2">
            <section aria-label={t("studio.build.ai.beforeHead")}>
              <h3 className="slds-text-title_caps">{t("studio.build.ai.beforeHead")}</h3>
              <ul>{before.map((l, i) => <li key={i}>{l}</li>)}</ul>
            </section>
            {ghost.op === "update" && (
              <section aria-label={t("studio.build.ai.afterHead")}>
                <h3 className="slds-text-title_caps">{t("studio.build.ai.afterHead")}</h3>
                <ul>{after.map((l, i) => <li key={i}>{l}</li>)}</ul>
              </section>
            )}
          </div>
        </div>
      )}
      <WorkflowBuilder
        hideList
        app={scope.app}
        entityNames={scope.entityNames}
        scopeLoading={scope.isLoading}
        initialRuleId={id === "moi" ? undefined : id}
        newDraft={id === "moi" || !id ? { entity: bang, trigger: khi, name: ten } : undefined}
        onSelectRule={(rid) => router.replace(studioHref(slug, "dung/tu-dong/luat/:id", { id: rid }, { app }), { scroll: false })}
        onDeleted={() => router.replace(back)}
      />
    </div>
  );
}
