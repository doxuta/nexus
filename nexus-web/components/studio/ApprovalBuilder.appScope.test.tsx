// Queue #456 approval-panel-app-scope (same family as #297 automation-panel-app-scope): the
// approval panel of ONE app (/studio/apps/<slug>?panel=duyet) must never open with another app's
// process in the editor. The list was filtered by the app's tables, but the selection was taken
// from the WHOLE tenant's list before that filter — so an app with no process listed "0 quy trình"
// beside another app's "Duyệt hợp đồng", its Xoá and Cập nhật live — and a hardcoded "accounts"
// default read GET /metadata/fields/accounts on every mount (404 on a tenant without that table).
// Pinned here: the default pick, the pick after a delete, the scope that arrives after the
// processes, and a new draft that lands on the app's own table (or, when it has none, no draft).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApprovalBuilder } from "./ApprovalBuilder";
import {
  ensureAuth, listEntities, listAdminUsers, listRoles, listFields,
  listApprovalProcesses, deleteApprovalProcess, type ApprovalProcessSummary,
} from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(), listEntities: vi.fn(), listAdminUsers: vi.fn(), listRoles: vi.fn(),
    listFields: vi.fn(), listApprovalProcesses: vi.fn(), createApprovalProcess: vi.fn(),
    updateApprovalProcess: vi.fn(), deleteApprovalProcess: vi.fn(),
  };
});


const proc = (id: string, name: string, entityName: string): ApprovalProcessSummary => ({
  id, name, entityName, trigger: "manual", isActive: true,
  steps: [{ id: "s1", approverType: "role", approverId: "r-mgr", slaSeconds: 3600 }],
});
// The tenant's first process belongs to CRM — exactly the one the bug opened under every app.
const CONTRACT = proc("p-contract", "Duyệt hợp đồng", "crm_contracts");
const CLASS_A = proc("p-class-a", "Duyệt mở lớp", "sm_classes");
const CLASS_B = proc("p-class-b", "Duyệt đổi lịch học", "sm_classes");

// The scope is a PROP of the page now (specs/074): what `?app=` resolves to.
type Scope = { appLabel: string | null; entityNames: Set<string> | null; scopeLoading: boolean };
let scope: Scope = { appLabel: null, entityNames: null, scopeLoading: false };
// The LMS app's nav lists one table; CRM's crm_contracts is the tenant's FIRST table.
const LEARNING_APP = (): Scope => ({ appLabel: "Đào tạo", entityNames: new Set(["sm_classes"]), scopeLoading: false });

const nameBox = () => screen.queryByRole("textbox", { name: "Tên quy trình" }) as HTMLInputElement | null;
const openName = () => nameBox()?.value ?? null;
const processList = () => screen.queryByRole("listbox", { name: "Quy trình phê duyệt" });

function renderBuilder() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const ui = () => <QueryClientProvider client={qc}><ApprovalBuilder {...scope} /></QueryClientProvider>;
  const r = render(ui());
  return { rerender: () => r.rerender(ui()) };
}

async function deleteOpen(id: string) {
  fireEvent.click(screen.getByRole("button", { name: "Xoá" }));
  fireEvent.click(await screen.findByRole("button", { name: "Xoá quy trình" })); // in-page confirm, no dialog
  await waitFor(() => expect(deleteApprovalProcess).toHaveBeenCalledWith(id));
}

beforeEach(() => {
  scope = { appLabel: null, entityNames: null, scopeLoading: false };
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(listApprovalProcesses).mockReset().mockResolvedValue([]);
  vi.mocked(listEntities).mockReset().mockResolvedValue([
    { name: "crm_contracts", label: "Hợp đồng", tableName: "obj_crm_contracts" },
    { name: "sm_classes", label: "Lớp học", tableName: "obj_sm_classes" },
  ]);
  vi.mocked(listAdminUsers).mockReset().mockResolvedValue([]);
  vi.mocked(listRoles).mockReset().mockResolvedValue([{ id: "r-mgr", name: "manager", label: "Trưởng phòng" }]);
  vi.mocked(listFields).mockReset().mockResolvedValue([]);
  vi.mocked(deleteApprovalProcess).mockReset().mockResolvedValue(undefined);
});

