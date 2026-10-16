// Queue #297 automation-panel-app-scope: the automation panel of ONE app (/studio/apps/<slug>
// ?panel=tu-dong) must never open with another app's rule in the editor. The list was filtered by
// the app's tables, but the selection was taken from the WHOLE tenant's list before that filter —
// so an app with zero rules showed "0 luật · đang lọc theo app" beside the Ticketing SLA rule,
// with its Xoá/Cập nhật buttons live. Pinned here: the default pick, the pick after a delete, the
// scope that arrives after the rules, and the recent-runs strip reading only this app's rules.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { WorkflowBuilder, type WorkflowBuilderProps } from "./WorkflowBuilder";
import { deleteWorkflow, listFields, listRecentWorkflowRuns, listWorkflows } from "@/lib/api";
import type { AppSummary } from "@/lib/api";

const rule = (id: string, name: string, entityName: string) => ({
  id, name, entityName, trigger: "after_create", isActive: true,
  condition: null,
  actions: [{ type: "in_app_notification", userId: "$owner", title: "T", message: "M" }],
});
// The tenant's first rule belongs to Ticketing — exactly the one the bug opened everywhere.
const TICKET_SLA = rule("wf-ticket", "Đặt hạn SLA — phiếu thường", "tk_tickets");
const CLASS_A = rule("wf-class-a", "Nhắc lớp sắp khai giảng", "sm_classes");
const CLASS_B = rule("wf-class-b", "Báo lớp đầy", "sm_classes");

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => undefined),
    listWorkflows: vi.fn(async () => []),
    deleteWorkflow: vi.fn(async () => undefined),
    listEntities: vi.fn(async () => [
      { name: "tk_tickets", label: "Phiếu hỗ trợ" },
      { name: "sm_classes", label: "Lớp học" },
    ]),
    listFields: vi.fn(async () => [{ name: "subject", label: "Tiêu đề", fieldType: "text" }]),
    listWorkflowRuns: vi.fn(async () => ({ runs: [], total: 0, limit: 20, offset: 0 })),
    listRecentWorkflowRuns: vi.fn(async () => []),
  };
});

vi.mock("@/lib/admin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/admin")>();
  return { ...actual, listBusinessCalendars: vi.fn(async () => []), listTeams: vi.fn(async () => []) };
});

// The scope the page hands the builder as props (specs/074) — a test changes it, then re-renders.
let scope: { app?: AppSummary | null; entityNames: Set<string> | null; isLoading: boolean } =
  { entityNames: null, isLoading: false };
const Wb = (extra: Partial<WorkflowBuilderProps> = {}) => (
  <WorkflowBuilder app={scope.app ?? null} entityNames={scope.entityNames} scopeLoading={scope.isLoading} {...extra} />
);


const LEARNING_APP = () => ({ entityNames: new Set(["sm_classes"]), isLoading: false });
const CLINIC: AppSummary = { id: "app-clinic", name: "clinic", label: "Phòng khám", icon: null, color: null, isDefault: false, sortOrder: 0 };
const entityBox = () => screen.queryByRole("combobox", { name: "Bảng dữ liệu" });
const ruleList = () => screen.getByRole("list", { name: "Luật tự động" });
const editorTitle = () => document.querySelector(".slds-page-header__title")?.textContent ?? null;

// The strip reads 8 runs; the list's "last run failed" marks (#337) read 100 per group — tell them apart.
const stripCalls = () => vi.mocked(listRecentWorkflowRuns).mock.calls.filter((c) => c[0] === 8);

beforeEach(() => {
  scope = { entityNames: null, isLoading: false };
  vi.mocked(listWorkflows).mockReset();
  vi.mocked(deleteWorkflow).mockClear();
  vi.mocked(listFields).mockClear();
  vi.mocked(listRecentWorkflowRuns).mockReset();
  vi.mocked(listRecentWorkflowRuns).mockResolvedValue([]);
});

