// #337: the rule list marks a rule whose NEWEST run failed, and the mark opens that run in the history tab.
import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { WorkflowBuilder } from "./WorkflowBuilder";
import { listRecentWorkflowRuns, listWorkflowRuns } from "@/lib/api";

const rule = (id: string, name: string) => ({
  id, name, entityName: "crm_contacts", trigger: "after_create", isActive: true, condition: null,
  actions: [{ type: "in_app_notification", userId: "$owner", title: "x", message: "y" }],
});

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => undefined),
    listWorkflows: vi.fn(async () => [rule("w-bad", "Luật hỏng"), rule("w-ok", "Luật đã sửa")]),
    // newest first: w-bad's newest run failed; w-ok failed once but its newest run succeeded.
    listRecentWorkflowRuns: vi.fn(async () => [
      { id: "r-bad", workflowId: "w-bad", status: "failed", triggerType: "after_create", actionsResult: null, startedAt: "2026-10-08T01:00:00Z", finishedAt: "2026-10-08T01:00:01Z" },
      { id: "r-ok", workflowId: "w-ok", status: "success", triggerType: "after_create", actionsResult: null, startedAt: "2026-10-08T00:59:00Z", finishedAt: "2026-10-08T00:59:01Z" },
      { id: "r-old", workflowId: "w-ok", status: "failed", triggerType: "after_create", actionsResult: null, startedAt: "2026-10-07T00:59:00Z", finishedAt: "2026-10-07T00:59:01Z" },
    ]),
    listWorkflowRuns: vi.fn(async () => ({ runs: [], total: 0, limit: 20, offset: 0 })),
    listEntities: vi.fn(async () => [{ name: "crm_contacts", label: "Liên hệ" }]),
    listFields: vi.fn(async () => [{ name: "first_name", label: "Tên", fieldType: "text" }]),
  };
});
vi.mock("@/lib/admin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/admin")>();
  return { ...actual, listBusinessCalendars: vi.fn(async () => []), listTeams: vi.fn(async () => []) };
});

describe("WorkflowBuilder — last run failed mark", () => {
  it("marks only the rule whose newest run failed, with one grouped read", async () => {
    render(<WorkflowBuilder />);
    const marks = await screen.findAllByRole("button", { name: "Lần chạy gần nhất lỗi" }, { timeout: 3000 });
    expect(marks).toHaveLength(1);
    expect(listRecentWorkflowRuns).toHaveBeenCalledWith(100, ["w-bad", "w-ok"]);
  });

  it("opens the run history of that rule", async () => {
    render(<WorkflowBuilder />);
    fireEvent.click(await screen.findByRole("button", { name: "Lần chạy gần nhất lỗi" }, { timeout: 3000 }));
    await waitFor(() => expect(listWorkflowRuns).toHaveBeenCalledWith("w-bad", expect.anything(), expect.anything()));
  });
});
