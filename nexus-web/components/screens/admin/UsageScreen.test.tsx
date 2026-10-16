// Component tests — "Mức dùng" (specs/066 L4), on SLDS (specs/038). Through the real lib/admin → fetch wire:
//   * opens on the last 30 days: GET /admin/usage?from=<today-29>; picking 365 sends from = today-364;
//   * the four total cards each say how many failed; the per-day table lists the server's days newest first;
//   * the note says when the hourly roll-up last ran ("HH:mm"); a server with the roll-up off gets a warning;
//   * the admin crumb reads "Quản trị / Mức dùng" and the Setup menu lists the page for any customer admin.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { UsageScreen } from "@/components/screens/admin/UsageScreen";
import { adminCrumbs } from "@/components/shell/AppShell";
import { ADMIN_LINKS } from "@/components/slds/shell/adminLinks";
import { logout } from "@/lib/api";
import type { UsageCounts, UsageReport } from "@/lib/admin";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const ZERO: UsageCounts = {
  workflowRuns: 0, workflowRunsFailed: 0, emailsSent: 0, emailsFailed: 0,
  webhooksDelivered: 0, webhooksFailed: 0, hooksReceived: 0, hooksFailed: 0, usersSeen: 0,
};

function report(over: Partial<UsageReport> = {}): UsageReport {
  return {
    timezone: "Asia/Ho_Chi_Minh",
    from: "2026-10-13",
    to: "2026-10-14",
    rollupEnabled: true,
    lastRollupAt: "2026-10-14T03:30:00Z", // 10:30 in Vietnam
    days: [
      { ...ZERO, day: "2026-10-13", workflowRuns: 5, workflowRunsFailed: 1, emailsSent: 3, emailsFailed: 2 },
      { ...ZERO, day: "2026-10-14", workflowRuns: 7, webhooksDelivered: 4, webhooksFailed: 1, hooksReceived: 6, hooksFailed: 2 },
    ],
    totals: { ...ZERO, workflowRuns: 12, workflowRunsFailed: 1, emailsSent: 3, emailsFailed: 2, webhooksDelivered: 4, webhooksFailed: 1, hooksReceived: 6, hooksFailed: 2 },
    ...over,
  };
}

function routedFetch(gets: string[], rep: UsageReport) {
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    if (u.includes("/admin/usage") && (init?.method ?? "GET") === "GET") {
      gets.push(u);
      return jsonRes({ data: rep }, 200);
    }
    return jsonRes({ error: `unmocked ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
}

/** YYYY-MM-DD of (today in Vietnam) − back days, the way the screen computes `from`. */
function vnDay(back: number): string {
  const d = new Date(Date.now() + 7 * 3600_000);
  d.setUTCDate(d.getUTCDate() - back);
  return d.toISOString().slice(0, 10);
}

describe("UsageScreen", () => {
  let originalFetch: FetchFn;
  let gets: string[];

  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout();
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
    gets = [];
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("opens on 30 days and shows totals with their failures, the day table and when the roll-up last ran", async () => {
    global.fetch = routedFetch(gets, report()) as unknown as typeof fetch;
    render(<UsageScreen />);

    expect(await screen.findByTestId("total-page.usage.card.runs")).toHaveTextContent("12");
    expect(new URL(gets[0]).searchParams.get("from")).toBe(vnDay(29));
    expect(screen.getByTestId("total-page.usage.card.emails")).toHaveTextContent("5"); // 3 sent + 2 failed
    expect(screen.getByTestId("total-page.usage.card.webhooksOut")).toHaveTextContent("5");
    expect(screen.getByTestId("total-page.usage.card.webhooksIn")).toHaveTextContent("8");
    expect(screen.getAllByText("1 lỗi").length).toBeGreaterThan(0);
    expect(screen.getAllByText("2 lỗi").length).toBe(2); // emails + inbound hooks
    expect(screen.getByText("14/10/2026")).toBeInTheDocument();
    expect(screen.getByText("13/10/2026")).toBeInTheDocument();
    expect(screen.getByText(/lần cuối 10:30/)).toBeInTheDocument();
    expect(screen.queryByText(/tắt việc tổng hợp theo giờ/)).not.toBeInTheDocument();
  });

  it("shows the Hiện tại block with business labels, a readable size and a per-day sign-in column", async () => {
    const rep = report({
      days: [{ ...ZERO, day: "2026-10-14", usersSeen: 3 }],
      current: {
        usersActive: 4, usersSeen30d: 6, recordsTotal: 1500, fileBytes: 3 * 1024 * 1024, fileCount: 2,
        records: [{ entity: "crm_contacts", label: "Liên hệ", count: 1200 }, { entity: "crm_leads", label: "Khách tiềm năng", count: 300 }],
        computedAt: "2026-10-14T03:30:00Z",
      },
    });
    global.fetch = routedFetch(gets, rep) as unknown as typeof fetch;
    render(<UsageScreen />);

    expect(await screen.findByTestId("current-usersActive")).toHaveTextContent("4");
    expect(screen.getByTestId("current-usersSeen30d")).toHaveTextContent("6");
    expect(screen.getByTestId("current-records")).toHaveTextContent("1.500");
    expect(screen.getByTestId("current-files")).toHaveTextContent("3,0 MB");
    expect(screen.getByText("2 tệp, kể cả tệp trong thùng rác")).toBeInTheDocument();
    expect(screen.getByText("Liên hệ")).toBeInTheDocument();
    expect(screen.getByText("Khách tiềm năng")).toBeInTheDocument();
    expect(screen.queryByText("crm_contacts")).not.toBeInTheDocument();
    expect(screen.getByText("Người đăng nhập")).toBeInTheDocument();
  });

  it("365 days asks from = today - 364", async () => {
    global.fetch = routedFetch(gets, report()) as unknown as typeof fetch;
    render(<UsageScreen />);
    await screen.findByTestId("total-page.usage.card.runs");

    fireEvent.click(screen.getByRole("combobox", { name: "Khoảng thời gian" }));
    fireEvent.click(await screen.findByRole("option", { name: "365 ngày qua" }));

    await waitFor(() => expect(gets.length).toBe(2));
    expect(new URL(gets[1]).searchParams.get("from")).toBe(vnDay(364));
  });

  it("warns when the server's roll-up is off", async () => {
    global.fetch = routedFetch(gets, report({ rollupEnabled: false, lastRollupAt: null })) as unknown as typeof fetch;
    render(<UsageScreen />);
    expect(await screen.findByText(/tắt việc tổng hợp theo giờ/)).toBeInTheDocument();
    expect(screen.getByText(/chưa có lần nào/)).toBeInTheDocument();
  });

  it("says so when the figures cannot be read", async () => {
    global.fetch = vi.fn(async () => jsonRes({ error: "Không đọc được mức dùng", code: "USAGE_QUERY_FAILED" }, 500)) as unknown as typeof fetch;
    render(<UsageScreen />);
    expect(await screen.findByText("Không đọc được mức dùng")).toBeInTheDocument();
  });

  it("has the crumb 'Quản trị / Mức dùng' and is in the Setup menu for every customer admin", () => {
    expect(adminCrumbs(["admin", "usage"]).map((c) => c.label)).toEqual(["Quản trị", "Mức dùng"]);
    const link = ADMIN_LINKS.find((l) => l.href === "/admin/usage");
    expect(link).toBeDefined();
    expect(link?.operatorOnly).toBeFalsy();
  });
});
