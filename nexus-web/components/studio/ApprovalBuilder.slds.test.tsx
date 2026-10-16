// ApprovalBuilder on SLDS (specs/038 T044): the process list is a split view (listbox of processes,
// the open one aria-selected), each process's steps are an ordered vertical progress list in
// approval order, and delete goes through an SLDS modal. lib/api mocked at the module seam.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
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

const CONTRACT: ApprovalProcessSummary = {
  id: "p1", name: "Duyệt hợp đồng", entityName: "crm_contracts", trigger: "manual", isActive: true,
  steps: [
    { id: "s1", name: "Trưởng phòng duyệt", approverType: "role", approverId: "r-mgr", slaSeconds: 3600 },
    { id: "s2", approverType: "user", approverId: "u-1", slaSeconds: 7200 },
    { id: "s3", approverType: "role", approverId: "r-dir", slaSeconds: 3600 },
  ],
};
const QUOTE: ApprovalProcessSummary = {
  id: "p2", name: "Duyệt báo giá", entityName: "crm_quotes", trigger: "after_create", isActive: false,
  steps: [{ id: "s1", approverType: "role", approverId: "r-dir", slaSeconds: 3600 }],
};

beforeEach(() => {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(listApprovalProcesses).mockReset().mockResolvedValue([CONTRACT, QUOTE]);
  vi.mocked(listEntities).mockReset().mockResolvedValue([
    { name: "crm_contracts", label: "Hợp đồng", tableName: "obj_crm_contracts" },
    { name: "crm_quotes", label: "Báo giá", tableName: "obj_crm_quotes" },
  ]);
  vi.mocked(listAdminUsers).mockReset().mockResolvedValue([
    { id: "u-1", email: "an@example.com", fullName: "Nguyễn An" },
  ] as Awaited<ReturnType<typeof listAdminUsers>>);
  vi.mocked(listRoles).mockReset().mockResolvedValue([
    { id: "r-mgr", name: "manager", label: "Trưởng phòng" },
    { id: "r-dir", name: "director", label: "Giám đốc" },
  ]);
  vi.mocked(listFields).mockReset().mockResolvedValue([]);
  vi.mocked(deleteApprovalProcess).mockReset().mockResolvedValue(undefined);
});

function renderBuilder() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={qc}><ApprovalBuilder /></QueryClientProvider>);
}

