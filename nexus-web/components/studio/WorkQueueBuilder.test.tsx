// WorkQueueBuilder — VIEC-HOM-NAY Studio screen contract: list renders queue rows (label, entity,
// role visibility, active badge), the empty state invites creation, the editor opens with the
// v1 action allowlist only, and a server 422 surfaces verbatim. lib/api mocked at the module seam.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { WorkQueueBuilder } from "./WorkQueueBuilder";
import {
  ensureAuth, listEntities, listFields, listRoles, listWorkQueuesAdmin, createWorkQueue,
  updateWorkQueue, deleteWorkQueue, ApiError, type WorkQueue,
} from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(),
    listEntities: vi.fn(),
    listFields: vi.fn(),
    listRoles: vi.fn(),
    listWorkQueuesAdmin: vi.fn(),
    createWorkQueue: vi.fn(),
    updateWorkQueue: vi.fn(),
    deleteWorkQueue: vi.fn(),
  };
});

const QUEUES: WorkQueue[] = [
  {
    id: "q1", name: "overdue_tickets", label: "Ticket sắp trễ hạn", entityName: "tk_tickets",
    roleId: "role-sla", filters: [{ field: "due_date", op: "lte", value: "today" }],
    sortOrder: "asc", actionVerb: "open", position: 0, isActive: true,
  },
  {
    id: "q2", name: "old_queue", label: "Hàng đợi cũ", entityName: "crm_leads",
    filters: [], sortOrder: "asc", actionVerb: "open", position: 1, isActive: false,
  },
];

function renderScreen() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <WorkQueueBuilder />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(listWorkQueuesAdmin).mockReset().mockResolvedValue(QUEUES);
  vi.mocked(listEntities).mockReset().mockResolvedValue([
    { name: "tk_tickets", label: "Phiếu hỗ trợ", tableName: "obj_tk_tickets" },
    { name: "crm_leads", label: "Leads", tableName: "obj_crm_leads" },
  ]);
  vi.mocked(listRoles).mockReset().mockResolvedValue([{ id: "role-sla", name: "sla", label: "Đội SLA" }]);
  vi.mocked(listFields).mockReset().mockResolvedValue([
    { name: "due_date", label: "Hạn xử lý", fieldType: "date" },
  ]);
});

