// Vận hành › Tự động: the 24h activity feed and the failure count behind the lede and the tab dot
// (specs/078 lát u7-tu-dong-tong-quan, FR-070, FR-071, FR-079).
import { useProjectQuery } from "@/components/studio/useProjectQuery";
import { getTenantHealth, listBackgroundJobs, listEmailLog, type BackgroundJob, type EmailLogEntry } from "@/lib/admin";
import { listRecentWorkflowRuns } from "@/lib/api";
import { healthLede } from "@/lib/operateHealth";
import type { WorkflowRun } from "@/lib/workflowRuns";

// ponytail: window of 50 rows per source; move to GET /admin/health when #414 lands (rule runs, email and inbound only; jobs have a 7-day count only).
export const WINDOW = 50;
const DAY = 86_400_000;

/** A source that is `undefined` was not read (error or not loaded): it is "unknown", never 0. */
export interface FeedSources {
  runs?: WorkflowRun[];
  jobs?: BackgroundJob[];
  emails?: EmailLogEntry[];
}

export interface ActivityItem {
  id: string;
  kind: "run" | "job" | "email";
  at: string;
  failed: boolean;
  /** rule id for runs/jobs (link to the run history) */
  workflowId: string | null;
  detail: string | null;
  retryable: boolean;
  /** a failed email the server says can be sent again (Gửi lại, two phases) */
  resendable?: boolean;
}

/** Job errors arrive as Go wrap chains (`services.X: pkg.y: cause`); the builder reads the cause, not the call path. */
const stripGoPrefix = (e: string | null) => e && e.replace(/^(?:[A-Za-z_]+\.[A-Za-z_]+: )+/, "");
const ms = (iso: string) => new Date(iso).getTime();
const within = (iso: string, now: number) => now - ms(iso) <= DAY;

export function buildActivityFeed(src: FeedSources, now = Date.now()): ActivityItem[] {
  const items: ActivityItem[] = [
    ...(src.runs ?? []).map((r): ActivityItem => ({ id: r.id, kind: "run", at: r.startedAt, failed: r.status === "failed", workflowId: r.workflowId, detail: r.error, retryable: false })),
    ...(src.jobs ?? []).filter((j) => j.status === "failed").map((j): ActivityItem => ({ id: j.id, kind: "job", at: j.createdAt, failed: true, workflowId: j.workflowId, detail: stripGoPrefix(j.lastError), retryable: j.jobType === "outbound_webhook" })),
    ...(src.emails ?? []).filter((e) => e.status === "failed").map((e): ActivityItem => ({ id: e.id, kind: "email", at: e.createdAt, failed: true, workflowId: e.workflowId, detail: e.lastError, retryable: false, resendable: e.canResend })),
  ];
  return items.filter((i) => within(i.at, now)).sort((a, b) => ms(b.at) - ms(a.at));
}

export interface FailureCount {
  runs: number;
  jobs: number;
  email: number;
  total: number;
  /** "từ N trở lên": a source filled the window and its oldest row is still inside 24h */
  atLeast: boolean;
  /** names of the sources that could not be read */
  unknown: string[];
  /** "Không lỗi" is allowed only when this is true: all three sources read and no failure */
  clean: boolean;
}

type Row = { at: string; bad: boolean };
const countBad = (rows: Row[] | undefined, now: number) => rows?.filter((r) => r.bad && within(r.at, now)).length ?? 0;
const full = (rows: Row[] | undefined, now: number) => !!rows && rows.length >= WINDOW && within(rows[rows.length - 1].at, now);

export function countRecentFailures(src: FeedSources, now = Date.now()): FailureCount {
  const runs = src.runs?.map((r) => ({ at: r.startedAt, bad: r.status === "failed" }));
  const jobs = src.jobs?.map((j) => ({ at: j.createdAt, bad: j.status === "failed" }));
  const emails = src.emails?.map((e) => ({ at: e.createdAt, bad: e.status === "failed" }));
  const unknown = [...(runs ? [] : ["lần chạy luật"]), ...(jobs ? [] : ["việc chạy nền"]), ...(emails ? [] : ["email"])];
  const c = { runs: countBad(runs, now), jobs: countBad(jobs, now), email: countBad(emails, now) };
  const total = c.runs + c.jobs + c.email;
  return { ...c, total, atLeast: [runs, jobs, emails].some((r) => full(r, now)), unknown, clean: unknown.length === 0 && total === 0 };
}

/** The three reads behind the overview and the tab dot: same keys, so the dot costs no extra call (FR-079). */
export function useOperateFeedSources(slug: string) {
  const runs = useProjectQuery(slug, ["operate", "runs", "recent"], () => listRecentWorkflowRuns(WINDOW));
  const jobs = useProjectQuery(slug, ["operate", "jobs", "failed"], async () => (await listBackgroundJobs({ status: "failed", limit: WINDOW })).jobs);
  const email = useProjectQuery(slug, ["operate", "email", "failed"], async () => (await listEmailLog({ status: "failed", limit: WINDOW })).entries);
  // A failed refetch keeps the last data (react-query), so a dot that is lit stays lit.
  return { runs, jobs, email, sources: { runs: runs.data, jobs: jobs.data, emails: email.data } as FeedSources };
}

/** Dot on the "Tự động" tab: any failure in the last 24h. A source that cannot be read never turns an existing dot off. */
export function useOperateAttention(slug: string): boolean {
  const { sources } = useOperateFeedSources(slug);
  // Same key as the Sức khoẻ page (FR-079): no extra call once it is loaded. A health read that failed adds nothing.
  const health = useProjectQuery(slug, ["operate", "health"], getTenantHealth, { retry: false });
  return countRecentFailures(sources).total > 0 || healthLede(health.data, slug).n > 0;
}
