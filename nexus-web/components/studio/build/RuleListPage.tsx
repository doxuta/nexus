"use client";

// RuleListPage (specs/074 lát f2-rule-list): `dung/tu-dong` — the rules of the project read as three plain lines each
// (Khi / Nếu / Thì, from the stored config via ruleSentence), filtered by `?app=` / `?bang=`, with the four sibling
// pages counted in the right column. A row's name opens its own page (`luat/<id>`); '+ Luật mới' opens a small form
// in place (no dialog) that hands the choice to `luat/moi`, where the rule is saved (switched off).
import Link from "next/link";
import { useMemo, useState } from "react";
import { useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import { appsCatalog, ensureAuth, getWorkflow, listApprovalProcesses, listEntities, listFields, listInboundHooks, listRecentWorkflowRuns, listWorkflows, listWorkQueuesAdmin, type WorkflowDetail, type WorkflowSummary } from "@/lib/api";
import { listBusinessCalendars } from "@/lib/admin";
import { scopeEntityOptions, splitByScope, useAppEntityNames } from "@/lib/appScope";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { useGuardedRouter } from "@/lib/navigation-blocker";
import { qk } from "@/lib/query/keys";
import { chunkIds, failedWorkflowIds } from "@/lib/ruleLastRun";
import { ruleSentence } from "@/lib/ruleSentence";
import { studioHref } from "@/lib/studioRoutes";
import { toggleRuleActive, switchOffNeedsDisableNow } from "@/lib/workflowToggle";
import { AppFilter } from "@/components/studio/AppFilter";
import { useProposedChange } from "@/components/studio/assistant/ProposalOverlay";
import { GhostLive, ProposedGhost } from "@/components/studio/assistant/ProposedGhost";
import { RuleSummary } from "@/components/studio/RuleSummary";
import { TRIGGERS } from "@/components/studio/WorkflowBuilder";
import { Button } from "@/components/slds/Button";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { EmptyState } from "@/components/slds/EmptyState";
import { Input } from "@/components/slds/Input";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";
import { Spinner } from "@/components/slds/Spinner";

type Msg = { tone: "success" | "error"; text: string };

export function RuleListPage({ slug, app, bang }: { slug: string; app?: string; bang?: string }) {
  const router = useGuardedRouter();
  const qc = useQueryClient();
  const appCode = app ?? null;
  const proposed = useProposedChange({ docType: "workflow" });
  const ghostRow = proposed?.op === "create" ? proposed : null;
  const scope = useAppEntityNames(appCode);
  const appsQ = useQuery({ queryKey: qk.apps(), queryFn: appsCatalog, staleTime: 60_000 });
  const rulesQ = useQuery({ queryKey: qk.adminWorkflows(), queryFn: async () => { await ensureAuth(); return listWorkflows(); } });
  const entitiesQ = useQuery({ queryKey: qk.entities(), queryFn: async () => { await ensureAuth(); return listEntities(); }, staleTime: 60_000 });
  const [creating, setCreating] = useState(false);
  const [form, setForm] = useState({ ten: "", bang: bang ?? "", khi: "after_create" });
  const [busy, setBusy] = useState<Set<string>>(new Set());
  const [msgs, setMsgs] = useState<Record<string, Msg>>({});

  const entities = useMemo(() => entitiesQ.data ?? [], [entitiesQ.data]);
  const all = useMemo(() => rulesQ.data ?? [], [rulesQ.data]);
  const { visible: inApp, hidden } = splitByScope(all, scope.entityNames, (w) => w.entityName);
  const shown = bang ? inApp.filter((w) => w.entityName === bang) : inApp;
  const tableNames = useMemo(() => Array.from(new Set(shown.map((w) => w.entityName).filter((n): n is string => !!n))), [shown]);

  const details = useQueries({ queries: shown.map((w) => ({ queryKey: qk.adminWorkflow(w.id), queryFn: async () => { await ensureAuth(); return getWorkflow(w.id); }, staleTime: 30_000 })) });
  const fieldQs = useQueries({ queries: tableNames.map((n) => ({ queryKey: qk.fields(n), queryFn: async () => { await ensureAuth(); return listFields(n); }, staleTime: 60_000 })) });
  const fields = fieldQs.flatMap((q) => q.data ?? []);
  const detailOf = new Map<string, WorkflowDetail>();
  details.forEach((q) => { if (q.data) detailOf.set(q.data.id, q.data); });

  const idKey = shown.map((w) => w.id).join(",");
  const runsQ = useQuery({
    queryKey: qk.adminWorkflowRuns(idKey),
    enabled: shown.length > 0,
    queryFn: async () => {
      await ensureAuth();
      const failed = new Map<string, string>();
      for (const grp of chunkIds(shown.map((w) => w.id))) for (const [k, v] of failedWorkflowIds(await listRecentWorkflowRuns(100, grp))) failed.set(k, v);
      return failed;
    },
  });

  const approvals = useQuery({ queryKey: qk.approvalProcesses(), queryFn: async () => { await ensureAuth(); return listApprovalProcesses(); } });
  const queues = useQuery({ queryKey: qk.workQueuesAdmin(), queryFn: async () => { await ensureAuth(); return listWorkQueuesAdmin(); } });
  const hooks = useQuery({ queryKey: qk.inboundHooks(), queryFn: async () => { await ensureAuth(); return listInboundHooks(); } });
  const calendars = useQuery({ queryKey: qk.businessCalendars(), queryFn: async () => { await ensureAuth(); return listBusinessCalendars(); } });
  const related: Array<[string, string]> = [
    ["dung/tu-dong/duyet", approvals.data ? tf("studio.build.automation.approval.relatedCount", { n: approvals.data.length }) : t("studio.route.approvals")],
    ["dung/tu-dong/hang-doi", queues.data ? tf("studio.build.automation.queue.relatedCount", { n: queues.data.length }) : t("studio.route.queues")],
    ["dung/tu-dong/cong-nhan", hooks.data ? tf("studio.build.automation.hook.count", { n: hooks.data.length }) : t("studio.route.hooks")],
    ["dung/tu-dong/lich-lam-viec", calendars.data ? tf("studio.build.automation.calendar.count", { n: calendars.data.length }) : t("studio.route.calendars")],
  ];

  const ruleHref = (id: string) => studioHref(slug, "dung/tu-dong/luat/:id", { id }, { app });
  const setMsg = (id: string, m: Msg | null) => setMsgs((cur) => { const n = { ...cur }; if (m) n[id] = m; else delete n[id]; return n; });

  async function toggle(w: WorkflowSummary, next: boolean) {
    // A rule the customer is running stops NOW through the rule page (063 disable-now), never a silent draft switch.
    if (!next && switchOffNeedsDisableNow(w)) { setMsg(w.id, { tone: "error", text: t("studio.build.automation.list.openToSwitchOff") }); return; }
    setBusy((b) => new Set(b).add(w.id));
    setMsg(w.id, null);
    const r = await toggleRuleActive(w.id, next);
    setBusy((b) => { const n = new Set(b); n.delete(w.id); return n; });
    if (r.ok) {
      setMsg(w.id, { tone: "success", text: tf(r.isActive ? "studio.build.automation.toggle.turnedOn" : "studio.build.automation.toggle.turnedOff", { name: w.name }) });
      await qc.invalidateQueries({ queryKey: qk.adminWorkflows() });
    } else setMsg(w.id, { tone: "error", text: r.message });
  }

  const tablesInScope = scopeEntityOptions(entities, scope.entityNames, bang);
  const openForm = () => { setForm((f) => ({ ...f, bang: f.bang || bang || tablesInScope[0]?.name || "" })); setCreating(true); };
  const allHref = studioHref(slug, "dung/tu-dong", {}, { bang });
  const loading = rulesQ.isPending || scope.isLoading;

  let body;
  if (scope.unknown) {
    body = <EmptyState plain className="slds-p-around_medium" title={tf("studio.build.data.unknownApp", { app: appCode ?? "" })} description={t("studio.build.data.unknownAppDesc")} action={<Link href={allHref}>{t("studio.build.data.showAll")}</Link>} />;
  } else if (loading) {
    body = <div className="slds-is-relative slds-p-around_xx-large"><Spinner size="medium" label={t("studio.build.automation.list.loading")} /></div>;
  } else if (rulesQ.isError || scope.error) {
    body = <div className="slds-p-around_medium"><ScopedNotification theme="error">{t("studio.build.automation.list.error")}</ScopedNotification></div>;
  } else if (shown.length === 0 && !ghostRow) {
    body = (
      <EmptyState
        plain
        className="slds-p-around_medium"
        title={scope.app ? tf("studio.build.automation.noRulesApp", { app: scope.app.label }) : t("studio.build.automation.list.empty")}
        description={t("studio.build.automation.list.emptyDesc")}
        action={<Button variant="brand" onClick={openForm}>{t("studio.build.automation.list.first")}</Button>}
      />
    );
  } else {
    body = (
      <ul className="slds-has-dividers_bottom slds-p-horizontal_medium">
        {ghostRow && (
          <li className="slds-item nx-proposed">
            <ProposedGhost change={ghostRow} />
          </li>
        )}
        {shown.map((w) => {
          const d = detailOf.get(w.id);
          const sentence = ruleSentence((d ?? w) as WorkflowDetail, entities, fields);
          const failedRun = runsQ.data?.get(w.id);
          return (
            <RuleSummary
              key={w.id}
              name={w.name}
              sentence={sentence}
              trigger={w.trigger}
              active={w.isActive}
              draftState={w.draftState}
              href={ruleHref(w.id)}
              onToggle={(next) => void toggle(w, next)}
              toggling={busy.has(w.id)}
              failed={!!failedRun}
              // ponytail: opens the rule page; the page does not take a run id yet (rule page shows its recent runs)
              onOpenFailedRun={() => router.push(ruleHref(w.id))}
              message={msgs[w.id]}
            />
          );
        })}
      </ul>
    );
  }

  return (
    <EmbeddedSurface>
      <GhostLive change={ghostRow} />
      <div className="flex flex-col items-stretch gap-4 lg:flex-row lg:items-start">
        <div className="min-w-0 flex-1">
          <div className="slds-grid slds-grid_vertical-align-center slds-wrap slds-p-horizontal_medium slds-p-vertical_small">
            <div className="slds-col">
              <AppFilter slug={slug} route="dung/tu-dong" apps={appsQ.data ?? []} value={appCode} hiddenCount={hidden} hiddenNoun={t("studio.build.automation.hiddenNoun")} query={{ bang }} />
            </div>
            <Button variant="neutral" onClick={openForm} disabled={creating}>{t("studio.build.automation.list.new")}</Button>
          </div>
          {creating && (
            <form
              className="slds-p-horizontal_medium slds-p-bottom_medium"
              onSubmit={(e) => {
                e.preventDefault();
                router.push(studioHref(slug, "dung/tu-dong/luat/moi", {}, { app, bang: form.bang || undefined, khi: form.khi, ten: form.ten.trim() || undefined }));
              }}
            >
              <Input label={t("studio.build.automation.list.form.name")} value={form.ten} onChange={(e) => setForm({ ...form, ten: e.target.value })} />
              <Select
                label={t("studio.build.automation.list.form.entity")}
                placeholder={false}
                value={form.bang}
                onChange={(e) => setForm({ ...form, bang: e.target.value })}
                options={tablesInScope.map((e) => ({ value: e.name, label: e.label || e.name }))}
              />
              <Select
                label={t("studio.build.automation.list.form.trigger")}
                placeholder={false}
                value={form.khi}
                onChange={(e) => setForm({ ...form, khi: e.target.value })}
                options={TRIGGERS.map((k) => ({ value: k, label: t(`page.workflow.trigger.${k}` as I18nKey) }))}
              />
              <div className="slds-m-top_small">
                <Button type="submit" variant="brand" disabled={!form.bang}>{t("studio.build.automation.list.form.go")}</Button>
                <Button variant="neutral" className="slds-m-left_x-small" onClick={() => setCreating(false)}>{t("studio.build.automation.list.form.cancel")}</Button>
              </div>
            </form>
          )}
          {body}
        </div>
        <aside className="w-full shrink-0 px-4 lg:w-[300px] lg:pl-0" aria-label={t("studio.build.automation.related")}>
          <h2 className="slds-text-heading_small slds-m-bottom_x-small">{t("studio.build.automation.related")}</h2>
          <ul className="slds-has-dividers_bottom">
            {related.map(([id, label]) => (
              <li key={id} className="slds-item"><Link href={studioHref(slug, id as Parameters<typeof studioHref>[1])}>{label}</Link></li>
            ))}
          </ul>
        </aside>
      </div>
    </EmbeddedSurface>
  );
}
