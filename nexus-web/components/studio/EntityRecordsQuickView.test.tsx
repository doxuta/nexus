// Hub tab "Dữ liệu" — a read-only glance at the table's first rows through the SAME metadata →
// columns pipeline as the App-face list, in an SLDS data table inside a related-list card.
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { id: "u1", isAdmin: true } }) }));

const listRecordsPage = vi.fn();
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  listFields: vi.fn().mockResolvedValue([
    { id: "f1", name: "name", label: "Tên công ty", fieldType: "text" },
    { id: "f2", name: "phone", label: "Điện thoại", fieldType: "text" },
  ]),
  getLayout: vi.fn().mockResolvedValue({ entityName: "crm_accounts", views: { list: { columns: [{ field: "name" }, { field: "phone" }] } } }),
  listRecordsPage: (...a: unknown[]) => listRecordsPage(...a),
  listUserDirectory: vi.fn().mockResolvedValue([]),
}));

import { EntityRecordsQuickView } from "./EntityRecordsQuickView";
import { clearWorkspace, setWorkspace } from "@/lib/workspace";

function renderView() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <EntityRecordsQuickView entityName="crm_accounts" />
    </QueryClientProvider>,
  );
}

beforeEach(() => vi.clearAllMocks());

describe("EntityRecordsQuickView", () => {
  it("shows the first rows in a data table with the layout's list columns and the real total", async () => {
    listRecordsPage.mockResolvedValue({
      records: [
        { id: "r1", version: 1, data: { name: "Acme", phone: "0901" } },
        { id: "r2", version: 1, data: { name: "Globex", phone: "0902" } },
      ],
      pagination: { page: 1, limit: 20, total: 57, totalPages: 3 },
    });
    renderView();
    const grid = await screen.findByRole("grid", { name: "Xem nhanh bản ghi của crm_accounts" });
    expect(await within(grid).findByText("Acme")).toBeInTheDocument();
    expect(within(grid).getByRole("columnheader", { name: /Điện thoại/ })).toBeInTheDocument();
    expect(screen.getByText(/57 bản ghi/)).toBeInTheDocument();
    expect(screen.getByText(/xem trước 20/)).toBeInTheDocument();
    expect(listRecordsPage).toHaveBeenCalledWith("crm_accounts", "limit=20");
  });

  // specs/073 kh4 T273: Studio opens no new tab (FR-133) — outside a session too.
  it("links to the full list on the App face in the same tab", async () => {
    listRecordsPage.mockResolvedValue({ records: [], pagination: { page: 1, limit: 20, total: 0, totalPages: 0 } });
    renderView();
    const link = await screen.findByRole("link", { name: /Mở đầy đủ ở App khách/ });
    expect(link).toHaveAttribute("href", "/crm_accounts");
    expect(link).not.toHaveAttribute("target");
    expect(link).not.toHaveAttribute("rel");
  });

  // specs/062 L2 (D-20): the full list opened in a new tab would run on the builder's HOME login.
  it("in a workspace session the full-list link opens in the same tab", async () => {
    setWorkspace({
      accessToken: "WS", expiresAt: "2099-01-01T00:00:00Z", homeUserId: "u-1",
      session: { id: "ws-1", tenantId: "t-2", tenantSlug: "x", tenantName: "Khách X", openedAt: "2098-12-31T00:00:00Z", expiresAt: "2099-01-01T08:00:00Z" },
    });
    try {
      listRecordsPage.mockResolvedValue({ records: [], pagination: { page: 1, limit: 20, total: 0, totalPages: 0 } });
      renderView();
      const link = await screen.findByRole("link", { name: /Mở đầy đủ ở App khách/ });
      expect(link).toHaveAttribute("href", "/crm_accounts");
      expect(link).not.toHaveAttribute("target");
      expect(link).not.toHaveAttribute("rel");
    } finally {
      clearWorkspace();
    }
  });

  it("an empty table says so", async () => {
    listRecordsPage.mockResolvedValue({ records: [], pagination: { page: 1, limit: 20, total: 0, totalPages: 0 } });
    renderView();
    expect(await screen.findByText("Chưa có bản ghi nào")).toBeInTheDocument();
  });

  it("a failed load shows the error instead of an empty grid", async () => {
    listRecordsPage.mockRejectedValue(new Error("RECORDS_FORBIDDEN"));
    renderView();
    expect(await screen.findByText(/RECORDS_FORBIDDEN/)).toBeInTheDocument();
    expect(screen.queryByRole("grid")).not.toBeInTheDocument();
  });
});

describe("EntityRecordsQuickView — ask the assistant (specs/076 ai6)", () => {
  const rows = { records: [{ id: "rec-1", version: 1, data: { name: "Acme", phone: "0901" } }], pagination: { total: 1 } };
  function renderWith(onAsk?: (id: string) => void) {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={qc}>
        <EntityRecordsQuickView entityName="crm_accounts" onAskAssistant={onAsk} />
      </QueryClientProvider>,
    );
  }
  it("with the prop each row has a 'Hỏi trợ lý' button that passes the row id only", async () => {
    listRecordsPage.mockResolvedValue(rows);
    const onAsk = vi.fn();
    renderWith(onAsk);
    const btn = await screen.findByRole("button", { name: "Hỏi trợ lý về bản ghi này" });
    btn.click();
    expect(onAsk).toHaveBeenCalledWith("rec-1");
  });
  it("without the prop there is no such button", async () => {
    listRecordsPage.mockResolvedValue(rows);
    renderWith();
    await screen.findByText("Acme");
    expect(screen.queryByRole("button", { name: /Hỏi trợ lý/ })).toBeNull();
  });
});
