import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RecordSlaCard } from "./RecordSlaCard";
import { listRecordSLA } from "@/lib/api";

vi.mock("@/lib/api", () => ({ ensureAuth: vi.fn(), listRecordSLA: vi.fn() }));

const wrap = () => render(
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <RecordSlaCard entity="tickets" recordId="r1" />
  </QueryClientProvider>,
);
const sla = (o: object) => ({ dueAt: "2999-01-01T00:00:00Z", basis: "business", paused: false, met: false, breached: false, ...o });

describe("RecordSlaCard", () => {
  it("renders nothing when the record has no tracking item", async () => {
    vi.mocked(listRecordSLA).mockResolvedValue([]);
    const { container } = wrap();
    await new Promise((r) => setTimeout(r, 20));
    expect(container.textContent).toBe("");
  });
  it("shows running, paused and met clocks in customer words", async () => {
    vi.mocked(listRecordSLA).mockResolvedValue([
      { id: "a", stepName: "Phản hồi đầu tiên", status: "waiting", sla: sla({}) },
      { id: "b", stepName: "Xử lý", status: "waiting", sla: sla({ paused: true, dueAt: null, remainingSeconds: 7200 }) },
      { id: "c", stepName: "Đóng", status: "completed", sla: sla({ met: true }) },
    ]);
    wrap();
    expect(await screen.findByText("Còn hạn")).toBeTruthy();
    expect(screen.getByText("Tạm dừng")).toBeTruthy();
    expect(screen.getByText("Đạt — giờ làm việc")).toBeTruthy();
    expect(screen.getByText(/còn khoảng 2 giờ/)).toBeTruthy();
    expect(screen.getByText(/tính theo giờ làm việc/)).toBeTruthy();
  });
});
