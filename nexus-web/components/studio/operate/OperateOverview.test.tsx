// specs/078 lát u7-tu-dong-tong-quan T104: the overview of Việc tự động & kết nối.
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, retryBackgroundJob } from "@/lib/admin";
import { useProjectQuery } from "@/components/studio/useProjectQuery";
import { OperateOverview } from "./OperateOverview";

vi.mock("@/components/studio/useProjectQuery", () => ({ useProjectQuery: vi.fn() }));
vi.mock("@tanstack/react-query", async (o) => ({ ...(await o<typeof import("@tanstack/react-query")>()), useQueryClient: () => ({ invalidateQueries: vi.fn() }) }));
const gate = { assertScope: vi.fn(() => true), signal: undefined as AbortSignal | undefined };
vi.mock("./useScopeGate", () => ({ useScopeGate: () => gate }));
vi.mock("@/lib/admin", async (o) => ({ ...(await o<typeof import("@/lib/admin")>()), retryBackgroundJob: vi.fn() }));

const iso = (h: number) => new Date(Date.now() - h * 3_600_000).toISOString();
const job = (id: string, h: number, over = {}) => ({ id, jobType: "outbound_webhook", status: "failed", attempts: 3, maxAttempts: 3, lastError: "HTTP 500", workflowId: "w1", runAt: iso(h), createdAt: iso(h), finishedAt: iso(h), ...over });
const ok = (data: unknown) => ({ isPending: false, isError: false, error: null, data, refetch: vi.fn() });
const bad = { isPending: false, isError: true, error: new Error("x"), data: undefined, refetch: vi.fn() };

function serve(by: Record<string, unknown>) {
  vi.mocked(useProjectQuery).mockImplementation(((_s: string, key: string[]) => by[key.join("/")] ?? ok([])) as never);
}
const BASE = {
  "operate/rules": ok([{ id: "w1", name: "Gửi đơn", entityName: "crm_accounts" }]),
  "operate/hooks": ok([{ id: "h1", name: "Cổng thanh toán", entityName: "crm_accounts", isActive: true, secret: "SHHH" }]),
  "operate/apikeys": ok([]),
  "operate/runs/recent": ok([]),
  "operate/email/failed": ok([]),
};

beforeEach(() => {
  vi.clearAllMocks();
  gate.assertScope.mockReturnValue(true);
});

