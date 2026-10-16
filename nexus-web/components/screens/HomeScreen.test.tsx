// HomeScreen (/home) on SLDS — cards + tiles + data-table in a card + badges + illustration
// (specs/038 T028). The widgets and their data paths are unchanged; this pins what each widget
// must still let the user do: open a queue / an approval / a recently viewed record / a pinned
// list or its "new" form / an entity list from its count / a recently updated record / a dashboard.
// Module seams mocked: lib/api (4 reads), metadata hooks, auth, active app, pins, recents.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { HomeScreen } from "./HomeScreen";
import {
  listRecordsPage, listDashboards, listPendingApprovals, listWorkQueues, workItemCounts,
  type ApprovalItem, type WorkQueueView,
} from "@/lib/api";

const auth = vi.hoisted(() => ({ user: { id: "u1", fullName: "Trần Sales Rep", email: "test@example.com", isAdmin: false } }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => auth }));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    listRecordsPage: vi.fn(),
    listDashboards: vi.fn(),
    listPendingApprovals: vi.fn(),
    listWorkQueues: vi.fn(),
    workItemCounts: vi.fn(),
  };
});

// Stable references, like react-query's: a fresh array per render would re-fire the screen's
// entity-keyed effects forever.
const meta = vi.hoisted(() => ({
  entities: [
    { name: "crm_accounts", label: "Khách hàng" },
    { name: "crm_contacts", label: "Liên hệ" },
  ],
  fields: [{ name: "name", label: "Tên", fieldType: "text" }],
}));
vi.mock("@/lib/metadata/hooks", () => ({
  useEntities: () => ({ data: meta.entities }),
  useFields: () => ({ data: meta.fields, isLoading: false }),
}));

vi.mock("@/lib/apps", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/apps")>();
  return { ...actual, useActiveApp: () => ({ activeApp: actual.ALL_ITEMS, activeNav: null }) };
});

const pins = vi.hoisted(() => ({ pins: ["crm_accounts"] as string[] }));
vi.mock("@/lib/pins", () => ({ usePins: () => ({ pins: pins.pins, toggle: vi.fn() }) }));

vi.mock("@/lib/recents", () => ({
  pruneRecentRecords: () => [{ entity: "crm_accounts", id: "r9", label: "Acme Corp", at: Date.now() - 5 * 60_000 }],
  onRecentsChanged: () => () => undefined,
}));

const approval = (i: number): ApprovalItem =>
  ({ ID: `ap${i}`, TenantID: "t", EntityName: "crm_quotes", RecordID: `rec${i}xxxxxx`, CurrentStep: 0, Status: "pending" }) as ApprovalItem;
const queue = (over: Partial<WorkQueueView>): WorkQueueView => ({
  id: "q1", name: "new_leads", label: "Lead mới chưa xử lý", entityName: "crm_leads", actionVerb: "open",
  count: 2, href: "/crm_leads?queue=q1", ...over,
});

function page(entity: string) {
  const total = entity === "crm_accounts" ? 36 : 19; // accounts = hero (most records)
  return {
    records: entity === "crm_accounts" ? [{ id: "a1", version: 1, data: { name: "Tập đoàn Tinh Vân" }, updatedAt: "2026-09-20T03:00:00Z" }] : [],
    pagination: { page: 1, limit: 1, total, totalPages: total },
  };
}

function renderHome() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <HomeScreen />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  auth.user.isAdmin = false;
  pins.pins = ["crm_accounts"];
  vi.mocked(listRecordsPage).mockImplementation(async (entity: string) => page(entity) as never);
  vi.mocked(listDashboards).mockResolvedValue([]);
  vi.mocked(listPendingApprovals).mockResolvedValue([]);
  vi.mocked(workItemCounts).mockResolvedValue({ mine: 0, pool: 0, overdue: 0 });
  vi.mocked(listWorkQueues).mockResolvedValue([queue({}), queue({ id: "q2", label: "Hàng đợi lỗi", count: null, href: "/crm_leads?queue=q2" })]);
});

