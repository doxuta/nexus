// specs/078 lát u7-cong-nhan-du-lieu-vao T131: Dữ liệu nhận vào.
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { listHookDeliveries, listInboundHooks } from "@/lib/api";
import { InboundDeliveriesPage } from "./InboundDeliveriesPage";

vi.mock("@/components/studio/useProjectQuery", () => ({
  useProjectQuery: (_s: string, key: string[], fn: () => Promise<unknown>) => useQuery({ queryKey: key, queryFn: fn, retry: false }),
}));
vi.mock("@/lib/api", async (o) => ({ ...(await o<typeof import("@/lib/api")>()), listInboundHooks: vi.fn(), listHookDeliveries: vi.fn() }));

const hook = (n: number) => ({ id: `h${n}`, name: `Cổng ${n}`, entityName: "crm_accounts", isActive: true });
const at = new Date().toISOString();
const wrap = (ui: ReactNode) => render(<QueryClientProvider client={new QueryClient()}>{ui}</QueryClientProvider>);

beforeEach(() => vi.clearAllMocks());

describe("InboundDeliveriesPage", () => {
  it("no hook: empty state with link to the build page", async () => {
    vi.mocked(listInboundHooks).mockResolvedValue([]);
    wrap(<InboundDeliveriesPage slug="northwind" />);
    expect(await screen.findByText("Chưa có cổng nhận dữ liệu")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Tạo cổng nhận dữ liệu/ })).toHaveAttribute("href", expect.stringContaining("dung/tu-dong/cong-nhan"));
  });

  it("reads at most 10 hooks and says so; payloadHead never reaches the DOM", async () => {
    vi.mocked(listInboundHooks).mockResolvedValue(Array.from({ length: 12 }, (_, i) => hook(i)) as never);
    vi.mocked(listHookDeliveries).mockResolvedValue([{ id: "d1", hookId: "h0", status: "created", payloadHead: "SECRET-PAYLOAD", createdAt: at }]);
    wrap(<InboundDeliveriesPage slug="northwind" />);
    expect(await screen.findByText("Hiện 10 cổng đầu; chọn cổng để xem riêng")).toBeInTheDocument();
    expect(listHookDeliveries).toHaveBeenCalledTimes(10);
    expect(document.body.textContent).not.toContain("SECRET-PAYLOAD");
  });

  it("?cong reads one hook only; three statuses; hook never hit says so", async () => {
    vi.mocked(listInboundHooks).mockResolvedValue([hook(1), hook(2)] as never);
    vi.mocked(listHookDeliveries).mockResolvedValue([
      { id: "a", hookId: "h1", status: "created", createdAt: at },
      { id: "b", hookId: "h1", status: "updated", createdAt: at },
      { id: "c", hookId: "h1", status: "error", error: "memo 'ABC' sai", createdAt: at },
    ]);
    wrap(<InboundDeliveriesPage slug="northwind" cong="h1" />);
    expect(await screen.findByText("Đã tạo")).toBeInTheDocument();
    expect(screen.getByText("Đã cập nhật")).toBeInTheDocument();
    expect(screen.getByText("Lỗi")).toBeInTheDocument();
    expect(listHookDeliveries).toHaveBeenCalledTimes(1);
    expect(document.body.textContent).not.toContain("ABC");
  });

  it("raw error only inside Chi tiết and only when the flag is on; rendered as text", async () => {
    vi.mocked(listInboundHooks).mockResolvedValue([hook(1)] as never);
    vi.mocked(listHookDeliveries).mockResolvedValue([{ id: "c", hookId: "h1", status: "error", error: "<img onerror=x>", createdAt: at }]);
    const { container, unmount } = wrap(<InboundDeliveriesPage slug="northwind" showRaw />);
    expect(await screen.findByText("Chi tiết")).toBeInTheDocument();
    expect(container.textContent).toContain("<img onerror=x>");
    expect(container.querySelector("img")).toBeNull();
    unmount();
    const off = wrap(<InboundDeliveriesPage slug="northwind" showRaw={false} />);
    expect(await screen.findByText("Lỗi")).toBeInTheDocument();
    expect(off.container.textContent).not.toContain("onerror");
  });

  it("hook with no deliveries", async () => {
    vi.mocked(listInboundHooks).mockResolvedValue([hook(1)] as never);
    vi.mocked(listHookDeliveries).mockResolvedValue([]);
    wrap(<InboundDeliveriesPage slug="northwind" />);
    expect(await screen.findByText("Cổng này chưa nhận lần nào.")).toBeInTheDocument();
  });

  it("read failure shows the error state", async () => {
    vi.mocked(listInboundHooks).mockRejectedValue(new Error("x"));
    wrap(<InboundDeliveriesPage slug="northwind" />);
    expect(await screen.findByRole("button", { name: "Thử lại" })).toBeInTheDocument();
  });
});