describe("OperateOverview", () => {
  it("clean: all three sources read and empty says Không lỗi and the empty-feed sentence", () => {
    serve({ ...BASE, "operate/jobs/failed": ok([]) });
    render(<OperateOverview slug="northwind" />);
    expect(screen.getByText("Không lỗi trong 24 giờ qua.")).toBeInTheDocument();
    expect(screen.getByText("24 giờ qua chưa có việc tự động nào chạy.")).toBeInTheDocument();
  });
  it("one source unreadable: never says Không lỗi, names the source (FR-071)", () => {
    serve({ ...BASE, "operate/jobs/failed": bad });
    render(<OperateOverview slug="northwind" />);
    expect(screen.queryByText(/Không lỗi/)).toBeNull();
    expect(screen.getByText(/Chưa đọc được việc chạy nền/)).toBeInTheDocument();
  });
  it("#414: GET /admin/health read in full replaces the windows: its counts, no 'từ N trở lên', jobs as 7 days (T165)", () => {
    const h = {
      status: "warn", generatedAt: iso(0), queueEnabled: true,
      jobs: { failed7d: [{ jobType: "outbound_webhook", count: 3, lastAt: iso(30) }], stuck: 0 },
      workflowRuns: { failed24h: 2, failed7d: 2, topFailing: [] },
      emails: { failed24h: 1, failed7d: 1, unresentFailed: 1 },
      inboundHooks: { failed24h: 0, byHook: [] },
      doctor: { status: "ok", critical: 0, warn: 0, checkedAt: iso(1) },
    };
    serve({ ...BASE, "operate/jobs/failed": ok([]), "operate/health": ok(h) });
    render(<OperateOverview slug="northwind" />);
    expect(screen.getByText(/3 lỗi trong 24 giờ qua/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "2 lần chạy luật" })).toHaveAttribute("href", "/studio/du-an/northwind/van-hanh/tu-dong/lan-chay");
    expect(screen.getByRole("link", { name: "1 email" })).toHaveAttribute("href", expect.stringContaining("trang-thai=failed"));
    expect(screen.getByRole("link", { name: "3 việc chạy nền lỗi" })).toBeInTheDocument();
    expect(screen.queryByText(/từ \d+ trở lên/)).toBeNull();
  });
  it("#414: health clean for rules/email/hooks but jobs failed in 7 days never claims plain Không lỗi (T110 finding)", () => {
    const h = {
      status: "warn", generatedAt: iso(0), queueEnabled: true,
      jobs: { failed7d: [{ jobType: "outbound_webhook", count: 6, lastAt: iso(1) }], stuck: 0 },
      workflowRuns: { failed24h: 0, failed7d: 0, topFailing: [] },
      emails: { failed24h: 0, failed7d: 0, unresentFailed: 0 },
      inboundHooks: { failed24h: 0, byHook: [] },
      doctor: { status: "ok", critical: 0, warn: 0, checkedAt: iso(1) },
    };
    serve({ ...BASE, "operate/jobs/failed": ok([job("j1", 1)]), "operate/health": ok(h) });
    render(<OperateOverview slug="northwind" />);
    expect(screen.queryByText("Không lỗi trong 24 giờ qua.")).toBeNull();
    expect(screen.getByText(/Không lần chạy luật, email hay dữ liệu nhận vào nào lỗi/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "6 việc chạy nền lỗi" })).toBeInTheDocument();
  });
  it("#414: a health block missing falls back to the windows, still fail-closed", () => {
    serve({ ...BASE, "operate/jobs/failed": bad, "operate/health": ok({ jobs: { failed7d: [], stuck: 0 } }) });
    render(<OperateOverview slug="northwind" />);
    expect(screen.queryByText(/Không lỗi/)).toBeNull();
    expect(screen.getByText(/Chưa đọc được việc chạy nền/)).toBeInTheDocument();
  });
  it("each number in the lede links to its sub page; connections show no secret and one Mở link", () => {
    serve({ ...BASE, "operate/jobs/failed": ok([job("j1", 1)]) });
    render(<OperateOverview slug="northwind" />);
    expect(screen.getByRole("link", { name: "1 việc chạy nền" })).toHaveAttribute("href", "/studio/du-an/northwind/van-hanh/tu-dong/viec-nen");
    expect(screen.getByRole("heading", { name: "Nhật ký 24 giờ qua" })).toBeInTheDocument();
    expect(document.body.textContent).not.toContain("SHHH");
    expect(screen.getAllByRole("link", { name: "Mở Cổng thanh toán" })).toHaveLength(1);
    expect(screen.queryByText("Đổi khoá")).toBeNull();
  });
  it("connection keys join the list: name, last change, status, one Mở link to the vault, no secret (u7-khoa-ket-noi)", () => {
    const cred = (id: string, over = {}) => ({ id, name: id, label: "Cổng giao hàng", baseUrl: "https://ship.example.com", authType: "hmac", status: "ready", secretHint: "ab12", rotatedAt: "2026-10-01T09:30:00Z", usageCount: 1, secret: "LEAK-ME", ...over });
    serve({ ...BASE, "operate/jobs/failed": ok([]), "operate/credentials": ok([cred("c1"), cred("c2", { label: "Kho hàng", status: "needs_secret", rotatedAt: null })]) });
    render(<OperateOverview slug="northwind" />);
    const open = screen.getByRole("link", { name: "Mở Cổng giao hàng" });
    expect(open).toHaveAttribute("href", "/studio/du-an/northwind/van-hanh/tu-dong/khoa-ket-noi");
    expect(screen.getByText(/Khoá kết nối · đổi lần cuối/)).toBeInTheDocument();
    expect(screen.getByText("Khoá kết nối · chưa đổi lần nào")).toBeInTheDocument();
    expect(screen.getByText("Chờ nhập khoá")).toBeInTheDocument();
    expect(document.body.textContent).not.toContain("LEAK-ME");
    expect(document.body.textContent).not.toContain("ab12");
  });
  it("keys unreadable (non-503) keeps the hook rows and says the list is partial, not a whole-section error", () => {
    serve({ ...BASE, "operate/jobs/failed": ok([]), "operate/credentials": bad });
    render(<OperateOverview slug="northwind" />);
    expect(screen.getByRole("link", { name: "Mở Cổng thanh toán" })).toBeInTheDocument();
    expect(screen.getByText(/Một phần danh sách kết nối chưa tải được/)).toBeInTheDocument();
  });
  it("Gửi lại N: confirm first, then calls one by one", async () => {
    serve({ ...BASE, "operate/jobs/failed": ok([job("j1", 1), job("j2", 2)]) });
    vi.mocked(retryBackgroundJob).mockResolvedValue({} as never);
    render(<OperateOverview slug="northwind" />);
    await userEvent.click(screen.getByRole("button", { name: "Gửi lại 2 việc lỗi" }));
    expect(retryBackgroundJob).not.toHaveBeenCalled();
    expect(screen.getByText(/Gửi lại 2 việc tới hệ thống bên ngoài\? Cùng mã giao nhận/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Gửi lại" }));
    await waitFor(() => expect(screen.getByText("Đã gửi lại 2 trên 2, 0 lỗi.")).toBeInTheDocument());
    expect(vi.mocked(retryBackgroundJob).mock.calls.map((c) => c[0])).toEqual(["j1", "j2"]);
  });
  it("scope changes after call 2: call 3 is never sent (FR-131)", async () => {
    serve({ ...BASE, "operate/jobs/failed": ok([job("j1", 1), job("j2", 2), job("j3", 3)]) });
    vi.mocked(retryBackgroundJob).mockResolvedValue({} as never);
    gate.assertScope.mockReturnValueOnce(true).mockReturnValueOnce(true).mockReturnValue(false);
    render(<OperateOverview slug="northwind" />);
    await userEvent.click(screen.getByRole("button", { name: "Gửi lại 3 việc lỗi" }));
    await userEvent.click(screen.getByRole("button", { name: "Gửi lại" }));
    await waitFor(() => expect(screen.getByText(/Phiên dựng đã đổi nên dừng lại. Đã làm xong 2 trên 3/)).toBeInTheDocument());
    expect(retryBackgroundJob).toHaveBeenCalledTimes(2);
  });
  it("429 stops the loop; 409 counts as not resendable and the loop goes on", async () => {
    serve({ ...BASE, "operate/jobs/failed": ok([job("j1", 1), job("j2", 2), job("j3", 3)]) });
    vi.mocked(retryBackgroundJob)
      .mockRejectedValueOnce(new ApiError(409, "JOB_NOT_RETRYABLE", "x"))
      .mockRejectedValueOnce(new ApiError(429, "RATE_LIMITED", "x"));
    render(<OperateOverview slug="northwind" />);
    await userEvent.click(screen.getByRole("button", { name: "Gửi lại 3 việc lỗi" }));
    await userEvent.click(screen.getByRole("button", { name: "Gửi lại" }));
    await waitFor(() => expect(screen.getByText(/gửi quá nhanh nên dừng lại. Đã làm xong 0 trên 3/)).toBeInTheDocument());
    expect(retryBackgroundJob).toHaveBeenCalledTimes(2);
  });
  it("row button: light confirm under the table, then one POST", async () => {
    serve({ ...BASE, "operate/jobs/failed": ok([job("j1", 1)]) });
    vi.mocked(retryBackgroundJob).mockResolvedValue({} as never);
    render(<OperateOverview slug="northwind" />);
    const row = screen.getByText(/Gửi dữ liệu · Gửi đơn/).closest("tr") as HTMLElement;
    await userEvent.click(within(row).getByRole("button", { name: "Gửi lại việc Gửi đơn" }));
    expect(retryBackgroundJob).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Gửi lại" }));
    await waitFor(() => expect(retryBackgroundJob).toHaveBeenCalledWith("j1"));
  });
});