describe("HomeScreen (SLDS)", () => {
  it("is built from SLDS cards with no Nexus Be tokens", async () => {
    const { container } = renderHome();
    expect(await screen.findByRole("heading", { level: 1 })).toHaveTextContent("Rep");
    await screen.findByText("Lead mới chưa xử lý");
    expect(container.querySelectorAll("article.slds-card").length).toBeGreaterThanOrEqual(5);
    expect(container.innerHTML).not.toMatch(/\b(bg-panel2?|text-fg|border-bd|text-fnt|text-mut|rounded-card|bg-acd|text-ac-hi|bg-wnd|animate-pulse)\b/);
  });

  it("Việc của tôi: queue rows deep-link, an uncountable queue shows —, approvals past 5 link to the inbox", async () => {
    vi.mocked(listPendingApprovals).mockResolvedValue([1, 2, 3, 4, 5, 6].map(approval));
    renderHome();
    const q = (await screen.findByText("Lead mới chưa xử lý")).closest("a")!;
    expect(q).toHaveAttribute("href", "/crm_leads?queue=q1");
    expect(within(screen.getByText("Hàng đợi lỗi").closest("li")!).getByText("—")).toBeInTheDocument();
    expect(await screen.findByRole("link", { name: "Xem tất cả 6 yêu cầu" })).toHaveAttribute("href", "/approvals");
    expect(screen.getAllByText(/Phê duyệt · crm_quotes/)).toHaveLength(5);
  });

  it("Việc của tôi: the work-item row shows own / claimable / overdue counts and opens the inbox", async () => {
    vi.mocked(workItemCounts).mockResolvedValue({ mine: 3, pool: 2, overdue: 1 });
    renderHome();
    const row = (await screen.findByRole("link", { name: /Việc của tôi/ }));
    expect(row).toHaveAttribute("href", "/approvals");
    expect(within(row.closest("li")!).getByText(/3 việc của bạn/)).toBeInTheDocument();
    expect(within(row.closest("li")!).getByText(/2 chờ nhận/)).toBeInTheDocument();
    expect(within(row.closest("li")!).getByText("1 quá hạn")).toBeInTheDocument();
  });

  it("Vừa xem: a recently viewed record links to its detail page", async () => {
    renderHome();
    const link = await screen.findByRole("link", { name: "Acme Corp" });
    expect(link).toHaveAttribute("href", "/crm_accounts/r9");
    expect(within(link.closest("li")!).getByText(/5 phút trước/)).toBeInTheDocument();
  });

  it("Lối tắt: a pinned entity opens its list and has a 'new' shortcut", async () => {
    renderHome();
    const create = await screen.findByRole("link", { name: "Tạo Khách hàng mới" });
    expect(create).toHaveAttribute("href", "/crm_accounts/new");
    expect(create.closest(".slds-button-group")).not.toBeNull();
  });

  it("Lối tắt: with no pins, explains how to pin", async () => {
    pins.pins = [];
    renderHome();
    expect(await screen.findByText(/Chưa ghim đối tượng nào/)).toBeInTheDocument();
  });

  it("KPI: one tile per entity with the real count, linking to the list", async () => {
    renderHome();
    const tile = (await screen.findByRole("link", { name: "Tổng Khách hàng" })).closest("article")!;
    expect(tile).toHaveClass("slds-tile");
    expect(within(tile).getByText("36")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Tổng Khách hàng" })).toHaveAttribute("href", "/crm_accounts");
  });

  it("Cập nhật gần đây: the hero entity's latest records in a table, each linking to the record", async () => {
    renderHome();
    const row = await screen.findByRole("link", { name: "Tập đoàn Tinh Vân" });
    expect(row).toHaveAttribute("href", "/crm_accounts/a1");
    expect(row.closest("table")).toHaveClass("slds-table");
    // The "(n)" is the entity's record total, not the rows the limit=6 query happened to return.
    const card = row.closest("article")!;
    expect(within(card).getByRole("heading", { level: 2 })).toHaveTextContent(/\(36\)$/);
  });

  it("Bảng điều khiển: empty tells a non-admin who creates them, gives an admin the link", async () => {
    const { unmount } = renderHome();
    expect(await screen.findByText(/Quản trị viên có thể tạo và cấp cho bạn/)).toBeInTheDocument();
    unmount();
    auth.user.isAdmin = true;
    renderHome();
    expect(await screen.findByRole("link", { name: "Tạo bảng điều khiển" })).toHaveAttribute("href", "/dashboards");
  });

  it("Bảng điều khiển: lists catalog dashboards linking to the viewer", async () => {
    vi.mocked(listDashboards).mockResolvedValue([{ id: "d1", name: "CRM Overview" }] as never);
    renderHome();
    expect(await screen.findByRole("link", { name: "CRM Overview" })).toHaveAttribute("href", "/dashboards/d1");
  });
});
