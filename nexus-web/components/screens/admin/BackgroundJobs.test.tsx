// Component tests — "Việc nền" (DAILY-QUEUE #33, specs/021 §3 + Nợ còn mở), on SLDS (specs/038).
// Pins the contract the customer admin relies on, through the real lib/admin → fetch wire:
//   * the default view is the failed jobs: GET /admin/jobs?status=failed&limit=50;
//   * a row shows the job type, the rule's NAME (from /admin/workflows; gone ⇒ "Quy tắc đã xoá"),
//     the status, attempts/maxAttempts and the receiver's error VERBATIM;
//   * "Gửi lại" exists ONLY on a failed outbound_webhook whose rule still exists; a failed timer and
//     a webhook of a deleted rule say why they cannot be resent (the handler fails a deleted rule's
//     delivery permanently, so that resend could only fail again); a rule list that could not be
//     read claims nothing ("—", button kept);
//   * "Gửi lại" asks first, POSTs /admin/jobs/:id/retry and the row turns "Đang chờ" 0/10, focus
//     lands on the result notification (the button that opened the prompt is gone);
//   * 409/404 explain and reload the list, 503 says the queue is off;
//   * filters and "Xem thêm" (keyset cursor) feed the next GET;
//   * the Setup menu lists the page for every customer admin (not operator-only).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, within, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { BackgroundJobs } from "@/components/screens/admin/BackgroundJobs";
import { ADMIN_LINKS } from "@/components/slds/shell/adminLinks";
import { logout } from "@/lib/api";
import type { BackgroundJob } from "@/lib/admin";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const WEBHOOK_ERROR =
  "services.OutboundWebhookJobHandler: workflow.HTTPWebhookSender.Deliver: post http://127.0.0.1:8198: status 422";

const WEBHOOK_FAILED: BackgroundJob = {
  id: "0b8f5a9e-1111-4000-8000-000000000001",
  jobType: "outbound_webhook",
  status: "failed",
  attempts: 1,
  maxAttempts: 10,
  lastError: WEBHOOK_ERROR,
  workflowId: "wf-1",
  runAt: "2026-09-30T02:00:00Z",
  createdAt: "2026-09-30T02:00:00Z",
  finishedAt: "2026-09-30T02:00:01Z",
};

const TIMER_FAILED: BackgroundJob = {
  ...WEBHOOK_FAILED,
  id: "0b8f5a9e-2222-4000-8000-000000000002",
  jobType: "workflow_timer",
  attempts: 5,
  maxAttempts: 5,
  lastError: "services.WorkflowTimerJobHandler: load rule: timeout",
  workflowId: "wf-gone",
  createdAt: "2026-09-29T02:00:00Z",
};

// A delivery of a rule that was deleted after it failed.
const WEBHOOK_RULE_GONE: BackgroundJob = {
  ...WEBHOOK_FAILED,
  id: "0b8f5a9e-3333-4000-8000-000000000003",
  workflowId: "wf-gone",
};

// A delivery the receiver answered 200 (#256): it can be sent again when they lost the order.
const WEBHOOK_DONE: BackgroundJob = {
  ...WEBHOOK_FAILED,
  id: "0b8f5a9e-4444-4000-8000-000000000004",
  status: "done",
  lastError: null,
};

// A delivered timer: resending would run every action of its rule again — no button.
const TIMER_DONE: BackgroundJob = {
  ...TIMER_FAILED,
  id: "0b8f5a9e-5555-4000-8000-000000000005",
  status: "done",
  workflowId: "wf-1",
  lastError: null,
};

const WORKFLOWS = [{ id: "wf-1", name: "Báo đơn mới cho kho" }];

interface Calls {
  gets: string[];
  posts: string[];
  workflows: number;
}

