// specs/078 lát u7-suc-khoe T163: the four states of Sức khoẻ, the "not built" case and the token-scoped read.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { useProjectQuery } from "@/components/studio/useProjectQuery";
import { ApiError } from "@/lib/api";
import { getTenantHealth, type TenantHealth } from "@/lib/admin";
import { HealthPage } from "./HealthPage";

vi.mock("./UsageSection", () => ({ UsageSection: () => null }));
vi.mock("@/components/studio/useProjectQuery", () => ({ useProjectQuery: vi.fn() }));

const health = (over: Partial<TenantHealth> = {}): TenantHealth => ({
  status: "ok",
  generatedAt: "2026-10-14T10:00:00Z",
  queueEnabled: true,
  jobs: { failed7d: [], stuck: 0 },
  workflowRuns: { failed24h: 0, failed7d: 0, topFailing: [] },
  emails: { failed24h: 0, failed7d: 0, unresentFailed: 0 },
  inboundHooks: { failed24h: 0, byHook: [] },
  doctor: { status: "ok", critical: 0, warn: 0, checkedAt: "2026-10-14T08:00:00Z" },
  ...over,
});
const refetch = vi.fn();
const serve = (q: object) => vi.mocked(useProjectQuery).mockReturnValue({ isPending: false, isError: false, error: null, data: undefined, refetch, ...q } as never);

beforeEach(() => {
  vi.clearAllMocks();
});

describe("HealthPage", () => {
  it("loading: a spinner, no sentence", () => {
    serve({ isPending: true });
    render(<HealthPage slug="northwind" />);
    expect(screen.queryByText(/Không có gì cần anh xử lý/)).toBeNull();
    expect(screen.queryByText(/không lỗi/)).toBeNull();
  });
  it("clean: one line and no list, no KPI row, no overall badge", () => {
    serve({ data: health() });
    render(<HealthPage slug="northwind" />);
    expect(screen.getByText("không lỗi")).toBeInTheDocument();
    expect(screen.getByText("Không có gì cần anh xử lý.")).toBeInTheDocument();
    expect(screen.queryByRole("list")).toBeNull();
    expect(document.querySelector(".slds-card, .slds-badge")).toBeNull();
  });
  it("one failing rule and one failed mail: lede counts them, each row has one button that opens the filtered page", () => {
    serve({
      data: health({
        workflowRuns: { failed24h: 1, failed7d: 1, topFailing: [{ workflowId: "w1", name: "Gửi đơn", failed: 1, lastAt: "2026-10-14T09:00:00Z", lastError: "x" }] },
        emails: { failed24h: 1, failed7d: 1, unresentFailed: 1 },
      }),
    });
    render(<HealthPage slug="northwind" />);
    expect(screen.getByText("2 việc lỗi")).toBeInTheDocument();
    expect(screen.queryByText("không lỗi")).toBeNull();
    expect(screen.getAllByRole("listitem")).toHaveLength(2);
    expect(screen.getByRole("link", { name: "Xem email lỗi" })).toHaveAttribute("href", "/studio/du-an/northwind/van-hanh/tu-dong/email?trang-thai=failed");
    expect(screen.getByRole("link", { name: "Mở nhật ký" })).toHaveAttribute("href", expect.stringContaining("luat=w1"));
  });
  it("a block missing: no 'không lỗi', no 'all good', an unknown row with Thử lại that refetches", async () => {
    const h = health() as unknown as Record<string, unknown>;
    delete h.emails;
    serve({ data: h });
    render(<HealthPage slug="northwind" />);
    expect(screen.queryByText("không lỗi")).toBeNull();
    expect(screen.queryByText("Không có gì cần anh xử lý.")).toBeNull();
    expect(screen.getByText(/Chưa đọc được email/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Thử lại" }));
    expect(refetch).toHaveBeenCalledTimes(1);
  });
  it("read failed: one sentence and Thử lại, never a clean line", async () => {
    serve({ isError: true, error: new ApiError(500, "INTERNAL", "boom") });
    render(<HealthPage slug="northwind" />);
    expect(screen.getByText("Không đọc được số liệu sức khoẻ.")).toBeInTheDocument();
    expect(screen.queryByText(/Không có gì cần/)).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Thử lại" }));
    expect(refetch).toHaveBeenCalled();
  });
  it("#414 not in this build (404): the honest pending sentence", () => {
    serve({ isError: true, error: new ApiError(404, "NOT_FOUND", "no route") });
    render(<HealthPage slug="northwind" />);
    expect(screen.getByText("Số liệu sức khoẻ chưa có trong bản Nexus này.")).toBeInTheDocument();
  });
  it("reads only through useProjectQuery, refetches on focus, never polls", () => {
    serve({ data: health() });
    render(<HealthPage slug="northwind" />);
    const [slug, key, fetcher, opts] = vi.mocked(useProjectQuery).mock.calls[0] as unknown as [string, string[], unknown, Record<string, unknown>];
    expect(slug).toBe("northwind");
    expect(key).toEqual(["operate", "health"]);
    expect(fetcher).toBe(getTenantHealth);
    expect(opts.refetchOnWindowFocus).toBe(true);
    expect(opts).not.toHaveProperty("refetchInterval");
  });
});
