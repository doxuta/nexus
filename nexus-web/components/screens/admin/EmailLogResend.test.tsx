// Component tests — "Gửi lại" on the Nhật ký email screen (specs/066 L2), on SLDS, through the real
// lib/admin → fetch wire:
//   * the button exists only on rows the server marks canResend;
//   * it opens a Modal that FIRST asks for the preview (POST {}) and shows the masked recipient, the
//     subject as it will be sent, the "rebuilt from current data" + "may arrive twice" lines, and a warning
//     when the rule changed since the send — nothing is sent before the second click;
//   * confirm sends {dryRun:false}, toasts and reloads the list; a lost double press (409) says so and
//     reloads; a server without mail (503) blocks the confirm;
//   * a resent row reads "Đã gửi lại · HH:mm", the row a resend created carries "Gửi lại" — no cross link.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { EmailLog } from "@/components/screens/admin/EmailLog";
import { Toaster } from "@/components/slds/Toast";
import { logout } from "@/lib/api";
import type { EmailLogEntry } from "@/lib/admin";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const BASE: EmailLogEntry = {
  id: "log-1",
  createdAt: "2026-10-01T02:00:00Z",
  finishedAt: "2026-10-01T02:00:05Z",
  source: "workflow",
  status: "failed",
  recipientMasked: "a***@gmail.com",
  subjectTemplate: "Chào {{ho_ten}}",
  workflowId: "wf-1",
  workflowName: "Chào khách mới",
  actionIndex: 1,
  entityName: "crm_contacts",
  recordId: "rec-1",
  jobId: "job-1",
  attempts: 1,
  lastError: "boom [redacted]",
  resendOf: null,
  resentAt: null,
  createdBy: null,
  createdByName: null,
  canResend: true,
};

const PREVIEW = { dryRun: true, recipientMasked: "l***@x.vn", subject: "Chào Lan", ruleChangedSinceSend: false };

interface Calls { gets: string[]; posts: { url: string; body: unknown }[] }

