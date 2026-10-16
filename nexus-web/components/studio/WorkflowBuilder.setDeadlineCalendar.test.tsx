// Queue #79 (specs/037 §2.4): a set_deadline action may pin its own working calendar — "phiếu Khẩn:
// 12 giờ làm việc theo lịch Trực 24/7" in an office-hours organisation. The builder must SHOW the
// pin (a hidden one makes "12 giờ làm việc → 16:34 thứ Tư" read like an arithmetic bug), let the
// admin change or remove it, and — when the pinned calendar was deleted — show the dead pin and pin
// the save's 422 onto that dropdown with a way out, instead of a rule nobody can save any more.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { WorkflowBuilder } from "./WorkflowBuilder";
import { studioHref } from "@/lib/studioRoutes";
import { ApiError, listWorkflows, updateWorkflow } from "@/lib/api";
import { touchRule } from "@/test/touchRule";

const rule = (calendarId?: string) => ({
  id: "rule-79",
  name: "Phiếu Khẩn — 12 giờ làm việc",
  entityName: "tk_tickets",
  trigger: "after_create",
  isActive: true,
  condition: null,
  actions: [{
    type: "set_deadline", field: "due_date", offset: { amount: 12, unit: "business_hours" },
    ...(calendarId ? { calendarId } : {}),
  }],
});

const cal = (id: string, name: string, isDefault: boolean) =>
  ({ id, tenantId: "t1", name, timezone: "Asia/Ho_Chi_Minh", hours: {}, holidays: [], isDefault });

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => undefined),
    listWorkflows: vi.fn(async () => [rule("cal-247")]),
    updateWorkflow: vi.fn(async () => rule()),
    listEntities: vi.fn(async () => [{ name: "tk_tickets", label: "Phiếu hỗ trợ" }]),
    listFields: vi.fn(async () => [{ name: "due_date", label: "Hạn xử lý", fieldType: "datetime" }]),
  };
});

vi.mock("@/lib/admin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/admin")>();
  return {
    ...actual,
    listBusinessCalendars: vi.fn(async () => [cal("cal-office", "Giờ hành chính", true), cal("cal-247", "Trực 24/7", false)]),
    listTeams: vi.fn(async () => []),
  };
});

const nav = vi.hoisted(() => ({ path: "/studio/du-an/acme/dung/tu-dong" }));
vi.mock("next/navigation", () => ({ usePathname: () => nav.path, useRouter: () => ({ push: vi.fn(), replace: vi.fn() }), useSearchParams: () => new URLSearchParams() }));


const calendarSelect = () => screen.getByLabelText("Lịch làm việc") as HTMLSelectElement;
const save = async () => {
  await touchRule();
  const btn = screen.getAllByRole("button").find((b) => b.textContent?.trim() === "Cập nhật");
  expect(btn).toBeDefined();
  fireEvent.click(btn!);
};
const savedAction = () =>
  (vi.mocked(updateWorkflow).mock.calls[0][1] as { actions: Record<string, unknown>[] }).actions[0];

beforeEach(() => vi.clearAllMocks());

describe("WorkflowBuilder — set_deadline pinned to its own working calendar (#79)", () => {
  it("shows the pin by name and does not claim the deadline walks the default calendar", async () => {
    render(<WorkflowBuilder />);
    await waitFor(() => expect(screen.getByRole("option", { name: "Trực 24/7" })).toBeInTheDocument(), { timeout: 3000 });
    expect(calendarSelect().value).toBe("cal-247");
    expect(screen.queryByText(/lịch làm việc mặc định của tổ chức/)).toBeNull();
    expect(screen.getByText(/Tính theo lịch đã chọn/)).toBeInTheDocument();
  });

  it("choosing the organisation default removes the pin from the save", async () => {
    render(<WorkflowBuilder />);
    await waitFor(() => expect(calendarSelect().value).toBe("cal-247"), { timeout: 3000 });
    fireEvent.change(calendarSelect(), { target: { value: "" } });
    await save();
    await waitFor(() => expect(updateWorkflow).toHaveBeenCalled());
    expect(savedAction()).toMatchObject({ type: "set_deadline", offset: { amount: 12, unit: "business_hours" } });
    expect(savedAction()).not.toHaveProperty("calendarId");
  });

  it("pins a calendar picked from the list", async () => {
    vi.mocked(listWorkflows).mockResolvedValueOnce([rule()] as never);
    render(<WorkflowBuilder />);
    await waitFor(() => expect(screen.getByRole("option", { name: "Trực 24/7" })).toBeInTheDocument(), { timeout: 3000 });
    expect(calendarSelect().value).toBe("");
    fireEvent.change(calendarSelect(), { target: { value: "cal-247" } });
    await save();
    await waitFor(() => expect(updateWorkflow).toHaveBeenCalled());
    expect(savedAction()).toMatchObject({ calendarId: "cal-247" });
  });

  it("shows a deleted pin and puts its 422 on the calendar dropdown with a way out", async () => {
    vi.mocked(listWorkflows).mockResolvedValueOnce([rule("cal-dead")] as never);
    vi.mocked(updateWorkflow).mockRejectedValueOnce(new ApiError(
      422, "SET_DEADLINE_CALENDAR_NOT_FOUND",
      'lịch làm việc "cal-dead" ghim cho action set_deadline không tồn tại trong tổ chức',
      { field: "actions[0].calendarId" },
    ));
    render(<WorkflowBuilder />);
    await waitFor(() => expect(screen.getByRole("option", { name: "Trực 24/7" })).toBeInTheDocument(), { timeout: 3000 });
    // The dead pin stays visible (not silently swapped for "default"), named as gone.
    expect(calendarSelect().value).toBe("cal-dead");
    expect(screen.getByRole("option", { name: /cal-dead.*không còn/ })).toBeInTheDocument();
    await save();
    await waitFor(() => expect(calendarSelect()).toHaveAttribute("aria-invalid", "true"));
    // The guidance names the control that fixes it; the raw engine sentence (UUID, "action
    // set_deadline") does not reach the screen.
    expect(screen.getAllByText(/chọn lịch khác hoặc «Lịch mặc định của tổ chức»/).length).toBeGreaterThan(0);
    expect(screen.queryByText(/ghim cho action set_deadline/)).toBeNull();
  });

  it("the link to the calendars goes through studioHref inside a project (T162)", async () => {
    render(<WorkflowBuilder />);
    await waitFor(() => expect(screen.getByRole("option", { name: "Trực 24/7" })).toBeInTheDocument(), { timeout: 3000 });
    const link = screen.getByRole("link", { name: /Tạo lịch làm việc/ });
    expect(link.getAttribute("href")).toBe(studioHref("acme", "dung/tu-dong/lich-lam-viec"));
  });
});