describe("WorkQueueBuilder", () => {
  it("renders queue rows with role visibility + active badges", async () => {
    renderScreen();
    expect(await screen.findByText("Ticket sắp trễ hạn")).toBeInTheDocument();
    expect(screen.getByText("Đội SLA")).toBeInTheDocument();     // role label resolved
    expect(screen.getByText("Mọi người")).toBeInTheDocument();   // null role
    expect(screen.getByText("Đang bật")).toBeInTheDocument();
    expect(screen.getByText("Đang tắt")).toBeInTheDocument();
  });

  it("empty state invites creating the first queue", async () => {
    vi.mocked(listWorkQueuesAdmin).mockResolvedValue([]);
    renderScreen();
    await waitFor(() => expect(document.body.textContent).toContain("Chưa có hàng đợi nào"));
  });

  it("editor exposes ONLY the v1 action allowlist and surfaces a server 422 verbatim", async () => {
    vi.mocked(createWorkQueue).mockRejectedValue(
      new ApiError(422, "WORKQUEUE_FILTER_INVALID", 'giá trị lọc ngày "xxx" không hợp lệ'),
    );
    renderScreen();
    fireEvent.click(await screen.findByText("Tạo hàng đợi"));

    // v1 allowlist: exactly the two deep-link actions, no generic action framework.
    const actionSelect = screen.getByText("Mở danh sách đã lọc").closest("select")!;
    expect(actionSelect.querySelectorAll("option")).toHaveLength(3);
    expect(screen.getByText("Mở hộp phê duyệt")).toBeInTheDocument();
    expect(screen.getByText("Nhận việc từ hàng đợi")).toBeInTheDocument();

    // Fill the minimum, save, and the backend's Vietnamese 422 shows verbatim.
    fireEvent.change(screen.getByPlaceholderText("Khách chưa gọi lại"), { target: { value: "Q" } });
    fireEvent.change(screen.getByPlaceholderText("uncalled_leads"), { target: { value: "q" } });
    // Entity is an SLDS combobox (autocomplete): open it, choose the option.
    fireEvent.click(screen.getByRole("combobox", { name: "Đối tượng" }));
    fireEvent.click(await screen.findByRole("option", { name: /Phiếu hỗ trợ/ }));
    fireEvent.click(screen.getByText("Lưu hàng đợi"));

    await waitFor(() =>
      expect(screen.getByText(/giá trị lọc ngày "xxx" không hợp lệ/)).toBeInTheDocument(),
    );
    // The panel scrolls and the button bar is pinned to its bottom: the error sits right above that
    // bar, so it is in view wherever the user scrolled to before pressing Lưu.
    const note = screen.getByText(/giá trị lọc ngày "xxx" không hợp lệ/).closest(".slds-scoped-notification")!;
    expect(note.nextElementSibling).toContainElement(screen.getByRole("button", { name: "Lưu hàng đợi" }));
  });

  it("verb 'Nhận việc': hides filters + sort (server refuses them) and saves claim with a clean body", async () => {
    vi.mocked(createWorkQueue).mockReset().mockResolvedValue(QUEUES[0]);
    renderScreen();
    fireEvent.click(await screen.findByText("Tạo hàng đợi"));
    fireEvent.change(screen.getByPlaceholderText("Khách chưa gọi lại"), { target: { value: "Chờ nhận" } });
    fireEvent.change(screen.getByPlaceholderText("uncalled_leads"), { target: { value: "pool" } });
    fireEvent.click(screen.getByRole("combobox", { name: "Đối tượng" }));
    fireEvent.click(await screen.findByRole("option", { name: /Phiếu hỗ trợ/ }));
    expect(screen.getByText("Điều kiện lọc (bản ghi nào vào hàng đợi)")).toBeInTheDocument();
    fireEvent.change(screen.getByText("Mở danh sách đã lọc").closest("select")!, { target: { value: "claim" } });
    expect(screen.queryByText("Điều kiện lọc (bản ghi nào vào hàng đợi)")).toBeNull();
    expect(screen.queryByText("Sắp xếp danh sách theo")).toBeNull();
    expect(screen.getByText(/đếm việc đang chờ nhận/)).toBeInTheDocument();
    fireEvent.click(screen.getByText("Lưu hàng đợi"));
    await waitFor(() => expect(createWorkQueue).toHaveBeenCalled());
    expect(vi.mocked(createWorkQueue).mock.calls[0][0]).toMatchObject({ actionVerb: "claim", filters: [], sortOrder: "asc" });
    expect(vi.mocked(createWorkQueue).mock.calls[0][0].sortField).toBeUndefined();
  });

  it("row menu Sửa opens the docked editor prefilled and saves the edit (PUT, code locked)", async () => {
    vi.mocked(updateWorkQueue).mockResolvedValue(QUEUES[0]);
    renderScreen();
    await screen.findByRole("grid", { name: "Danh sách hàng đợi" });
    (await screen.findByRole("button", { name: "Thêm thao tác cho Ticket sắp trễ hạn" })).focus();
    await userEvent.keyboard("{Enter}");
    await userEvent.click(screen.getByRole("menuitem", { name: "Sửa" }));

    expect(await screen.findByRole("heading", { name: "Sửa hàng đợi" })).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Khách chưa gọi lại")).toHaveValue("Ticket sắp trễ hạn");
    expect(screen.getByPlaceholderText("uncalled_leads")).toBeDisabled();
    fireEvent.change(screen.getByPlaceholderText("Khách chưa gọi lại"), { target: { value: "Ticket gấp" } });
    fireEvent.click(screen.getByText("Lưu hàng đợi"));
    await waitFor(() => expect(updateWorkQueue).toHaveBeenCalledTimes(1));
    expect(vi.mocked(updateWorkQueue).mock.calls[0][0]).toBe("q1");
    expect(vi.mocked(updateWorkQueue).mock.calls[0][1]).toMatchObject({
      label: "Ticket gấp", name: "overdue_tickets", entityName: "tk_tickets",
      filters: [{ field: "due_date", op: "lte", value: "today" }],
    });
  });

  it("row menu Xoá asks in the page (no dialog), then deletes", async () => {
    vi.mocked(deleteWorkQueue).mockResolvedValue(undefined);
    renderScreen();
    await screen.findByRole("grid", { name: "Danh sách hàng đợi" });
    (await screen.findByRole("button", { name: "Thêm thao tác cho Hàng đợi cũ" })).focus();
    await userEvent.keyboard("{Enter}");
    await userEvent.click(screen.getByRole("menuitem", { name: "Xoá" }));
    expect(await screen.findByText(/“Hàng đợi cũ” sẽ biến mất/)).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Xoá" }));
    await waitFor(() => expect(deleteWorkQueue).toHaveBeenCalledWith("q2"));
  });

  // panels/base + modals/base docs: focus returns to what opened them. From the row menu that is the
  // row's ▾ button — the menu item is gone by the time the panel or modal closes.
  describe("focus return from the row menu", () => {
    const settle = () => act(() => new Promise((r) => setTimeout(r, 20)));
    async function openRowMenu(label: string, item: string) {
      await screen.findByRole("grid", { name: "Danh sách hàng đợi" });
      const menuButton = await screen.findByRole("button", { name: `Thêm thao tác cho ${label}` });
      menuButton.focus();
      await userEvent.keyboard("{Enter}");
      await userEvent.click(screen.getByRole("menuitem", { name: item }));
      await settle();
      return menuButton;
    }

    it("Sửa: focus goes into the editor, and back to the row's menu button when it closes", async () => {
      renderScreen();
      const menuButton = await openRowMenu("Ticket sắp trễ hạn", "Sửa");
      const panel = screen.getByRole("heading", { name: "Sửa hàng đợi" }).closest(".slds-panel") as HTMLElement;
      expect(panel).toContainElement(document.activeElement as HTMLElement);
      await userEvent.click(within(panel).getByRole("button", { name: "Thu gọn Sửa hàng đợi" }));
      await settle();
      expect(document.activeElement).toBe(menuButton);
    });

    it("Xoá → Huỷ: the question leaves the page and nothing was deleted", async () => {
      vi.mocked(deleteWorkQueue).mockClear();
      renderScreen();
      await openRowMenu("Hàng đợi cũ", "Xoá");
      await userEvent.click(await screen.findByRole("button", { name: "Huỷ" }));
      await settle();
      expect(screen.queryByText(/sẽ biến mất/)).toBeNull();
      expect(deleteWorkQueue).not.toHaveBeenCalled();
    });
  });

  it("the buttons that open the editor control its panel (aria-controls + aria-expanded)", async () => {
    const { container } = renderScreen();
    const open = await screen.findByRole("button", { name: "Ticket sắp trễ hạn" });
    const create = screen.getByRole("button", { name: "Tạo hàng đợi" });
    const panel = container.querySelector(".slds-panel")!;
    expect(panel.id).not.toBe("");
    expect(open).toHaveAttribute("aria-controls", panel.id);
    expect(create).toHaveAttribute("aria-controls", panel.id);
    expect(open).toHaveAttribute("aria-expanded", "false");
    expect(create).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(open);
    expect(open).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByRole("button", { name: "Hàng đợi cũ" })).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(create);
    expect(create).toHaveAttribute("aria-expanded", "true");
    expect(open).toHaveAttribute("aria-expanded", "false");
  });

  it("opening a row by its name and collapsing the panel returns focus to that name", async () => {
    renderScreen();
    const open = await screen.findByRole("button", { name: "Ticket sắp trễ hạn" });
    await userEvent.click(open);
    await userEvent.click(screen.getByRole("button", { name: "Thu gọn Sửa hàng đợi" }));
    expect(document.activeElement).toBe(open);
  });
});

