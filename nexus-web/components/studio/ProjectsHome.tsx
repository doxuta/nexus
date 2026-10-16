"use client";

// "Dự án khách" — the Studio front page (specs/073 FR-050/FR-091, US9; demo A shot 1440-01-home).
// Design Read: the navy "Cần anh xem" band (the one bold thing) over ONE raised stage board (four columns, rules not cards).
// All of it comes from ONE request, GET /admin/projects (no per-customer N+1). The only extra calls are the optional
// "Khám lại" of snapshots older than 24h, two at a time. "Dự án mới" stays out until tao1 (no engine path in this view).
// Context: ctx "nha" (home token). Opening a project opens its build session (one click, FR-020); pages that live in the
// home context (settings, backups) are plain links.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { useCallback, useEffect, useRef, useState } from "react";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Input } from "@/components/slds/Input";
import { Spinner } from "@/components/slds/Spinner";
import { healthCheckProject, listProjects, patchProject, type ProjectRow } from "@/lib/api";
import { errorMessage } from "@/lib/errorMessages";
import { STEP_ROUTE, type BuildStepKey } from "@/lib/buildSteps";
import { t, tf } from "@/lib/i18n";
import { useConfirmLeave } from "@/lib/navigation-blocker";
import { isStaleCheck, type CheckState } from "@/lib/projectCheck";
import { attentionItems, type AttentionItem } from "@/lib/projectSignals";
import { qk } from "@/lib/query/keys";
import { enterProject, homeSlug } from "@/lib/studioContext";
import { ROUTES, studioHref, type RouteId } from "@/lib/studioRoutes";
import { AttentionBand } from "./AttentionBand";
import { ContextGate } from "./ContextGate";
import { StageBoard } from "./StageBoard";

const PARALLEL_CHECKS = 2;

export function ProjectsHome() {
  const home = homeSlug();
  const router = useRouter();
  const qc = useQueryClient();
  const confirmLeave = useConfirmLeave();
  const [q, setQ] = useState(useSearchParams().get("q") ?? "");
  const [checks, setChecks] = useState<Record<string, CheckState>>({});
  const [problem, setProblem] = useState<string | null>(null);
  const projects = useQuery({ queryKey: qk.projects(), queryFn: listProjects });
  const rows = (projects.data ?? []).filter((r) => r.slug !== home);

  const check = useCallback(
    async (row: ProjectRow) => {
      setChecks((c) => ({ ...c, [row.tenantId]: { kind: "running" } }));
      try {
        const h = await healthCheckProject(row.tenantId);
        setChecks((c) => ({ ...c, [row.tenantId]: { kind: "done", status: h.status, at: h.checkedAt ?? new Date().toISOString() } }));
      } catch (e) {
        setChecks((c) => ({ ...c, [row.tenantId]: { kind: "failed", why: errorMessage(e) } }));
      }
    },
    [],
  );

  // Snapshots older than 24h are re-run once per project per visit, two at a time; the stored result then refreshes the band.
  const started = useRef(new Set<string>());
  useEffect(() => {
    const todo = rows.filter((r) => isStaleCheck(r.health.checkedAt) && !started.current.has(r.tenantId));
    if (todo.length === 0) return;
    todo.forEach((r) => started.current.add(r.tenantId));
    const queue = [...todo];
    const worker = async () => {
      for (let r = queue.shift(); r; r = queue.shift()) await check(r);
    };
    void Promise.all(Array.from({ length: Math.min(PARALLEL_CHECKS, queue.length) }, worker)).then(() =>
      qc.invalidateQueries({ queryKey: qk.projects() }),
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps -- `rows` is derived from projects.data; keyed on it
  }, [projects.data]);

  const patch = useMutation({
    mutationFn: (v: { tenantId: string; patch: { industry?: string | null; trial?: boolean } }) => patchProject(v.tenantId, v.patch),
    onError: (e) => setProblem(tf("studio.projects.actionFailed", { why: errorMessage(e) })),
    onSuccess: () => setProblem(null),
  });

  const go = async (slug: string, route: RouteId) => {
    const href = studioHref(slug, route);
    // `nha` pages (settings, backups) live in the home context: no session to open.
    if (ROUTES.find((r) => r.id === route)?.ctx === "nha") return router.push(href);
    try {
      await enterProject(slug, href, { qc, confirmLeave });
    } catch (e) {
      setProblem(tf("studio.projects.actionFailed", { why: errorMessage(e) }));
    }
  };

  const search = (v: string) => {
    setQ(v);
    router.replace(v.trim() ? `/studio?q=${encodeURIComponent(v.trim())}` : "/studio");
  };

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
      <div className="mx-auto flex w-full max-w-[1560px] flex-col gap-4 p-6 max-md:p-4">
        <header className="slds-grid slds-grid_align-spread slds-grid_vertical-align-end slds-wrap">
          <div>
            <h1 className="slds-text-heading_large">{t("studio.projects.title")}</h1>
            <p className="nx-lede slds-m-top_xx-small">{t("studio.projects.ledeHome")}</p>
          </div>
          <div className="slds-grid slds-grid_vertical-align-end slds-wrap">
            <div className="w-72 max-md:w-full">
              <Input type="search" label={t("studio.projects.search")} labelHidden placeholder={t("studio.projects.search")} value={q} onChange={(e) => search(e.target.value)} />
            </div>
            <Link href="/studio/moi" className="slds-button slds-button_brand slds-m-left_small">{t("studio.newProject.title")}</Link>
          </div>
        </header>
        <ContextGate slug={home} ctx="nha">
          {problem && <ScopedNotification theme="error">{problem}</ScopedNotification>}
          {projects.isPending ? (
            <div className="slds-is-relative slds-p-around_xx-large">
              <Spinner size="medium" />
            </div>
          ) : projects.isError ? (
            <ScopedNotification theme="error">
              {errorMessage(projects.error)}{" "}
              <button type="button" className="slds-button" onClick={() => void projects.refetch()}>
                {t("studio.query.retry")}
              </button>
            </ScopedNotification>
          ) : (
            <>
              <AttentionBand
                items={attentionItems(rows)}
                onOpenProject={(slug) => void go(slug, "dung/du-lieu")}
                onAction={(it: AttentionItem) => void go(it.projectSlug, it.action.route)}
              />
              <StageBoard
                rows={rows}
                query={q}
                checks={checks}
                onOpen={(r) => void go(r.slug, "dung/du-lieu")}
                onOpenStep={(r, step: BuildStepKey) => void go(r.slug, STEP_ROUTE[step])}
                onTrial={(r, on) => patch.mutate({ tenantId: r.tenantId, patch: { trial: on } })}
                onIndustry={(r, industry) => patch.mutate({ tenantId: r.tenantId, patch: { industry } })}
                onHealthCheck={(r) => void check(r)}
                onSettings={(r) => void go(r.slug, "cai-dat")}
              />
            </>
          )}
          <p className="slds-text-body_small slds-text-color_weak">
            <Link href={studioHref(home, "dung/du-lieu")}>{t("studio.project.home")}</Link> · {t("studio.projects.homeSub")}
          </p>
        </ContextGate>
      </div>
    </div>
  );
}