describe("ApprovalBuilder — SLDS split view + vertical steps", () => {
  it("lists processes as split-view options and opens the first", async () => {
    renderBuilder();
    const list = await screen.findByRole("listbox", { name: "Quy trình phê duyệt" });
    const options = within(list).getAllByRole("option");
    expect(options.map((o) => o.getAttribute("aria-selected"))).toEqual(["true", "false"]);
    expect(options[0]).toHaveTextContent("Duyệt hợp đồng");
    expect(options[0]).toHaveTextContent("Hợp đồng · 3 bước");
    expect(options[1]).toHaveTextContent("Đang tắt");
    expect(screen.getByRole("textbox", { name: "Tên quy trình" })).toHaveValue("Duyệt hợp đồng");
  });

  it("shows the steps in approval order, each with its approver", async () => {
    renderBuilder();
    const steps = await screen.findByRole("list", { name: "Các bước phê duyệt" });
    const items = within(steps).getAllByRole("listitem").filter((li) => li.classList.contains("slds-progress__item"));
    expect(items).toHaveLength(3);
    expect(items[0]).toHaveTextContent("Bước 1");
    expect(items[0]).toHaveTextContent("Trưởng phòng duyệt");
    expect(within(items[0]).getByText("Trưởng phòng")).toBeInTheDocument();
    expect(within(items[1]).getByText("Nguyễn An")).toBeInTheDocument();
    expect(within(items[2]).getByText("Giám đốc")).toBeInTheDocument();
  });

  it("opens another process from the list", async () => {
    renderBuilder();
    const list = await screen.findByRole("listbox", { name: "Quy trình phê duyệt" });
    fireEvent.click(within(list).getByRole("option", { name: /Duyệt báo giá/ }));
    expect(screen.getByRole("textbox", { name: "Tên quy trình" })).toHaveValue("Duyệt báo giá");
    expect(within(list).getByRole("option", { name: /Duyệt báo giá/ })).toHaveAttribute("aria-selected", "true");
  });

  // split-view docs: the list must implement the ARIA listbox keyboard interaction — one tab stop,
  // arrows/Home/End move focus, Enter/Space open.
  describe("listbox keyboard", () => {
    const tabStops = (list: HTMLElement) =>
      within(list).getAllByRole("option").map((o) => o.getAttribute("tabindex"));

    it("has one tab stop, on the open process", async () => {
      renderBuilder();
      const list = await screen.findByRole("listbox", { name: "Quy trình phê duyệt" });
      expect(tabStops(list)).toEqual(["0", "-1"]);
    });

    it("ArrowDown/ArrowUp/Home/End move focus between processes and stop at the ends", async () => {
      renderBuilder();
      const list = await screen.findByRole("listbox", { name: "Quy trình phê duyệt" });
      const [contract, quote] = within(list).getAllByRole("option");
      contract.focus();
      await userEvent.keyboard("{ArrowDown}");
      expect(document.activeElement).toBe(quote);
      await userEvent.keyboard("{ArrowDown}");
      expect(document.activeElement).toBe(quote);
      await userEvent.keyboard("{Home}");
      expect(document.activeElement).toBe(contract);
      await userEvent.keyboard("{ArrowUp}");
      expect(document.activeElement).toBe(contract);
      await userEvent.keyboard("{End}");
      expect(document.activeElement).toBe(quote);
      // Moving focus does not open anything.
      expect(screen.getByRole("textbox", { name: "Tên quy trình" })).toHaveValue("Duyệt hợp đồng");
    });

    it("Enter opens the focused process, Space too", async () => {
      renderBuilder();
      const list = await screen.findByRole("listbox", { name: "Quy trình phê duyệt" });
      within(list).getAllByRole("option")[0].focus();
      await userEvent.keyboard("{ArrowDown}{Enter}");
      expect(screen.getByRole("textbox", { name: "Tên quy trình" })).toHaveValue("Duyệt báo giá");
      expect(within(list).getByRole("option", { name: /Duyệt báo giá/ })).toHaveAttribute("aria-selected", "true");
      within(list).getByRole("option", { name: /Duyệt báo giá/ }).focus();
      await userEvent.keyboard("{ArrowUp}[Space]");
      expect(screen.getByRole("textbox", { name: "Tên quy trình" })).toHaveValue("Duyệt hợp đồng");
    });

    it("the tab stop follows focus, so Tab leaves the list; back in, it is on the open process", async () => {
      renderBuilder();
      const list = await screen.findByRole("listbox", { name: "Quy trình phê duyệt" });
      const [contract, quote] = within(list).getAllByRole("option");
      contract.focus();
      await userEvent.keyboard("{ArrowDown}");
      expect(tabStops(list)).toEqual(["-1", "0"]);
      await userEvent.tab();
      expect(list).not.toContainElement(document.activeElement as HTMLElement);
      expect(tabStops(list)).toEqual(["0", "-1"]);
      await userEvent.tab({ shift: true });
      expect(document.activeElement).toBe(contract);
      expect(quote).toHaveAttribute("aria-selected", "false");
    });
  });

  it("deletes through the SLDS confirm modal and opens the next process", async () => {
    renderBuilder();
    await screen.findByRole("listbox", { name: "Quy trình phê duyệt" });
    fireEvent.click(screen.getByRole("button", { name: "Xoá" }));
    // In the page, not a dialog (specs/074 T154): the question names the process, the red button answers it.
    expect(await screen.findByText("Quy trình «Duyệt hợp đồng» sẽ bị xoá.")).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Xoá quy trình" }));
    await waitFor(() => expect(deleteApprovalProcess).toHaveBeenCalledWith("p1"));
    await waitFor(() => expect(screen.getByRole("textbox", { name: "Tên quy trình" })).toHaveValue("Duyệt báo giá"));
  });
});
