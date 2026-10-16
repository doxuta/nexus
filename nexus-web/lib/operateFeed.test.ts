// specs/078 lát u7-tu-dong-tong-quan T103: the 24h activity feed of Vận hành › Tự động.
import { describe, expect, it } from "vitest";
import type { BackgroundJob, EmailLogEntry } from "@/lib/admin";
import type { WorkflowRun } from "@/lib/workflowRuns";
import { buildActivityFeed, countRecentFailures, WINDOW } from "./operateFeed";

const NOW = Date.parse("2026-10-14T10:00:00Z");
const ago = (h: number) => new Date(NOW - h * 3_600_000).toISOString();
const run = (id: string, h: number, status = "success"): WorkflowRun =>
  ({ id, workflowId: "w1", triggerType: "on_create", recordId: null, status, error: null, actionsResult: null, startedAt: ago(h), finishedAt: ago(h) });
const job = (id: string, h: number, over: Partial<BackgroundJob> = {}): BackgroundJob =>
  ({ id, jobType: "outbound_webhook", status: "failed", attempts: 3, maxAttempts: 3, lastError: "HTTP 500", workflowId: "w1", runAt: ago(h), createdAt: ago(h), finishedAt: ago(h), ...over });
const mail = (id: string, h: number): EmailLogEntry =>
  ({ id, createdAt: ago(h), status: "failed", source: "workflow", recipientMasked: "a***@x.vn", lastError: "smtp" }) as EmailLogEntry;

describe("buildActivityFeed", () => {
  it("merges the three sources, newest first, inside 24h only", () => {
    const feed = buildActivityFeed({ runs: [run("r1", 5), run("old", 30)], jobs: [job("j1", 1)], emails: [mail("e1", 3)] }, NOW);
    expect(feed.map((i) => i.id)).toEqual(["j1", "e1", "r1"]);
  });
  it("retryable only for a failed outbound_webhook job", () => {
    const feed = buildActivityFeed({ runs: [run("r1", 2, "failed")], jobs: [job("j1", 1), job("j2", 1, { jobType: "email_send" })], emails: [mail("e1", 1)] }, NOW);
    expect(feed.filter((i) => i.retryable).map((i) => i.id)).toEqual(["j1"]);
  });
});

describe("countRecentFailures", () => {
  it("counts per source inside the window", () => {
    const c = countRecentFailures({ runs: [run("a", 1, "failed"), run("b", 2), run("c", 40, "failed")], jobs: [job("j", 1)], emails: [] }, NOW);
    expect(c).toMatchObject({ runs: 1, jobs: 1, email: 0, total: 2, unknown: [], clean: false, atLeast: false });
  });
  it("clean only when all three sources loaded and empty", () => {
    expect(countRecentFailures({ runs: [], jobs: [], emails: [] }, NOW).clean).toBe(true);
  });
  it("a source that failed to load is unknown, never zero, never clean (FR-071)", () => {
    const c = countRecentFailures({ runs: undefined, jobs: [], emails: [] }, NOW);
    expect(c.unknown).toEqual(["lần chạy luật"]);
    expect(c.clean).toBe(false);
    expect(countRecentFailures({ runs: [], jobs: undefined, emails: undefined }, NOW).unknown).toEqual(["việc chạy nền", "email"]);
  });
  it("atLeast when a source fills the window and its oldest row is still inside 24h", () => {
    const many = Array.from({ length: WINDOW }, (_, i) => job(`j${i}`, 1 + i * 0.01));
    expect(countRecentFailures({ runs: [], jobs: many, emails: [] }, NOW)).toMatchObject({ jobs: WINDOW, atLeast: true });
    const spread = many.map((j, i) => (i === WINDOW - 1 ? { ...j, createdAt: ago(30) } : j));
    expect(countRecentFailures({ runs: [], jobs: spread, emails: [] }, NOW).atLeast).toBe(false);
  });
});

describe("job error detail", () => {
  it("drops the Go wrap-chain prefix, keeps the cause", () => {
    const [i] = buildActivityFeed({ jobs: [job("j1", 1, { lastError: "services.OutboundWebhookJobHandler: dial tcp 127.0.0.1:9: connect: connection refused" })] }, NOW);
    expect(i.detail).toBe("dial tcp 127.0.0.1:9: connect: connection refused");
  });
});