describe("WorkflowBuilder under an app scope", () => {
  it("an app with no rules selects NO rule: empty state, no Xoá / Cập nhật of another app's rule", async () => {
    scope = LEARNING_APP();
    vi.mocked(listWorkflows).mockResolvedValue([TICKET_SLA]);
    render(<Wb />);

    expect(await screen.findByRole("button", { name: "Tạo luật đầu tiên" })).toBeInTheDocument();
    expect(screen.getByText("0 luật")).toBeInTheDocument();
    expect(screen.queryByText(TICKET_SLA.name)).toBeNull();
    expect(screen.queryByRole("button", { name: "Xoá" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Cập nhật" })).toBeNull();
    // Nothing was opened, so no rule's fields were read either (the old default "accounts" 404'd).
    expect(listFields).not.toHaveBeenCalled();
  });

  it("an app with rules opens ITS first rule, not the tenant's first", async () => {
    scope = LEARNING_APP();
    vi.mocked(listWorkflows).mockResolvedValue([TICKET_SLA, CLASS_A, CLASS_B]);
    render(<Wb />);

    const first = await within(ruleList()).findByRole("button", { name: new RegExp(`^${CLASS_A.name}`) });
    await waitFor(() => expect(first).toHaveAttribute("aria-current", "page"));
    expect(editorTitle()).toBe(CLASS_A.name);
    expect(within(ruleList()).queryByText(TICKET_SLA.name)).toBeNull();
  });

  it("after a delete the next pick stays inside the app — and none left means no pick", async () => {
    scope = LEARNING_APP();
    vi.mocked(listWorkflows).mockResolvedValueOnce([TICKET_SLA, CLASS_A, CLASS_B]);
    render(<Wb />);
    await waitFor(() => expect(editorTitle()).toBe(CLASS_A.name));

    vi.mocked(listWorkflows).mockResolvedValueOnce([TICKET_SLA, CLASS_B]);
    await userEvent.click(screen.getByRole("button", { name: "Xoá" }));
    await userEvent.click(within(await screen.findByRole("group", { name: "Xoá luật?" })).getByRole("button", { name: "Xoá luật" }));
    await waitFor(() => expect(deleteWorkflow).toHaveBeenCalledWith(CLASS_A.id));
    await waitFor(() => expect(editorTitle()).toBe(CLASS_B.name));

    vi.mocked(listWorkflows).mockResolvedValueOnce([TICKET_SLA]);
    await userEvent.click(screen.getByRole("button", { name: "Xoá" }));
    await userEvent.click(within(await screen.findByRole("group", { name: "Xoá luật?" })).getByRole("button", { name: "Xoá luật" }));
    await waitFor(() => expect(deleteWorkflow).toHaveBeenCalledWith(CLASS_B.id));
    expect(await screen.findByRole("button", { name: "Tạo luật đầu tiên" })).toBeInTheDocument();
    expect(editorTitle()).toBeNull();
  });

  it("a scope that arrives after the rules still moves the pick into the app", async () => {
    // The workspace syncs the active app in an effect, and the app's nav loads after that: the
    // rules can land while the builder still reads the previous (tenant-wide or loading) scope.
    scope = { entityNames: null, isLoading: true };
    vi.mocked(listWorkflows).mockResolvedValue([TICKET_SLA, CLASS_A]);
    const { rerender } = render(<Wb />);
    await waitFor(() => expect(listWorkflows).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 50));
    // While the scope loads: nothing picked, and the list lists nobody's rules (not every app's).
    expect(editorTitle()).toBeNull();
    expect(within(ruleList()).queryByRole("button", { name: new RegExp(`^${TICKET_SLA.name}`) })).toBeNull();
    expect(within(ruleList()).getAllByText("Đang tải…").length).toBeGreaterThan(0);

    scope = LEARNING_APP();
    rerender(<Wb />);
    await waitFor(() => expect(editorTitle()).toBe(CLASS_A.name));

    // And a rule already open that a later scope hides is swapped out, not left in the editor.
    scope = { entityNames: new Set(["hr_staff"]), isLoading: false };
    rerender(<Wb />);
    await waitFor(() => expect(editorTitle()).toBeNull());
  });

  it("while the scope loads no runs are read; once it lands only the app's; \"Xem toàn tenant\" keeps the open rule", async () => {
    scope = { entityNames: null, isLoading: true };
    vi.mocked(listWorkflows).mockResolvedValue([TICKET_SLA, CLASS_A]);
    const { rerender } = render(<Wb />);
    await waitFor(() => expect(listWorkflows).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 50));
    expect(listRecentWorkflowRuns).not.toHaveBeenCalled(); // not every app's runs first

    scope = LEARNING_APP();
    rerender(<Wb />);
    await waitFor(() => expect(editorTitle()).toBe(CLASS_A.name));
    await waitFor(() => expect(listRecentWorkflowRuns).toHaveBeenCalledWith(8, [CLASS_A.id]));

    // "Xem toàn tenant" is the maker's own choice: honoured, the open rule stays, the strip reads
    // every rule.
    scope = { entityNames: null, isLoading: false };
    rerender(<Wb />);
    await waitFor(() => expect(within(ruleList()).getByText(TICKET_SLA.name)).toBeInTheDocument());
    expect(editorTitle()).toBe(CLASS_A.name);
    await waitFor(() => expect(stripCalls().at(-1)).toEqual([8, undefined]));
  });

  it("with no app scope (toàn tenant) the first rule of the tenant opens, as before", async () => {
    vi.mocked(listWorkflows).mockResolvedValue([TICKET_SLA, CLASS_A]);
    render(<Wb />);
    await waitFor(() => expect(editorTitle()).toBe(TICKET_SLA.name));
    // …and "Chạy gần đây" reads every rule's runs (no rule filter on the wire).
    await waitFor(() => expect(listRecentWorkflowRuns).toHaveBeenCalled());
    expect(vi.mocked(listRecentWorkflowRuns).mock.calls[0][1]).toBeUndefined();
  });

  it("\"Chạy gần đây\" asks for the runs of THIS app's rules only", async () => {
    scope = LEARNING_APP();
    vi.mocked(listWorkflows).mockResolvedValue([TICKET_SLA, CLASS_A, CLASS_B]);
    vi.mocked(listRecentWorkflowRuns).mockResolvedValue([
      { id: "run-1", workflowId: CLASS_B.id, status: "success", startedAt: "2026-09-30T08:00:00Z" } as never,
    ]);
    render(<Wb />);
    const strip = (await screen.findByRole("heading", { name: "Chạy gần đây" })).closest("section") as HTMLElement;
    await waitFor(() => expect(listRecentWorkflowRuns).toHaveBeenCalled());
    expect(stripCalls().at(-1)).toEqual([8, [CLASS_A.id, CLASS_B.id]]);
    expect(await within(strip).findByText(CLASS_B.name)).toBeInTheDocument();
  });

  it("an app with no rules has no \"Chạy gần đây\" strip and asks for no runs", async () => {
    scope = LEARNING_APP();
    vi.mocked(listWorkflows).mockResolvedValue([TICKET_SLA]);
    render(<Wb />);
    await screen.findByRole("button", { name: "Tạo luật đầu tiên" });
    expect(screen.queryByRole("heading", { name: "Chạy gần đây" })).toBeNull();
    expect(listRecentWorkflowRuns).not.toHaveBeenCalled();
  });

  // Queue #457: the table a NEW rule is drafted on. startCreate took entityOptions[0] ?? the
  // tenant's first table ?? "accounts" — an app with no table drafted on tk_tickets, and the table of
  // a rule left open from "Xem toàn tenant" (entityOptions keeps a stored value listed) came first.
  it("an app with no table says so and starts no draft (none on another app's table)", async () => {
    scope = { app: CLINIC, entityNames: new Set(), isLoading: false };
    vi.mocked(listWorkflows).mockResolvedValue([TICKET_SLA]);
    render(<Wb />);

    expect(await screen.findByRole("heading", { name: "“Phòng khám” chưa có bảng nào" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Tạo luật đầu tiên" })).toBeNull();
    const add = screen.getByRole("button", { name: "Tạo luật mới" });
    expect(add).toBeDisabled();
    await userEvent.click(add);
    expect(entityBox()).toBeNull();
    expect(listFields).not.toHaveBeenCalled();
  });

  it("a draft under an app is on the APP's table, never the tenant's first", async () => {
    scope = LEARNING_APP();
    vi.mocked(listWorkflows).mockResolvedValue([TICKET_SLA]);
    render(<Wb />);

    await userEvent.click(await screen.findByRole("button", { name: "Tạo luật đầu tiên" }));
    const entity = await screen.findByRole("combobox", { name: "Bảng dữ liệu" });
    expect(entity).toHaveValue("sm_classes");
    expect(within(entity).queryByRole("option", { name: "Phiếu hỗ trợ" })).toBeNull();
  });

  it("a draft started after leaving \"Xem toàn tenant\" does not inherit the other app's table", async () => {
    vi.mocked(listWorkflows).mockResolvedValue([TICKET_SLA]);
    const { rerender } = render(<Wb />);
    await waitFor(() => expect(editorTitle()).toBe(TICKET_SLA.name)); // tenant-wide: Ticketing's rule open

    scope = LEARNING_APP();
    rerender(<Wb />);
    await userEvent.click(await screen.findByRole("button", { name: "Tạo luật đầu tiên" }));
    expect(await screen.findByRole("combobox", { name: "Bảng dữ liệu" })).toHaveValue("sm_classes");
  });
});

// specs/074 lát f2-rule-page: the rule named by the URL is opened only when the filtered list shows it.
describe("WorkflowBuilder initialRuleId (rule page)", () => {
  it("a rule of another app is dropped: the app's empty sentence shows, not that rule", async () => {
    scope = { ...LEARNING_APP(), app: CLINIC };
    vi.mocked(listWorkflows).mockResolvedValue([TICKET_SLA]);
    render(<Wb hideList initialRuleId="wf-ticket" />);
    expect(await screen.findByText("App Phòng khám chưa có luật")).toBeInTheDocument();
    expect(editorTitle()).toBeNull();
    expect(screen.queryByRole("button", { name: "Xoá" })).toBeNull();
  });

  it("a rule in the app opens, and onSelectRule is not needed for it", async () => {
    scope = LEARNING_APP();
    vi.mocked(listWorkflows).mockResolvedValue([TICKET_SLA, CLASS_A, CLASS_B]);
    render(<Wb hideList initialRuleId="wf-class-b" />);
    await waitFor(() => expect(editorTitle()).toBe("Báo lớp đầy"));
  });

  it("an id that does not exist says so, and opens nothing", async () => {
    vi.mocked(listWorkflows).mockResolvedValue([TICKET_SLA]);
    render(<Wb hideList initialRuleId="nope" />);
    expect(await screen.findByText("Không tìm thấy luật này")).toBeInTheDocument();
    expect(editorTitle()).toBeNull();
  });

  it("newDraft opens a blank rule on the named table, trigger and name", async () => {
    vi.mocked(listWorkflows).mockResolvedValue([]);
    render(<Wb hideList newDraft={{ entity: "sm_classes", trigger: "after_update", name: "Luật mới của tôi" }} />);
    await waitFor(() => expect(editorTitle()).toBe("Luật mới của tôi"));
    expect(screen.getByRole("combobox", { name: "Bảng dữ liệu" })).toHaveValue("sm_classes");
  });
});
