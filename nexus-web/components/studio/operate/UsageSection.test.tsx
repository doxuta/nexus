// specs/078 lát u7-muc-dung-bieu-do T171: Sức khoẻ & mức dùng — the daily-usage half (lede + one chart + range + measure).
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { useProjectQuery } from "@/components/studio/useProjectQuery";
import { getUsage, type UsageReport } from "@/lib/admin";
import { ApiError } from "@/lib/api";
import { UsageSection } from "./UsageSection";

const replace = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ replace, push: vi.fn() }) }));
vi.mock("@/components/studio/useProjectQuery", () => ({ useProjectQuery: vi.fn() }));
vi.mock("@/lib/admin", async (orig) => ({ ...(await orig<typeof import("@/lib/admin")>()), getUsage: vi.fn() }));

const zero = { workflowRuns: 0, workflowRunsFailed: 0, emailsSent: 0, emailsFailed: 0, webhooksDelivered: 0, webhooksFailed: 0, hooksReceived: 0, hooksFailed: 0, usersSeen: 0 };
const REPORT: UsageReport = {
  timezone: "Asia/Ho_Chi_Minh",
  from: "2026-10-12",
  to: "2026-10-14",
  rollupEnabled: true,
  lastRollupAt: "2026-10-14T03:05:00Z",
  days: [
    { day: "2026-10-12", ...zero, usersSeen: 2, workflowRuns: 4 },
    { day: "2026-10-13", ...zero },
    { day: "2026-10-14", ...zero, usersSeen: 3, workflowRuns: 6, workflowRunsFailed: 1 },
  ],
  totals: zero,
  current: { usersActive: 5, usersSeen30d: 4, recordsTotal: 1200, records: [], fileBytes: 1.5 * 1024 ** 3, fileCount: 7, computedAt: "2026-10-14T03:05:00Z" },
};
const state = (o: object) => vi.mocked(useProjectQuery).mockReturnValue({ isPending: false, isError: false, error: null, data: REPORT, refetch: vi.fn(), ...o } as never);

beforeEach(() => {
  vi.clearAllMocks();
  vi.useRealTimers();
  state({});
});

describe("UsageSection", () => {
  it("lede: seen/active people and size, no '/ 10 GB' denominator", () => {
    render(<UsageSection slug="northwind" />);
    const lede = document.querySelector(".nx-lede") as HTMLElement;
    expect(lede).toHaveTextContent("4/5 người đăng nhập trong 30 ngày qua");
    expect(lede).toHaveTextContent("1.200 bản ghi");
    expect(lede).toHaveTextContent("1,50 GB");
    expect(lede.textContent).not.toMatch(/\/\s*10/);
  });
  it("title follows the measure and the range; default 30 days and 'Người đăng nhập mỗi ngày'", async () => {
    render(<UsageSection slug="northwind" />);
    expect(screen.getByRole("heading", { name: "Người đăng nhập mỗi ngày, 30 ngày qua" })).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByLabelText("Đại lượng"), "runs");
    expect(screen.getByRole("heading", { name: "Lần chạy luật, 30 ngày qua" })).toBeInTheDocument();
    expect(screen.getByRole("list", { name: "Chú giải" })).toHaveTextContent("Lỗi");
  });
  it("the measure is a Select with the five measures", () => {
    render(<UsageSection slug="northwind" />);
    const sel = screen.getByLabelText("Đại lượng") as HTMLSelectElement;
    expect(sel.tagName).toBe("SELECT");
    expect(within(sel).getAllByRole("option").map((o) => o.textContent)).toEqual(["Người đăng nhập mỗi ngày", "Lần chạy luật", "Email", "Dữ liệu gửi đi", "Dữ liệu nhận vào"]);
  });
  it("?khoang picks the window, absent or junk = 30; changing it writes the URL", async () => {
    const { unmount } = render(<UsageSection slug="northwind" khoang="7" />);
    expect(screen.getByRole("heading", { name: /7 ngày qua/ })).toBeInTheDocument();
    unmount();
    render(<UsageSection slug="northwind" khoang="999" />);
    expect(screen.getByRole("heading", { name: /30 ngày qua/ })).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByLabelText("Khoảng"), "90");
    expect(replace).toHaveBeenCalledWith("/studio/du-an/northwind/van-hanh/suc-khoe?khoang=90");
  });
  it("365 days asks from = today-364 in the SERVER zone, cached 60 s under ['operate','usage',from,to]", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-10-14T17:00:00Z")); // already 15/10 in Asia/Ho_Chi_Minh
    render(<UsageSection slug="northwind" khoang="365" />);
    const [slug, key, fetcher, opts] = vi.mocked(useProjectQuery).mock.calls[0];
    expect(slug).toBe("northwind");
    expect(key).toEqual(["operate", "usage", "2025-10-16", "2026-10-15"]);
    expect(opts).toMatchObject({ staleTime: 60_000 });
    vi.mocked(getUsage).mockResolvedValue(REPORT);
    await fetcher();
    expect(getUsage).toHaveBeenCalledWith("2025-10-16");
  });
  it("rollup off: a warning; note always shows the roll-up time (lastRollupAt), null drops the clause", () => {
    state({ data: { ...REPORT, rollupEnabled: false } });
    const { unmount } = render(<UsageSection slug="northwind" />);
    expect(screen.getByText(/Số liệu theo ngày đang tắt trên máy chủ này/)).toBeInTheDocument();
    expect(screen.getByText(/Hôm nay tính trực tiếp, ngày trước cập nhật mỗi giờ \(lần cuối \d{2}:\d{2}\)/)).toBeInTheDocument();
    unmount();
    state({ data: { ...REPORT, lastRollupAt: null } });
    render(<UsageSection slug="northwind" />);
    expect(screen.getByText("Hôm nay tính trực tiếp, ngày trước cập nhật mỗi giờ.")).toBeInTheDocument();
  });
  it("#412 not on the server (404): the honest PendingPage, no chart", () => {
    state({ data: undefined, isError: true, error: new ApiError(404, "NOT_FOUND", "404 page not found") });
    render(<UsageSection slug="northwind" />);
    expect(screen.getByText(/chưa có trong bản Nexus này/)).toBeInTheDocument();
    expect(screen.queryByRole("group", { name: /Biểu đồ/ })).toBeNull();
  });
});
