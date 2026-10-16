// specs/050 S2b (T041): a rule whose webhook names a vault key ({credentialId, path} — set by API
// until the key picker lands in S9 / FE-named-credentials-builder) must survive being opened and
// re-saved in the old builder. toActionPayload only knew `{type, url}` and fell back to
// "https://example.com/hook": one "Cập nhật" silently rewired the rule to a placeholder host and
// dropped the key.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { WorkflowBuilder } from "./WorkflowBuilder";
import { listWorkflows, updateWorkflow } from "@/lib/api";
import { touchRule } from "@/test/touchRule";

const RULE = {
  id: "rule-vault",
  name: "Báo đơn cho cổng thanh toán",
  entityName: "crm_contacts",
  trigger: "after_create",
  isActive: true,
  condition: null,
  actions: [{ type: "outbound_webhook", credentialId: "cred-1", path: "/orders?kind=paid" }],
};

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => undefined),
    listWorkflows: vi.fn(async () => [RULE]),
    updateWorkflow: vi.fn(async () => RULE),
    listEntities: vi.fn(async () => [{ name: "crm_contacts", label: "Liên hệ" }]),
    listFields: vi.fn(async () => [{ name: "first_name", label: "Tên", fieldType: "text" }]),
  };
});

vi.mock("@/lib/admin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/admin")>();
  return { ...actual, listBusinessCalendars: vi.fn(async () => []), listTeams: vi.fn(async () => []) };
});


const savedActions = () => (vi.mocked(updateWorkflow).mock.calls.at(-1)![1] as { actions: unknown[] }).actions;

describe("WorkflowBuilder — a webhook that uses a vault key", () => {
  beforeEach(() => vi.mocked(updateWorkflow).mockClear());

  it("re-saves {credentialId, path} as is — no url, no placeholder host", async () => {
    render(<WorkflowBuilder />);
    await touchRule();
    fireEvent.click(await screen.findByRole("button", { name: "Cập nhật" }, { timeout: 3000 }));
    await waitFor(() => expect(updateWorkflow).toHaveBeenCalled(), { timeout: 3000 });
    expect(savedActions()).toEqual([{ type: "outbound_webhook", credentialId: "cred-1", path: "/orders?kind=paid" }]);
  });

  it("a key without a path saves without one", async () => {
    vi.mocked(listWorkflows).mockResolvedValueOnce([{ ...RULE, actions: [{ type: "outbound_webhook", credentialId: "cred-1" }] }]);
    render(<WorkflowBuilder />);
    await touchRule();
    fireEvent.click(await screen.findByRole("button", { name: "Cập nhật" }, { timeout: 3000 }));
    await waitFor(() => expect(updateWorkflow).toHaveBeenCalled(), { timeout: 3000 });
    expect(savedActions()).toEqual([{ type: "outbound_webhook", credentialId: "cred-1" }]);
  });

  it("hides the URL box and says the key is used, read-only", async () => {
    render(<WorkflowBuilder />);
    expect(await screen.findByText("Dùng khoá kết nối · /orders?kind=paid", {}, { timeout: 3000 })).toBeInTheDocument();
    expect(screen.queryByLabelText("URL webhook")).not.toBeInTheDocument();
    expect(screen.queryByDisplayValue("https://example.com/hook")).not.toBeInTheDocument();
  });

  it("a url webhook keeps its URL box and payload", async () => {
    vi.mocked(listWorkflows).mockResolvedValueOnce([{ ...RULE, actions: [{ type: "outbound_webhook", url: "https://erp.test/in" }] }]);
    render(<WorkflowBuilder />);
    expect(await screen.findByDisplayValue("https://erp.test/in", {}, { timeout: 3000 })).toBeInTheDocument();
    await touchRule();
    fireEvent.click(screen.getByRole("button", { name: "Cập nhật" }));
    await waitFor(() => expect(updateWorkflow).toHaveBeenCalled(), { timeout: 3000 });
    expect(savedActions()).toEqual([{ type: "outbound_webhook", url: "https://erp.test/in" }]);
  });
});