describe("ApprovalBuilder under an app scope", () => {
  it("an app with no process opens NONE: empty state, no Xoá / Cập nhật of another app's process", async () => {
    scope = LEARNING_APP();
    vi.mocked(listApprovalProcesses).mockResolvedValue([CONTRACT]);
    renderBuilder();

    expect(await screen.findByRole("button", { name: "Tạo quy trình" })).toBeInTheDocument();
    expect(screen.getByText(/^0 quy trình/)).toBeInTheDocument();
    expect(screen.queryByText(CONTRACT.name)).toBeNull();
    expect(nameBox()).toBeNull();
    expect(screen.queryByRole("button", { name: "Xoá" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Cập nhật" })).toBeNull();
    // Nothing was opened, so no table's fields were read (the old "accounts" default 404'd).
    expect(listFields).not.toHaveBeenCalled();
  });

  it("an app with processes opens ITS first process, not the tenant's first", async () => {
    scope = LEARNING_APP();
    vi.mocked(listApprovalProcesses).mockResolvedValue([CONTRACT, CLASS_A, CLASS_B]);
    renderBuilder();

    await waitFor(() => expect(openName()).toBe(CLASS_A.name));
    const options = within(processList()!).getAllByRole("option");
    expect(options.map((o) => o.getAttribute("aria-selected"))).toEqual(["true", "false"]);
    expect(within(processList()!).queryByText(CONTRACT.name)).toBeNull();
    // Only the open process's table is read — never "accounts", never the other app's table.
    await waitFor(() => expect(listFields).toHaveBeenCalledWith("sm_classes"));
    expect(listFields).not.toHaveBeenCalledWith("accounts");
    expect(listFields).not.toHaveBeenCalledWith("crm_contracts");
  });

  it("after a delete the next pick stays inside the app — and none left means the empty state", async () => {
    scope = LEARNING_APP();
    vi.mocked(listApprovalProcesses).mockResolvedValue([CONTRACT, CLASS_A, CLASS_B]);
    renderBuilder();
    await waitFor(() => expect(openName()).toBe(CLASS_A.name));

    await deleteOpen(CLASS_A.id);
    await waitFor(() => expect(openName()).toBe(CLASS_B.name));

    await deleteOpen(CLASS_B.id);
    expect(await screen.findByRole("button", { name: "Tạo quy trình" })).toBeInTheDocument();
    expect(nameBox()).toBeNull();
    expect(screen.queryByRole("button", { name: "Xoá" })).toBeNull();
    // Not even for one frame: a pick of the tenant's next process (CRM's), corrected after paint,
    // would still have opened it and read its table.
    expect(listFields).not.toHaveBeenCalledWith("crm_contracts");
  });

  it("a scope that arrives after the processes never opens another app's process", async () => {
    // AppWorkspace syncs the active app in an effect and the app's nav loads after that: the
    // processes can land while the builder still reads a loading scope.
    scope = { appLabel: "Đào tạo", entityNames: null, scopeLoading: true };
    vi.mocked(listApprovalProcesses).mockResolvedValue([CONTRACT, CLASS_A]);
    const { rerender } = renderBuilder();
    await waitFor(() => expect(listApprovalProcesses).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 50));
    // While the scope loads: nothing open, no list of every app's processes, no empty-state flash.
    expect(nameBox()).toBeNull();
    expect(screen.queryByText(CONTRACT.name)).toBeNull();
    expect(processList()).toBeNull();
    expect(screen.queryByRole("button", { name: "Tạo quy trình" })).toBeNull();

    scope = LEARNING_APP();
    rerender();
    await waitFor(() => expect(openName()).toBe(CLASS_A.name));

    // And a process already open that a later scope hides is swapped out, not left in the editor.
    scope = { appLabel: "Nhân sự", entityNames: new Set(["hr_staff"]), scopeLoading: false };
    rerender();
    await waitFor(() => expect(nameBox()).toBeNull());
    expect(screen.queryByRole("button", { name: "Xoá" })).toBeNull();
  });

  it("initialProcessId opens exactly that process and reports it; one outside the scope is dropped", async () => {
    scope = LEARNING_APP();
    vi.mocked(listApprovalProcesses).mockResolvedValue([CONTRACT, CLASS_A, CLASS_B]);
    const onSelect = vi.fn();
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { unmount } = render(
      <QueryClientProvider client={qc}><ApprovalBuilder {...scope} initialProcessId={CLASS_B.id} onSelect={onSelect} /></QueryClientProvider>,
    );
    await waitFor(() => expect(openName()).toBe(CLASS_B.name));
    await waitFor(() => expect(onSelect).toHaveBeenLastCalledWith(CLASS_B.id));
    unmount();

    // `duyet/<id>` of another app's process (or one that does not exist) never opens it.
    onSelect.mockClear();
    render(<QueryClientProvider client={qc}><ApprovalBuilder {...scope} initialProcessId={CONTRACT.id} onSelect={onSelect} /></QueryClientProvider>);
    await waitFor(() => expect(openName()).toBe(CLASS_A.name));
    expect(screen.queryByText(CONTRACT.name)).toBeNull();
    await waitFor(() => expect(onSelect).toHaveBeenLastCalledWith(CLASS_A.id));
  });

  it("an app with no process and a foreign id opens nothing and reports none", async () => {
    scope = { appLabel: "Nhân sự", entityNames: new Set(["hr_staff"]), scopeLoading: false };
    vi.mocked(listApprovalProcesses).mockResolvedValue([CONTRACT]);
    const onSelect = vi.fn();
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={qc}><ApprovalBuilder {...scope} initialProcessId={CONTRACT.id} onSelect={onSelect} /></QueryClientProvider>);
    await screen.findByText(/^0 quy trình/);
    expect(nameBox()).toBeNull();
    expect(screen.queryByRole("button", { name: "Xoá" })).toBeNull();
    await waitFor(() => expect(onSelect).toHaveBeenLastCalledWith(null));
  });

  it("with no app scope (toàn tenant) the tenant's first process opens, as before", async () => {
    vi.mocked(listApprovalProcesses).mockResolvedValue([CONTRACT, CLASS_A]);
    renderBuilder();
    await waitFor(() => expect(openName()).toBe(CONTRACT.name));
    expect(screen.getByRole("button", { name: "Xoá" })).toBeInTheDocument();
  });

  it("a new process under an app scope is drafted on the APP's table, never the tenant's first", async () => {
    scope = LEARNING_APP();
    vi.mocked(listApprovalProcesses).mockResolvedValue([CONTRACT]);
    renderBuilder();
    await screen.findByText(/^0 quy trình/);

    fireEvent.click(screen.getByRole("button", { name: "Quy trình mới" }));
    const entity = await screen.findByRole("combobox", { name: "Đối tượng áp dụng" });
    expect(entity).toHaveValue("sm_classes");
    expect(within(entity).queryByRole("option", { name: "Hợp đồng" })).toBeNull();
  });

  it("a draft started after leaving \"Xem toàn tenant\" does not inherit the other app's table", async () => {
    vi.mocked(listApprovalProcesses).mockResolvedValue([CONTRACT]);
    const { rerender } = renderBuilder();
    await waitFor(() => expect(openName()).toBe(CONTRACT.name)); // tenant-wide: CRM's process open

    scope = LEARNING_APP();
    rerender();
    fireEvent.click(await screen.findByRole("button", { name: "Tạo quy trình" }));
    expect(await screen.findByRole("combobox", { name: "Đối tượng áp dụng" })).toHaveValue("sm_classes");
  });

  it("an app with no table says so and starts no draft (none on another app's table)", async () => {
    scope = { appLabel: "Đào tạo", entityNames: new Set(), scopeLoading: false };
    vi.mocked(listApprovalProcesses).mockResolvedValue([CONTRACT]);
    renderBuilder();

    expect(await screen.findByRole("heading", { name: "“Đào tạo” chưa có bảng nào" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Tạo quy trình" })).toBeNull();
    const add = screen.getByRole("button", { name: "Quy trình mới" });
    expect(add).toBeDisabled();
    fireEvent.click(add);
    expect(nameBox()).toBeNull();
    expect(listFields).not.toHaveBeenCalled();
  });
});
