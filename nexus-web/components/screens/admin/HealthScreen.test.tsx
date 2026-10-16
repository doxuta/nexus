// Component tests — "Sức khoẻ" (specs/066 L6), through the real lib/admin → fetch wire.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { HealthScreen } from "@/components/screens/admin/HealthScreen";
import { ADMIN_LINKS } from "@/components/slds/shell/adminLinks";
import { logout } from "@/lib/api";
import type { TenantHealth } from "@/lib/admin";

let operator = false;
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { isPlatformOperator: operator } }) }));

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

const jsonRes = (body: unknown, status: number) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function health(over: Partial<TenantHealth> = {}): TenantHealth {
  return {
    status: "warn",
    generatedAt: "2026-10-14T03:30:00Z",
    queueEnabled: true,
    jobs: { failed7d: [{ jobType: "outbound_webhook", count: 1, lastAt: "2026-10-14T03:00:00Z" }], stuck: 0 },
    workflowRuns: {
      failed24h: 2, failed7d: 3,
      topFailing: [{ workflowId: "w1", name: "Gửi email chào", failed: 2, lastAt: "2026-10-14T03:00:00Z", lastError: "x" }],
    },
    emails: { failed24h: 1, failed7d: 2, unresentFailed: 2 },
    inboundHooks: { failed24h: 4, byHook: [{ hookId: "h1", name: "Cổng thanh toán", failed: 4, lastAt: "2026-10-14T03:00:00Z" }] },
    doctor: { status: "unknown", critical: 0, warn: 0, checkedAt: null },
    ...over,
  };
}

describe("HealthScreen", () => {
  let originalFetch: FetchFn;
  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout();
    window.localStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
    operator = false;
  });
  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
  });

  function serve(h: TenantHealth) {
    global.fetch = vi.fn(async (url: RequestInfo | URL) =>
      String(url).includes("/admin/health") ? jsonRes({ data: h }, 200) : jsonRes({ error: "unmocked", code: "X" }, 500)) as unknown as typeof fetch;
  }

  it("shows the overall badge, each signal with names only, and the two links", async () => {
    serve(health());
    render(<HealthScreen />);
    expect(await screen.findByText("Cần chú ý")).toBeInTheDocument();
    expect(screen.getByText("Giao webhook: lỗi 1")).toBeInTheDocument();
    expect(screen.getByText("Gửi email chào: lỗi 2")).toBeInTheDocument();
    expect(screen.getByText("Cổng thanh toán: lỗi 4")).toBeInTheDocument();
    expect(screen.getByText("2 email chưa gửi lại")).toBeInTheDocument();
    expect(screen.getByText("Chưa khám")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Mở Việc nền" })).toHaveAttribute("href", "/admin/jobs");
    expect(screen.getByRole("link", { name: "Xem email lỗi" })).toHaveAttribute("href", "/admin/email-log?status=failed");
    expect(screen.queryByRole("link", { name: "Mở Bác sĩ hệ thống" })).not.toBeInTheDocument();
  });

  it("shows the Doctor link only to an operator", async () => {
    operator = true;
    serve(health({ status: "critical", jobs: { failed7d: [], stuck: 3 } }));
    render(<HealthScreen />);
    expect(await screen.findByText("Nghiêm trọng")).toBeInTheDocument();
    expect(screen.getByTestId("stuck")).toHaveTextContent("3 việc");
    expect(screen.getByRole("link", { name: "Mở Bác sĩ hệ thống" })).toHaveAttribute("href", "/admin/doctor");
  });

  it("says so when the signals cannot be read, and lists the page in Setup for any admin", async () => {
    global.fetch = vi.fn(async () => jsonRes({ error: "Không đọc được sức khoẻ vận hành", code: "HEALTH_QUERY_FAILED" }, 500)) as unknown as typeof fetch;
    render(<HealthScreen />);
    expect(await screen.findByText("Không đọc được sức khoẻ vận hành")).toBeInTheDocument();
    expect(ADMIN_LINKS.find((l) => l.href === "/admin/health")?.operatorOnly).toBeFalsy();
  });
});