function routedFetch(
  calls: Calls,
  pages: EmailLogEntry[][],
  post: (body: { dryRun?: boolean }) => Response,
) {
  let page = 0;
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    if (u.includes("/metadata/entities") && method === "GET") {
      return jsonRes({ data: [{ name: "crm_contacts", label: "Liên hệ", tableName: "obj_crm_contacts" }] }, 200);
    }
    if (u.includes("/admin/email-log") && method === "GET") {
      calls.gets.push(u);
      const data = pages[Math.min(page++, pages.length - 1)];
      return jsonRes({ data, pagination: { limit: 50, nextCursor: null }, meta: { mailConfigured: true, queueEnabled: true } }, 200);
    }
    if (/\/admin\/email-log\/[^/]+\/resend$/.test(u) && method === "POST") {
      const body = JSON.parse(String(init?.body ?? "{}"));
      calls.posts.push({ url: u, body });
      return post(body);
    }
    return jsonRes({ error: `unmocked ${method} ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
}

describe("EmailLog — Gửi lại", () => {
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
    window.history.pushState({}, "", "/admin/email-log");
    calls = { gets: [], posts: [] };
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.history.pushState({}, "", "/");
  });

  const open = async () => {
    fireEvent.click(await screen.findByRole("button", { name: /Gửi lại email lúc/ }));
    return screen.findByRole("dialog");
  };

  it("shows the button only on rows the server says canResend", async () => {
    const other: EmailLogEntry = { ...BASE, id: "log-2", recipientMasked: "z***@x.vn", canResend: false, resentAt: "2026-10-01T03:30:00Z" };
    window.history.pushState({}, "", "/admin/email-log?status=");
    global.fetch = routedFetch(calls, [[BASE, other]], () => jsonRes({}, 500)) as unknown as typeof fetch;
    render(<EmailLog />);
    await screen.findByText("z***@x.vn");
    expect(screen.getAllByRole("button", { name: /Gửi lại email lúc/ })).toHaveLength(1);
  });

  it("previews first (POST {}), explains, and sends nothing before the second click", async () => {
    global.fetch = routedFetch(calls, [[BASE]], () => jsonRes({ data: PREVIEW }, 200)) as unknown as typeof fetch;
    render(<EmailLog />);
    const dialog = await open();

    expect(await within(dialog).findByText("l***@x.vn")).toBeInTheDocument();
    expect(within(dialog).getByText("Chào Lan")).toBeInTheDocument();
    expect(within(dialog).getByText("Chào khách mới")).toBeInTheDocument();
    expect(within(dialog).getByText(/dựng lại từ quy tắc đang chạy/)).toBeInTheDocument();
    expect(within(dialog).getByText(/có thể nhận hai thư/)).toBeInTheDocument();
    expect(within(dialog).queryByText(/Luật đã đổi/)).not.toBeInTheDocument();
    expect(calls.posts).toHaveLength(1);
    expect(calls.posts[0].url).toContain("/admin/email-log/log-1/resend");
    expect(calls.posts[0].body).toEqual({});
  });

  it("warns when the rule changed since the send", async () => {
    global.fetch = routedFetch(calls, [[BASE]], () => jsonRes({ data: { ...PREVIEW, ruleChangedSinceSend: true } }, 200)) as unknown as typeof fetch;
    render(<EmailLog />);
    const dialog = await open();
    expect(await within(dialog).findByText(/Quy tắc đã đổi kể từ lần gửi/)).toBeInTheDocument();
  });

  it("confirm sends {dryRun:false}, toasts and reloads the list", async () => {
    global.fetch = routedFetch(
      calls,
      [[BASE], [{ ...BASE, resentAt: "2026-10-01T03:30:00Z", canResend: false }]],
      (body) => (body.dryRun === false ? jsonRes({ data: { dryRun: false, entry: { ...BASE, id: "log-new", status: "queued", resendOf: "log-1", canResend: false } } }, 200) : jsonRes({ data: PREVIEW }, 200)),
    ) as unknown as typeof fetch;
    render(<><Toaster /><EmailLog /></>);
    const dialog = await open();
    await within(dialog).findByText("l***@x.vn");
    fireEvent.click(within(dialog).getByRole("button", { name: "Gửi lại" }));

    await waitFor(() => expect(calls.posts).toHaveLength(2));
    expect(calls.posts[1].body).toEqual({ dryRun: false });
    expect(await screen.findByText(/Đã đưa email vào hàng gửi/)).toBeInTheDocument();
    await waitFor(() => expect(calls.gets.length).toBeGreaterThanOrEqual(2));
    expect(await screen.findByText("Đã gửi lại · 10:30")).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("a lost double press (409) says so, closes and reloads", async () => {
    global.fetch = routedFetch(
      calls,
      [[BASE], [{ ...BASE, resentAt: "2026-10-01T03:30:00Z", canResend: false }]],
      (body) => (body.dryRun === false ? jsonRes({ error: "đã gửi lại", code: "EMAIL_NOT_RESENDABLE" }, 409) : jsonRes({ data: PREVIEW }, 200)),
    ) as unknown as typeof fetch;
    render(<><Toaster /><EmailLog /></>);
    const dialog = await open();
    await within(dialog).findByText("l***@x.vn");
    fireEvent.click(within(dialog).getByRole("button", { name: "Gửi lại" }));

    expect(await screen.findByText(/vừa được gửi lại/)).toBeInTheDocument();
    await waitFor(() => expect(calls.gets.length).toBeGreaterThanOrEqual(2));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("a server without mail (503) blocks the confirm with an alert", async () => {
    global.fetch = routedFetch(calls, [[BASE]], () => jsonRes({ error: "x", code: "MAIL_NOT_CONFIGURED" }, 503)) as unknown as typeof fetch;
    render(<EmailLog />);
    const dialog = await open();
    expect(await within(dialog).findByText(/chưa cấu hình gửi email/)).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Gửi lại" })).toBeDisabled();
  });

  it("a record that is gone reads plainly in the dialog", async () => {
    global.fetch = routedFetch(calls, [[BASE]], () => jsonRes({ error: "x", code: "EMAIL_RESEND_RECORD_GONE" }, 409)) as unknown as typeof fetch;
    render(<EmailLog />);
    const dialog = await open();
    expect(await within(dialog).findByText(/thùng rác nên không gửi lại được/)).toBeInTheDocument();
  });

  it("a switched-off rule says to turn it back on, in the dialog", async () => {
    global.fetch = routedFetch(calls, [[BASE]], () => jsonRes({ error: "x", code: "EMAIL_RESEND_RULE_DISABLED" }, 409)) as unknown as typeof fetch;
    render(<EmailLog />);
    const dialog = await open();
    expect(await within(dialog).findByText(/Quy tắc đang tắt nên chưa gửi lại được/)).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Gửi lại" })).toBeDisabled();
  });

  it("a preview that loses the race (409 NOT_RESENDABLE) closes the dialog and reloads the list", async () => {
    global.fetch = routedFetch(
      calls,
      [[BASE], [{ ...BASE, resentAt: "2026-10-01T03:30:00Z", canResend: false }]],
      () => jsonRes({ error: "đã gửi lại", code: "EMAIL_NOT_RESENDABLE" }, 409),
    ) as unknown as typeof fetch;
    render(<><Toaster /><EmailLog /></>);
    fireEvent.click(await screen.findByRole("button", { name: /Gửi lại email lúc/ }));
    expect(await screen.findByText(/vừa được gửi lại/)).toBeInTheDocument();
    await waitFor(() => expect(calls.gets.length).toBeGreaterThanOrEqual(2));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("labels a resent row and the row a resend created, without cross links", async () => {
    window.history.pushState({}, "", "/admin/email-log?status=");
    const resent: EmailLogEntry = { ...BASE, canResend: false, resentAt: "2026-10-01T03:30:00Z" };
    const created: EmailLogEntry = { ...BASE, id: "log-new", status: "sent", recipientMasked: "n***@x.vn", resendOf: "log-1", canResend: false };
    global.fetch = routedFetch(calls, [[created, resent]], () => jsonRes({}, 500)) as unknown as typeof fetch;
    render(<EmailLog />);
    expect(await screen.findByText("Đã gửi lại · 10:30")).toBeInTheDocument();
    expect(screen.getByText("Gửi lại", { selector: ".slds-badge" })).toBeInTheDocument();
    // The only links are the two record links (table label) — nothing points from one row to the other.
    expect(screen.getAllByRole("link").every((a) => a.getAttribute("href") === "/crm_contacts/rec-1")).toBe(true);
    expect(screen.queryByRole("button", { name: /Gửi lại email lúc/ })).not.toBeInTheDocument();
  });
});
