// Embedded in the Studio frame (specs/078 lát u7-viec-nen-email, T115): the page gets its status as a prop (never reads
// window.location), the record link is the prop, and a server without mail says the mail was never sent.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
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
  resendOf: null, resentAt: null, createdBy: null, createdByName: null, canResend: false,
};

function mock(rows: EmailLogEntry[], mailConfigured = true) {
  const gets: string[] = [];
  global.fetch = vi.fn(async (url: RequestInfo | URL) => {
    const u = String(url);
    if (u.includes("/metadata/entities")) return jsonRes({ data: [{ name: "crm_contacts", label: "Liên hệ", tableName: "t" }] }, 200);
    if (u.includes("/admin/email-log")) {
      gets.push(u);
      return jsonRes({ data: rows, pagination: { limit: 50, nextCursor: null }, meta: { mailConfigured, queueEnabled: true } }, 200);
    }
    return jsonRes({ error: "unmocked", code: "TEST_UNMOCKED" }, 500);
  }) as unknown as typeof fetch;
  return gets;
}

describe("EmailLog embedded", () => {
  const orig = global.fetch;
  beforeEach(async () => {
    await logout();
    window.localStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "t");
    window.history.pushState({}, "", "/?status=sent");
  });
  afterEach(() => { global.fetch = orig; window.history.pushState({}, "", "/"); });

  it("takes initialStatus from the prop and ignores window.location", async () => {
    const gets = mock([ROW]);
    render(<EmbeddedSurface><EmailLog initialStatus="queued" /></EmbeddedSurface>);
    await screen.findByRole("grid");
    expect(new URL(gets[0]).searchParams.get("status")).toBe("queued");
  });

  it("links the record through recordHref, no hard /<entity>/<id>", async () => {
    mock([ROW]);
    render(<EmbeddedSurface><EmailLog initialStatus="failed" recordHref={(e, i) => `/studio/du-an/a/van-hanh/du-lieu/${e}/${i}`} /></EmbeddedSurface>);
    const link = await within(await screen.findByRole("grid")).findByRole("link", { name: "Liên hệ" });
    expect(link).toHaveAttribute("href", "/studio/du-an/a/van-hanh/du-lieu/crm_contacts/rec-1");
  });

  it("warns that the mail was never sent when the server has no mail configured", async () => {
    mock([ROW], false);
    render(<EmbeddedSurface><EmailLog initialStatus="failed" /></EmbeddedSurface>);
    expect(await screen.findByText("Máy chủ chưa cấu hình gửi thư, các email này chưa từng được gửi.")).toBeInTheDocument();
  });

  it("empty log says 'Chưa có email nào'", async () => {
    mock([]);
    render(<EmbeddedSurface><EmailLog initialStatus="" /></EmbeddedSurface>);
    expect(await screen.findByText("Chưa có email nào")).toBeInTheDocument();
  });
});
