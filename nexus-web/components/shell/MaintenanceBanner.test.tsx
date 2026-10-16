// MaintenanceBanner — specs/068 lát B (T041): the strip appears when req() raises
// `nexus:tenant-maintenance`, names the deadline, hides itself when the deadline passes; and req()
// raises the event for 503 TENANT_MAINTENANCE only.
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, act } from "@testing-library/react";
import { MaintenanceBanner } from "./MaintenanceBanner";
import { TENANT_MAINTENANCE_EVENT, apiRequest } from "@/lib/api";

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

const raise = (until: string | null) =>
  act(() => { window.dispatchEvent(new CustomEvent(TENANT_MAINTENANCE_EVENT, { detail: { until } })); });

describe("MaintenanceBanner", () => {
  it("renders nothing until a write is refused", () => {
    const { container } = render(<MaintenanceBanner />);
    expect(container).toBeEmptyDOMElement();
  });

  it("shows an SLDS warning alert naming the deadline, then hides itself when it passes", () => {
    vi.useFakeTimers();
    render(<MaintenanceBanner />);
    raise(new Date(Date.now() + 60_000).toISOString());
    const alert = screen.getByRole("alert");
    expect(alert).toHaveClass("slds-alert_warning");
    expect(alert).toHaveTextContent(/đang được bảo trì/);
    act(() => { vi.advanceTimersByTime(61_000); });
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("falls back to a time-less sentence when the server named no deadline", () => {
    render(<MaintenanceBanner />);
    raise(null);
    expect(screen.getByRole("alert")).toHaveTextContent(/thử lại sau ít phút/);
  });
});

describe("req() maintenance event", () => {
  const respond = (status: number, body: unknown) =>
    vi.spyOn(global, "fetch").mockResolvedValue(new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } }));

  it("fires for 503 TENANT_MAINTENANCE with the deadline", async () => {
    respond(503, { error: "Dữ liệu đang được bảo trì", code: "TENANT_MAINTENANCE", details: { until: "2026-10-14T10:05:00Z" } });
    const seen: Array<string | null> = [];
    const on = (e: Event) => seen.push((e as CustomEvent<{ until: string | null }>).detail.until);
    window.addEventListener(TENANT_MAINTENANCE_EVENT, on);
    await expect(apiRequest("POST", "/contacts", {})).rejects.toMatchObject({ status: 503, code: "TENANT_MAINTENANCE" });
    window.removeEventListener(TENANT_MAINTENANCE_EVENT, on);
    expect(seen).toEqual(["2026-10-14T10:05:00Z"]);
  });

  it("does NOT fire for any other 503", async () => {
    respond(503, { error: "Hệ thống bận", code: "RECORD_HOOKS_UNAVAILABLE" });
    const on = vi.fn();
    window.addEventListener(TENANT_MAINTENANCE_EVENT, on);
    await expect(apiRequest("POST", "/contacts", {})).rejects.toMatchObject({ status: 503 });
    window.removeEventListener(TENANT_MAINTENANCE_EVENT, on);
    expect(on).not.toHaveBeenCalled();
  });
});
