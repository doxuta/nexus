// #64 found by a real click (2026-09-29): a set_deadline action on an after_create rule — exactly the
// shape of the four Ticketing SLA rules — never loaded the entity's fields, because the load effect
// was armed only for date_offset triggers and assign actions. The target dropdown came up empty, the
// saved target showed "không còn hợp kiểu", and a new deadline rule on create could not pick a field.
import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { WorkflowBuilder } from "./WorkflowBuilder";

const RULE = {
  id: "rule-1",
  name: "Đặt hạn SLA — phiếu thường",
  entityName: "tk_tickets",
  trigger: "after_create",
  isActive: true,
  condition: null,
  actions: [{ type: "set_deadline", field: "due_date", offset: { amount: 40, unit: "business_hours" } }],
};

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => undefined),
    listWorkflows: vi.fn(async () => [RULE]),
    listEntities: vi.fn(async () => [{ name: "tk_tickets", label: "Phiếu hỗ trợ" }]),
    listFields: vi.fn(async () => [
      { name: "title", label: "Tiêu đề", fieldType: "text" },
      { name: "due_date", label: "Hạn xử lý", fieldType: "datetime" },
      { name: "opened_on", label: "Ngày mở", fieldType: "date" },
    ]),
  };
});

vi.mock("@/lib/admin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/admin")>();
  return { ...actual, listBusinessCalendars: vi.fn(async () => []), listTeams: vi.fn(async () => []) };
});


describe("WorkflowBuilder — set_deadline on an after_create rule", () => {
  it("lists the entity's datetime fields and keeps the saved target valid", async () => {
    render(<WorkflowBuilder />);
    // The saved target resolves to a real datetime option (by label), not the stale-type fallback.
    await waitFor(() => expect(screen.getByRole("option", { name: "Hạn xử lý" })).toBeInTheDocument(), { timeout: 3000 });
    expect(screen.queryByText(/không còn hợp kiểu/)).toBeNull();
    expect(screen.queryByText(/chưa có trường kiểu ngày giờ/)).toBeNull();
    // Only datetime fields are offered as a deadline target.
    expect(screen.queryByRole("option", { name: "Ngày mở" })).toBeNull();
    expect(screen.queryByRole("option", { name: "Tiêu đề" })).toBeNull();
  });
});