// specs/074 lát f2-automation-approval-queue: the scope is a prop of the page (`?app=` / `?bang=`), the open queue
// follows the URL, and a queue of another app's table is never opened.
describe("WorkQueueBuilder under a page scope", () => {
  const render2 = (props: Parameters<typeof WorkQueueBuilder>[0]) => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(<QueryClientProvider client={qc}><WorkQueueBuilder {...props} /></QueryClientProvider>);
  };

  it("an app with no queue says so; the other app's queues are neither listed nor openable", async () => {
    render2({ entityNames: new Set(["hr_staff"]), initialQueueId: "q1" });
    await waitFor(() => expect(document.body.textContent).toContain("Chưa có hàng đợi nào"));
    expect(screen.queryByText("Ticket sắp trễ hạn")).toBeNull();
    expect(screen.queryByRole("heading", { name: "Sửa hàng đợi" })).toBeNull();
  });

  it("initialQueueId opens that queue and onSelect reports it; closing reports null", async () => {
    const onSelect = vi.fn();
    render2({ entityNames: new Set(["tk_tickets"]), initialQueueId: "q1", onSelect });
    expect(await screen.findByRole("heading", { name: "Sửa hàng đợi" })).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Khách chưa gọi lại")).toHaveValue("Ticket sắp trễ hạn");
    await waitFor(() => expect(onSelect).toHaveBeenLastCalledWith("q1"));
    expect(screen.queryByText("Hàng đợi cũ")).toBeNull(); // crm_leads is out of scope
    await userEvent.click(screen.getByRole("button", { name: "Thu gọn Sửa hàng đợi" }));
    await waitFor(() => expect(onSelect).toHaveBeenLastCalledWith(null));
  });

  it("an id that does not exist opens nothing", async () => {
    render2({ initialQueueId: "nope" });
    await screen.findByText("Ticket sắp trễ hạn");
    expect(screen.queryByRole("heading", { name: "Sửa hàng đợi" })).toBeNull();
  });

  it("?bang= narrows to one table", async () => {
    render2({ entity: "crm_leads" });
    expect(await screen.findByText("Hàng đợi cũ")).toBeInTheDocument();
    expect(screen.queryByText("Ticket sắp trễ hạn")).toBeNull();
  });
});