// URL-routed fetch: /admin/jobs pages come from `pages` in order (the last one repeats);
// the retry answer is `retry` (default: 200 with the requeued row); the rule list answer is
// `workflows` (default: 200 with WORKFLOWS).
function routedFetch(
  calls: Calls,
  pages: { data: BackgroundJob[]; nextCursor?: string }[],
  retry?: () => Response,
  workflows?: () => Response,
) {
  let page = 0;
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    if (u.includes("/admin/workflows") && method === "GET") {
      calls.workflows++;
      if (workflows) return workflows();
      return jsonRes({ data: { workflows: WORKFLOWS } }, 200);
    }
    if (u.includes("/admin/jobs/") && u.endsWith("/retry") && method === "POST") {
      calls.posts.push(u);
      if (retry) return retry();
      return jsonRes({
        data: { job: { ...WEBHOOK_FAILED, status: "queued", attempts: 0, lastError: null, finishedAt: null } },
      }, 200);
    }
    if (u.includes("/admin/jobs") && method === "GET") {
      calls.gets.push(u);
      const p = pages[Math.min(page++, pages.length - 1)];
      return jsonRes({ data: p.data, pagination: { limit: 50, nextCursor: p.nextCursor ?? null } }, 200);
    }
    return jsonRes({ error: `unmocked ${method} ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
}

const query = (u: string) => new URL(u).searchParams;

// The notification region the screen focuses after a resend (div[tabindex=-1] around it).
const noticeRegion = (text: HTMLElement) => text.closest("[tabindex='-1']") as HTMLElement;

async function confirmResend() {
  fireEvent.click(await screen.findByRole("button", { name: /^Gửi lại lần giao/ }));
  fireEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Gửi lại" }));
}

describe("BackgroundJobs", () => {
  let originalFetch: FetchFn;
  let calls: Calls;

  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout();
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
    calls = { gets: [], posts: [], workflows: 0 };
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("opens on the failed jobs and shows type, rule name, attempts and the error verbatim", async () => {
    global.fetch = routedFetch(calls, [{ data: [WEBHOOK_FAILED, TIMER_FAILED] }]) as unknown as typeof fetch;
    render(<BackgroundJobs />);

    expect(await screen.findByText("Báo đơn mới cho kho")).toBeInTheDocument();
    const q = query(calls.gets[0]);
    expect(q.get("status")).toBe("failed");
    expect(q.get("limit")).toBe("50");
    expect(q.has("type")).toBe(false);

    const grid = screen.getByRole("grid", { name: "Danh sách việc nền" });
    expect(within(grid).getByText(WEBHOOK_ERROR)).toBeInTheDocument();
    expect(within(grid).getByText("Giao webhook")).toBeInTheDocument();
    expect(within(grid).getByText("Hẹn giờ quy tắc")).toBeInTheDocument();
    expect(within(grid).getByText("1/10")).toBeInTheDocument();
    expect(within(grid).getByText("5/5")).toBeInTheDocument();
    // A job whose rule no longer exists says so instead of showing an id.
    expect(within(grid).getByText("Quy tắc đã xoá")).toBeInTheDocument();
    expect(within(grid).getAllByText("Lỗi")).toHaveLength(2);

    // Resend only on the failed webhook; the failed timer explains why not.
    expect(within(grid).getAllByRole("button", { name: /^Gửi lại/ })).toHaveLength(1);
    expect(within(grid).getByText("Không gửi lại tay được")).toBeInTheDocument();
  });

  it("Gửi lại asks first, POSTs the retry and the row turns queued with a fresh budget", async () => {
    global.fetch = routedFetch(calls, [{ data: [WEBHOOK_FAILED] }]) as unknown as typeof fetch;
    render(<BackgroundJobs />);

    fireEvent.click(await screen.findByRole("button", { name: /^Gửi lại lần giao/ }));
    const prompt = await screen.findByRole("alertdialog");
    expect(prompt).toHaveTextContent("Báo đơn mới cho kho");
    expect(prompt).toHaveTextContent("X-Nexus-Delivery-Id");
    expect(calls.posts).toHaveLength(0);

    fireEvent.click(within(prompt).getByRole("button", { name: "Gửi lại" }));
    await waitFor(() => expect(calls.posts).toHaveLength(1));
    expect(calls.posts[0]).toMatch(new RegExp(`/admin/jobs/${WEBHOOK_FAILED.id}/retry$`));

    const done = await screen.findByText(/Đã đưa lần giao vào hàng đợi/);
    // The button that opened the prompt is gone with the row's new state: focus goes to the outcome.
    await waitFor(() => expect(noticeRegion(done)).toHaveFocus());
    await waitFor(() => expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument());
    const grid = screen.getByRole("grid", { name: "Danh sách việc nền" });
    expect(within(grid).getByText("Đang chờ")).toBeInTheDocument();
    expect(within(grid).getByText("0/10")).toBeInTheDocument();
    expect(within(grid).queryByText(WEBHOOK_ERROR)).not.toBeInTheDocument();
    expect(within(grid).queryByRole("button", { name: /^Gửi lại/ })).not.toBeInTheDocument();
  });

  it("a delivered webhook can be sent again (#256) after a prompt that warns about the delivery id", async () => {
    global.fetch = routedFetch(calls, [{ data: [WEBHOOK_DONE, TIMER_DONE] }], () =>
      jsonRes({ data: { job: { ...WEBHOOK_DONE, status: "queued", attempts: 0, finishedAt: null } } }, 200),
    ) as unknown as typeof fetch;
    render(<BackgroundJobs />);

    const grid = await screen.findByRole("grid", { name: "Danh sách việc nền" });
    // Only the delivered webhook gets the button; the delivered timer gets none (and no apology either).
    await waitFor(() => expect(within(grid).getAllByRole("button", { name: /^Gửi lại/ })).toHaveLength(1));
    expect(within(grid).queryByText("Không gửi lại tay được")).not.toBeInTheDocument();

    fireEvent.click(within(grid).getByRole("button", { name: /^Gửi lại lần giao/ }));
    const prompt = await screen.findByRole("alertdialog");
    expect(prompt).toHaveTextContent("Gửi lại lần giao đã thành công?");
    expect(prompt).toHaveTextContent("X-Nexus-Delivery-Id");
    expect(prompt).toHaveTextContent("bỏ qua");
    fireEvent.click(within(prompt).getByRole("button", { name: "Gửi lại" }));
    await waitFor(() => expect(calls.posts).toHaveLength(1));
    expect(calls.posts[0]).toMatch(new RegExp(`/admin/jobs/${WEBHOOK_DONE.id}/retry$`));
    expect(await screen.findByText(/Đã đưa lần giao vào hàng đợi/)).toBeInTheDocument();
    expect(within(grid).getByText("Đang chờ")).toBeInTheDocument();
  });

  it("a 409 explains the job is no longer failed and reloads the list", async () => {
    global.fetch = routedFetch(calls, [{ data: [WEBHOOK_FAILED] }, { data: [] }], () =>
      jsonRes({ error: "Việc nền vừa đổi trạng thái", code: "JOB_NOT_RETRYABLE" }, 409),
    ) as unknown as typeof fetch;
    render(<BackgroundJobs />);
    await confirmResend();

    const notice = await screen.findByText(/vừa đổi trạng thái/);
    await waitFor(() => expect(calls.gets).toHaveLength(2));
    await waitFor(() => expect(noticeRegion(notice)).toHaveFocus());
  });

  it("a 404 explains the job is gone and reloads the list", async () => {
    global.fetch = routedFetch(calls, [{ data: [WEBHOOK_FAILED] }, { data: [] }], () =>
      jsonRes({ error: "Không tìm thấy việc nền", code: "JOB_NOT_FOUND" }, 404),
    ) as unknown as typeof fetch;
    render(<BackgroundJobs />);
    await confirmResend();

    const notice = await screen.findByText("Không còn thấy việc này. Danh sách đã được tải lại.");
    await waitFor(() => expect(calls.gets).toHaveLength(2));
    await waitFor(() => expect(noticeRegion(notice)).toHaveFocus());
    expect(await screen.findByText("Không có việc nền nào bị lỗi")).toBeInTheDocument();
  });

  it("a failed webhook of a deleted rule offers no resend and says why", async () => {
    global.fetch = routedFetch(calls, [{ data: [WEBHOOK_RULE_GONE] }]) as unknown as typeof fetch;
    render(<BackgroundJobs />);

    const grid = await screen.findByRole("grid", { name: "Danh sách việc nền" });
    expect(await within(grid).findByText("Quy tắc đã xoá")).toBeInTheDocument();
    // The handler fails a deleted rule's delivery permanently: a resend could only fail again.
    expect(within(grid).queryByRole("button", { name: /^Gửi lại/ })).not.toBeInTheDocument();
    expect(within(grid).getByText("Không gửi lại tay được")).toBeInTheDocument();

    act(() => within(grid).getByRole("button", { name: "Vì sao không gửi lại được?" }).focus());
    expect(await screen.findByRole("tooltip")).toHaveTextContent(/đã bị xoá/);
    expect(calls.posts).toHaveLength(0);
  });

  it("a rule list that cannot be read claims nothing: rule '—', resend kept", async () => {
    global.fetch = routedFetch(calls, [{ data: [WEBHOOK_RULE_GONE] }], undefined, () =>
      jsonRes({ error: "boom", code: "INTERNAL" }, 500),
    ) as unknown as typeof fetch;
    render(<BackgroundJobs />);

    const grid = await screen.findByRole("grid", { name: "Danh sách việc nền" });
    await waitFor(() => expect(calls.workflows).toBe(1));
    // Let the failed read settle (undefined "loading" and null "unreadable" look the same on purpose).
    await act(() => new Promise((r) => setTimeout(r, 30)));
    expect(within(grid).queryByText("Quy tắc đã xoá")).not.toBeInTheDocument();
    expect(within(grid).getByText("—")).toBeInTheDocument();
    expect(within(grid).getByRole("button", { name: /^Gửi lại lần giao/ })).toBeInTheDocument();
  });

  it("a 503 says the queue is switched off", async () => {
    global.fetch = routedFetch(calls, [{ data: [WEBHOOK_FAILED] }], () =>
      jsonRes({ error: "Hàng đợi việc nền đang tắt (JOBS_ENABLED=false)", code: "JOBS_DISABLED" }, 503),
    ) as unknown as typeof fetch;
    render(<BackgroundJobs />);

    fireEvent.click(await screen.findByRole("button", { name: /^Gửi lại lần giao/ }));
    fireEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Gửi lại" }));

    expect(await screen.findByText(/Hàng đợi việc nền của hệ thống đang tắt/)).toBeInTheDocument();
    // The row is untouched: still failed, still resendable once the queue is back.
    const grid = screen.getByRole("grid", { name: "Danh sách việc nền" });
    expect(within(grid).getByText("1/10")).toBeInTheDocument();
  });

  it("filters and Xem thêm feed the next GET", async () => {
    global.fetch = routedFetch(calls, [
      { data: [WEBHOOK_FAILED], nextCursor: "cur-1" },
      { data: [TIMER_FAILED] },
    ]) as unknown as typeof fetch;
    render(<BackgroundJobs />);

    fireEvent.click(await screen.findByRole("button", { name: "Xem thêm" }));
    expect(await screen.findByText("Hẹn giờ quy tắc")).toBeInTheDocument();
    expect(query(calls.gets[1]).get("after")).toBe("cur-1");
    expect(query(calls.gets[1]).get("status")).toBe("failed");
    expect(screen.getByText(WEBHOOK_ERROR)).toBeInTheDocument(); // appended, not replaced

    await userEvent.click(screen.getByRole("combobox", { name: "Trạng thái" }));
    await userEvent.click(screen.getByRole("option", { name: "Mọi trạng thái" }));
    await waitFor(() => expect(calls.gets).toHaveLength(3));
    expect(query(calls.gets[2]).has("status")).toBe(false);
    expect(query(calls.gets[2]).has("after")).toBe(false);

    await userEvent.click(screen.getByRole("combobox", { name: "Loại việc" }));
    await userEvent.click(screen.getByRole("option", { name: "Giao webhook" }));
    await waitFor(() => expect(calls.gets).toHaveLength(4));
    expect(query(calls.gets[3]).get("type")).toBe("outbound_webhook");
  });

  // specs/066 L1b: a rule's mail is a tenant-pinned job now. It reads "Gửi email" (never the machine
  // name), can be filtered on, has no "Gửi lại" here and points to Nhật ký email instead.
  it("labels email_alert 'Gửi email', filters on it and points to Nhật ký email", async () => {
    const EMAIL_FAILED: BackgroundJob = { ...WEBHOOK_FAILED, id: "0b8f5a9e-4444-4000-8000-000000000004", jobType: "email_alert", workflowId: "wf-1" };
    global.fetch = routedFetch(calls, [{ data: [EMAIL_FAILED] }]) as unknown as typeof fetch;
    render(<BackgroundJobs />);

    expect(await screen.findByText("Gửi email", { selector: "td *, td" })).toBeInTheDocument();
    expect(screen.queryByText("email_alert")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Gửi lại lần giao/ })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Xem ở Nhật ký email" })).toHaveAttribute("href", "/admin/email-log");
    expect(screen.queryByText("Không gửi lại tay được")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("combobox", { name: "Loại việc" }));
    await userEvent.click(screen.getByRole("option", { name: "Gửi email" }));
    await waitFor(() => expect(calls.gets).toHaveLength(2));
    expect(query(calls.gets[1]).get("type")).toBe("email_alert");
  });

  // #342: the BE answers credentialName (null = no key / key deleted) on every row once the payload
  // names a vault key; a server without the field (older build) gets no column and no notice.
  it("shows the connection key name in a 'Khoá' column when the server sends it", async () => {
    const WITH_KEY: BackgroundJob = { ...WEBHOOK_FAILED, credentialName: "stripe-prod" };
    const NO_KEY: BackgroundJob = { ...TIMER_FAILED, credentialName: null };
    global.fetch = routedFetch(calls, [{ data: [WITH_KEY, NO_KEY] }]) as unknown as typeof fetch;
    render(<BackgroundJobs />);

    const grid = await screen.findByRole("grid", { name: "Danh sách việc nền" });
    expect(await within(grid).findByRole("columnheader", { name: "Khoá" })).toBeInTheDocument();
    expect(within(grid).getByText("stripe-prod")).toBeInTheDocument();
  });

  it("has no 'Khoá' column when the server does not send credentialName", async () => {
    global.fetch = routedFetch(calls, [{ data: [WEBHOOK_FAILED, TIMER_FAILED] }]) as unknown as typeof fetch;
    render(<BackgroundJobs />);

    const grid = await screen.findByRole("grid", { name: "Danh sách việc nền" });
    expect(await within(grid).findByText(WEBHOOK_ERROR)).toBeInTheDocument();
    expect(within(grid).queryByRole("columnheader", { name: "Khoá" })).not.toBeInTheDocument();
  });

  it("an empty failed view says nothing failed", async () => {
    global.fetch = routedFetch(calls, [{ data: [] }]) as unknown as typeof fetch;
    render(<BackgroundJobs />);
    expect(await screen.findByText("Không có việc nền nào bị lỗi")).toBeInTheDocument();
  });

  it("the Setup menu lists Việc nền for every customer admin", () => {
    const link = ADMIN_LINKS.find((l) => l.href === "/admin/jobs");
    expect(link).toBeDefined();
    expect(link?.operatorOnly).toBeFalsy();
  });
});
