// Vận hành › Sức khoẻ (specs/078 lát u7-suc-khoe, FR-103): pure mapping from GET /admin/health to the lede
// and the list of signals. The overall label `status` is the server's (DeriveHealth); nothing here re-derives it.
// Fail-closed: a block that is absent or null is "unknown", never 0 and never "ổn" (FR-071, FR-103).
import type { TenantHealth } from "@/lib/admin";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { studioHref } from "@/lib/studioRoutes";

export type SignalLevel = "critical" | "warn" | "unknown";

export interface HealthSignal {
  id: string;
  level: SignalLevel;
  text: string;
  /** exactly one button: a link, or `retry` (refetch the health read) */
  action: { label: string; href?: string; retry?: true };
}

export interface HealthLede {
  /** n = rule runs + emails + inbound hooks failed in 24h (the server has no 24h number for background jobs) */
  n: number;
  /** the three parts of n, for the overview lede (each links to its own page) */
  parts: { runs: number; emails: number; hooks: number };
  /** m = background jobs failed in 7 days, summed over job types */
  m: number;
  /** names of the sources that could not be read */
  unknown: string[];
  /** "không lỗi" is allowed only when every source was read and n is 0 */
  clean: boolean;
  href: string;
}

type Partial24 = { failed24h?: unknown };
const num = (v: unknown): v is number => typeof v === "number" && Number.isFinite(v);

export function healthLede(h: TenantHealth | null | undefined, slug: string): HealthLede {
  const unknown: string[] = [];
  const read = (block: Partial24 | null | undefined, name: I18nKey): number => {
    if (!block || !num(block.failed24h)) {
      unknown.push(t(name));
      return 0;
    }
    return block.failed24h;
  };
  const runs = read(h?.workflowRuns, "studio.operate.health.src.runs");
  const mails = read(h?.emails, "studio.operate.health.src.emails");
  const hooks = read(h?.inboundHooks, "studio.operate.health.src.hooks");
  const jobs = h?.jobs && Array.isArray(h.jobs.failed7d) ? h.jobs.failed7d : null;
  if (!jobs) unknown.push(t("studio.operate.health.src.jobs"));
  const n = runs + mails + hooks;
  return {
    n,
    parts: { runs, emails: mails, hooks },
    m: (jobs ?? []).reduce((s, j) => s + (num(j.count) ? j.count : 0), 0),
    unknown,
    clean: unknown.length === 0 && n === 0,
    href: studioHref(slug, "van-hanh/tu-dong"),
  };
}

const RANK: Record<SignalLevel, number> = { critical: 0, warn: 1, unknown: 2 };

/** Only the signals that have a problem (+ "Chưa khám"); [] means everything is read and fine. */
export function healthSignals(h: TenantHealth | null | undefined, slug: string, now = Date.now()): HealthSignal[] {
  const out: HealthSignal[] = [];
  const to = (route: Parameters<typeof studioHref>[1], query?: Record<string, string>) => studioHref(slug, route, {}, query);
  const lost = (id: string, src: I18nKey) =>
    out.push({ id: `lost-${id}`, level: "unknown", text: tf("studio.operate.health.lost", { src: t(src) }), action: { label: t("studio.query.retry"), retry: true } });

  const jobs = h?.jobs;
  if (!jobs || !Array.isArray(jobs.failed7d) || !num(jobs.stuck)) lost("jobs", "studio.operate.health.src.jobs");
  else {
    const jobsHref = to("van-hanh/tu-dong/viec-nen");
    if (jobs.stuck > 0) out.push({ id: "jobs-stuck", level: "critical", text: tf("studio.operate.health.stuck", { n: jobs.stuck }), action: { label: t("studio.operate.health.openJobs"), href: jobsHref } });
    for (const j of jobs.failed7d) {
      const recent = now - Date.parse(j.lastAt) <= 86_400_000;
      const type = t(`page.jobs.type.${j.jobType}` as I18nKey) || j.jobType;
      out.push({ id: `jobs-${j.jobType}`, level: "warn", text: tf(recent ? "studio.operate.health.jobFailedRecent" : "studio.operate.health.jobFailed", { n: j.count, type }), action: { label: t("studio.operate.health.openLog"), href: jobsHref } });
    }
  }

  const runs = h?.workflowRuns;
  if (!runs || !num(runs.failed24h) || !Array.isArray(runs.topFailing)) lost("runs", "studio.operate.health.src.runs");
  else if (runs.topFailing.length > 0) {
    const top = runs.topFailing[0];
    out.push({ id: "runs", level: "warn", text: tf("studio.operate.health.ruleTop", { name: top.name, n: top.failed }), action: { label: t("studio.operate.health.openLog"), href: to("van-hanh/tu-dong/lan-chay", { luat: top.workflowId }) } });
  } else if (runs.failed24h > 0) {
    out.push({ id: "runs", level: "warn", text: tf("studio.operate.health.rules24", { n: runs.failed24h }), action: { label: t("studio.operate.health.openLog"), href: to("van-hanh/tu-dong/lan-chay") } });
  }

  const mail = h?.emails;
  if (!mail || !num(mail.failed24h) || !num(mail.unresentFailed)) lost("emails", "studio.operate.health.src.emails");
  else if (mail.unresentFailed > 0 || mail.failed24h > 0) {
    const text = mail.unresentFailed > 0 ? tf("studio.operate.health.mailUnsent", { n: mail.unresentFailed }) : tf("studio.operate.health.mail24", { n: mail.failed24h });
    out.push({ id: "emails", level: "warn", text, action: { label: t("studio.operate.health.openMail"), href: to("van-hanh/tu-dong/email", { "trang-thai": "failed" }) } });
  }

  const hooks = h?.inboundHooks;
  if (!hooks || !num(hooks.failed24h) || !Array.isArray(hooks.byHook)) lost("hooks", "studio.operate.health.src.hooks");
  else if (hooks.byHook.length > 0) {
    for (const k of hooks.byHook) out.push({ id: `hook-${k.hookId}`, level: "warn", text: tf("studio.operate.health.hook", { name: k.name, n: k.failed }), action: { label: t("studio.operate.health.openHook"), href: to("van-hanh/tu-dong/cong-nhan", { cong: k.hookId }) } });
  } else if (hooks.failed24h > 0) {
    out.push({ id: "hooks", level: "warn", text: tf("studio.operate.health.hooks24", { n: hooks.failed24h }), action: { label: t("studio.operate.health.openHook"), href: to("van-hanh/tu-dong/cong-nhan") } });
  }

  // Doctor: the sweep that never ran says so but does not move the overall level (server rule), so it is "unknown".
  const doc = h?.doctor;
  const check = to("bao-tri/kham");
  if (!doc) lost("doctor", "studio.operate.health.src.doctor");
  else if (!doc.checkedAt) out.push({ id: "doctor", level: "unknown", text: t("studio.operate.health.neverChecked"), action: { label: t("studio.operate.health.openCheck"), href: check } });
  else if (doc.status === "critical" || doc.status === "warn") {
    out.push({ id: "doctor", level: doc.status, text: tf("studio.operate.health.doctor", { c: doc.critical, w: doc.warn }), action: { label: t("studio.operate.health.openCheck"), href: check } });
  }

  return out.map((s, i) => ({ s, i })).sort((a, b) => RANK[a.s.level] - RANK[b.s.level] || a.i - b.i).map((x) => x.s);
}
