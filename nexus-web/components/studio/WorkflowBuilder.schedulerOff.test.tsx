// #291: a "scheduled" rule never fires while the server timer is off — the builder must say so
// before the rule is saved, stay silent when the timer is on, and stay silent when the status
// cannot be read (unknown is not "off").
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { WorkflowBuilder } from "./WorkflowBuilder";
import { getWorkflowSchedulerStatus, listWorkflows } from "@/lib/api";

const rule = {
  id: "rule-291", name: "Nhắc hằng ngày", entityName: "tk_tickets", trigger: "scheduled",
  isActive: true, condition: null, intervalSeconds: 86400,
  actions: [{ type: "update_field", field: "status", value: "x" }],
};

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => undefined),
    listWorkflows: vi.fn(async () => [rule]),
    getWorkflowSchedulerStatus: vi.fn(async () => ({ enabled: false, intervalSeconds: 0 })),
    listEntities: vi.fn(async () => [{ name: "tk_tickets", label: "Phiếu hỗ trợ" }]),
    listFields: vi.fn(async () => [{ name: "status", label: "Trạng thái", fieldType: "text" }]),
  };
});
vi.mock("@/lib/admin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/admin")>();
  return { ...actual, listBusinessCalendars: vi.fn(async () => []), listTeams: vi.fn(async () => []) };
});

const warning = () => screen.queryByText(/bộ hẹn giờ/i);
const ready = () => waitFor(() => expect(screen.getByDisplayValue("Nhắc hằng ngày")).toBeInTheDocument(), { timeout: 3000 });

beforeEach(() => { vi.clearAllMocks(); vi.mocked(listWorkflows).mockResolvedValue([rule] as never); });

describe("WorkflowBuilder — scheduler status warning (#291)", () => {
  it("warns (role=status) when the server timer is off", async () => {
    render(<WorkflowBuilder />);
    await ready();
    await waitFor(() => expect(warning()).toBeInTheDocument());
    expect(warning()!.closest('[role="status"]')).not.toBeNull();
  });

  it("is silent when the timer is on", async () => {
    vi.mocked(getWorkflowSchedulerStatus).mockResolvedValue({ enabled: true, intervalSeconds: 30 });
    render(<WorkflowBuilder />);
    await ready();
    await waitFor(() => expect(getWorkflowSchedulerStatus).toHaveBeenCalled());
    expect(warning()).toBeNull();
  });

  it("is silent when the status call fails", async () => {
    vi.mocked(getWorkflowSchedulerStatus).mockRejectedValue(new Error("boom"));
    render(<WorkflowBuilder />);
    await ready();
    await waitFor(() => expect(getWorkflowSchedulerStatus).toHaveBeenCalled());
    expect(warning()).toBeNull();
  });
});
