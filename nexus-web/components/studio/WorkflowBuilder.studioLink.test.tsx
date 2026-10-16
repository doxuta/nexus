// specs/073 kh2 T204: inside a Studio project the "create working calendar" links go to the Studio page, not /admin.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { WorkflowBuilder } from "./WorkflowBuilder";
import { listWorkflows } from "@/lib/api";

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


let pathname: string | null = "/studio/du-an/northwind/dung/tu-dong";
vi.mock("next/navigation", async (importOriginal) => ({ ...(await importOriginal<typeof import("next/navigation")>()), usePathname: () => pathname }));

beforeEach(() => vi.clearAllMocks());

describe("WorkflowBuilder — calendar links (T204)", () => {
  it("point at the Studio calendars page inside a project", async () => {
    pathname = "/studio/du-an/northwind/dung/tu-dong";
    render(<WorkflowBuilder />);
    await waitFor(() => expect(screen.getByRole("option", { name: "Trực 24/7" })).toBeInTheDocument(), { timeout: 3000 });
    const links = screen.getAllByRole("link", { name: "+ Tạo lịch làm việc" });
    expect(links.length).toBeGreaterThan(0);
    for (const a of links) expect(a).toHaveAttribute("href", "/studio/du-an/northwind/dung/tu-dong/lich-lam-viec");
  });

  it("keep the old admin screen outside a project", async () => {
    pathname = "/studio/workflows";
    render(<WorkflowBuilder />);
    await waitFor(() => expect(screen.getByRole("option", { name: "Trực 24/7" })).toBeInTheDocument(), { timeout: 3000 });
    for (const a of screen.getAllByRole("link", { name: "+ Tạo lịch làm việc" })) expect(a).toHaveAttribute("href", "/admin/business-calendars");
    expect(listWorkflows).toHaveBeenCalled();
  });
});
