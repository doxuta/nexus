// specs/078 lát u7-email-gui-lai (T148): "Gửi lại" inside the Studio frame is two phases in the page, gated by the scope.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const GUARD = vi.hoisted(() => ({ ok: true }));
vi.mock("@/components/studio/operate/useScopeGate", () => ({ useScopeGate: () => ({ assertScope: () => GUARD.ok }) }));

import { EmailLog } from "@/components/screens/admin/EmailLog";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { logout } from "@/lib/api";
import type { EmailLogEntry } from "@/lib/admin";

const jsonRes = (body: unknown, status: number) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const ROW: EmailLogEntry = {
  id: "log-1", createdAt: "2026-10-01T02:00:00Z", finishedAt: null, source: "workflow", status: "failed",
  recipientMasked: "a***@gmail.com", subjectTemplate: "Chào", workflowId: "wf-1", workflowName: "Chào khách",
  actionIndex: 1, entityName: "crm_contacts", recordId: "rec-1", jobId: "j", attempts: 1, lastError: "refused",
  resendOf: null, resentAt: null, createdBy: null, createdByName: null, canResend: true,
};

let posts: unknown[];
let postReply: () => Response;

function mock(rows: EmailLogEntry[], preview: Record<string, unknown> = {}) {
  global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    if (u.includes("/metadata/entities")) return jsonRes({ data: [{ name: "crm_contacts", label: "Liên hệ", tableName: "t" }] }, 200);
    if (u.includes("/resend")) {
      const body = JSON.parse(String(init?.body ?? "{}"));
      posts.push(body);
      if (body.dryRun === false) return postReply();
      return jsonRes({ data: { dryRun: true, recipientMasked: "a***@gmail.com", subject: "Chào An", ruleChangedSinceSend: false, ...preview } }, 200);
    }
    if (u.includes("/admin/email-log")) return jsonRes({ data: rows, pagination: { limit: 50, nextCursor: null }, meta: { mailConfigured: true, queueEnabled: true } }, 200);
    return jsonRes({ error: "unmocked", code: "TEST_UNMOCKED" }, 500);
  }) as unknown as typeof fetch;
}

describe("EmailLog embedded: resend", () => {
  const orig = global.fetch;
  beforeEach(async () => {
    GUARD.ok = true;
    posts = [];
    postReply = () => jsonRes({ data: { dryRun: false, id: "log-2" } }, 200);
    await logout();
    window.localStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "t");
  });
  afterEach(() => { global.fetch = orig; });

  const open = async () => {
    render(<EmbeddedSurface><EmailLog initialStatus="failed" /></EmbeddedSurface>);
    await userEvent.click(await screen.findByRole("button", { name: /Gửi lại email lúc/ }));
  };

  it("shows no Gửi lại when the server did not say canResend", async () => {
    mock([{ ...ROW, canResend: false }]);
    render(<EmbeddedSurface><EmailLog initialStatus="failed" /></EmbeddedSurface>);
    await screen.findByRole("grid");
    expect(screen.queryByRole("button", { name: /Gửi lại/ })).toBeNull();
  });

  it("phase 1 previews with dryRun and the rule-changed warning; phase 2 sends", async () => {
    mock([ROW], { ruleChangedSinceSend: true });
    await open();
    expect(await screen.findByText(/Sẽ gửi tới a\*\*\*@gmail\.com với tiêu đề “Chào An”/)).toBeInTheDocument();
    expect(screen.getByText(/Luật đã đổi kể từ lần gửi/)).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(posts).toEqual([{}]);
    await userEvent.click(screen.getByRole("button", { name: "Gửi lại" }));
    await waitFor(() => expect(posts).toEqual([{}, { dryRun: false }]));
  });

  it("scope changed between the two phases: phase 2 makes 0 calls", async () => {
    mock([ROW]);
    await open();
    await screen.findByText(/Sẽ gửi tới/);
    GUARD.ok = false;
    await userEvent.click(screen.getByRole("button", { name: "Gửi lại" }));
    await waitFor(() => expect(screen.queryByText(/Sẽ gửi tới/)).toBeNull());
    expect(posts).toEqual([{}]);
  });

  it("a 409 race is told in words, not as a raw error", async () => {
    mock([ROW]);
    postReply = () => jsonRes({ error: "x", code: "EMAIL_NOT_RESENDABLE" }, 409);
    await open();
    await screen.findByText(/Sẽ gửi tới/);
    await userEvent.click(screen.getByRole("button", { name: "Gửi lại" }));
    expect(await screen.findByText(/vừa được gửi lại/)).toBeInTheDocument();
  });

  it("a resent row reads «Đã gửi lại lúc HH:mm»", async () => {
    mock([{ ...ROW, canResend: false, resentAt: "2026-10-01T03:04:00Z" }]);
    render(<EmbeddedSurface><EmailLog initialStatus="failed" /></EmbeddedSurface>);
    const grid = await screen.findByRole("grid");
    expect(within(grid).getByText(/^Đã gửi lại lúc \d{2}:\d{2}$/)).toBeInTheDocument();
  });
});
