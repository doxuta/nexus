// RelatedTab — ENG-RELVIEW rendering contract:
//   • a parent-side 1-N relationship WITH viewConfig renders the configured label, the
//     configured COLUMNS as a table (headers from child field metas), the "+ Tạo" link
//     prefilled with the parent FK (?pf_<field>=<parentId>), and passes viewConfig.sort to
//     listChildRecords;
//   • a relationship WITHOUT viewConfig keeps the pre-RELVIEW fallback rendering: entity-label
//     header, recordLabel rows (no table). It DOES show the "+ Tạo" link — creating a child born
//     linked to the parent now defaults ON (opt-out per-relationship via allowCreate: false).
// lib/api is mocked at the module seam (same pattern as VerticalLibrary.test.tsx); the
// section components, byViewOrder and the table JSX are all real code.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RelatedTab } from "./LayoutDetail";
import {
  ensureAuth, listAllRelationships, listEntities, listChildRecords, listFields, listRelationships,
  type AdminRelationship, type ApiRecord,
} from "@/lib/api";

// STUDIO-DISCOVERABILITY 1.4c: the quick-create slide-over is stubbed at the module seam — the
// wiring under test is LayoutDetail's (click intercept → open with entity+FK prefill), not the
// form engine inside the modal (covered by QuickCreateModal.test.tsx / LayoutForm).
vi.mock("@/components/layout/QuickCreateModal", () => ({
  QuickCreateModal: ({ entity, open, prefill }: { entity: string; open: boolean; prefill?: Record<string, unknown> }) =>
    open ? <div data-testid="quick-create-modal" data-entity={entity} data-prefill={JSON.stringify(prefill ?? null)} /> : null,
}));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual, // keeps ApiError, isRelationRef etc. real
    ensureAuth: vi.fn(),
    listAllRelationships: vi.fn(),
    listEntities: vi.fn(),
    listChildRecords: vi.fn(),
    listFields: vi.fn(),
    listRelationships: vi.fn(),
  };
});

const CONFIGURED_REL: AdminRelationship = {
  id: "rel-orders",
  name: "orders_customer",
  parentEntityName: "customers",
  childEntityName: "orders",
  relationshipType: "1-N",
  childFkColumn: "customer_id",
  viewConfig: {
    label: "Đơn hàng của khách",
    columns: ["total"],
    sort: { field: "created_at", dir: "desc" },
    allowCreate: true,
  },
};

const PLAIN_REL: AdminRelationship = {
  id: "rel-tickets",
  name: "tickets_customer",
  parentEntityName: "customers",
  childEntityName: "tickets",
  relationshipType: "1-N",
  childFkColumn: "customer_id",
  // no viewConfig → byte-identical fallback rendering
};

const ORDER_RECORDS: ApiRecord[] = [
  { id: "o1", version: 1, data: { total: 150000, name: "Đơn A" } } as unknown as ApiRecord,
];
const TICKET_RECORDS: ApiRecord[] = [
  { id: "t1", version: 1, data: { name: "Ticket B" } } as unknown as ApiRecord,
];

// RelatedTab runs its own useQuery calls (the reverse polymorphic-timeline discovery), so it needs
// a provider exactly like LayoutDetail.print.test.tsx does. The mocked field metas below are
// number/text only → no polymorphic source is discovered → the timeline stays off in these tests
// and the ENG-RELVIEW assertions keep testing only what they name.
function renderTab() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <RelatedTab entity="customers" recordId="p1" allFields={[]} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(listAllRelationships).mockReset().mockResolvedValue([CONFIGURED_REL, PLAIN_REL]);
  vi.mocked(listEntities).mockReset().mockResolvedValue([
    { name: "orders", label: "Đơn hàng", tableName: "obj_orders" },
    { name: "tickets", label: "Phiếu hỗ trợ", tableName: "obj_tickets" },
  ]);
  vi.mocked(listFields).mockReset().mockResolvedValue([
    { name: "total", label: "Tổng tiền", fieldType: "number" },
    { name: "name", label: "Tên đơn", fieldType: "text" },
  ]);
  vi.mocked(listChildRecords).mockReset().mockImplementation(async (childEntity: string) =>
    childEntity === "orders"
      ? { records: ORDER_RECORDS, total: 1 }
      : { records: TICKET_RECORDS, total: 1 },
  );
});

describe("RelatedTab — ENG-RELVIEW viewConfig", () => {
  it("renders the configured label, columns table and + Tạo prefill link", async () => {
    renderTab();

    // Configured panel title (NOT the raw entity label); the SLDS card title carries the count.
    expect(await screen.findByText(/^Đơn hàng của khách/)).toBeInTheDocument();

    // Configured column header comes from the child field meta label.
    expect(await screen.findByText("Tổng tiền")).toBeInTheDocument();
    // Table cell renders the record's column value through FieldDisplay.
    expect(screen.getByRole("table")).toBeInTheDocument();

    // "+ Tạo" opens the child create form PREFILLED with the parent FK.
    const createLink = screen.getByTitle("Tạo Đơn hàng của khách mới, gắn sẵn vào bản ghi này");
    expect(createLink).toHaveAttribute("href", "/orders/new?pf_customer=p1");

    // queue #304: "Thêm nhiều" opens the multi-row grid with every row already linked to this parent.
    expect(screen.getAllByRole("link", { name: "Thêm nhiều" }).map((l) => l.getAttribute("href"))).toContain("/orders/batch?pf_customer=p1");

    // viewConfig.sort is passed through to the child list call; 6th arg = DP-2.1 expand list
    // (the child's relation fields minus the FK back to this parent).
    expect(vi.mocked(listChildRecords)).toHaveBeenCalledWith(
      "orders", "customer", "p1", expect.any(Number), { field: "created_at", dir: "desc" },
      expect.any(Array),
    );
  });

  it("opens the quick-create slide-over with the parent FK prefilled on plain click (href kept as fallback)", async () => {
    renderTab();

    const createLink = await screen.findByTitle("Tạo Đơn hàng của khách mới, gắn sẵn vào bản ghi này");
    // Full-page fallback stays on the anchor (middle/cmd-click, no-JS).
    expect(createLink).toHaveAttribute("href", "/orders/new?pf_customer=p1");
    expect(screen.queryByTestId("quick-create-modal")).toBeNull();

    fireEvent.click(createLink!);

    const modal = screen.getByTestId("quick-create-modal");
    expect(modal).toHaveAttribute("data-entity", "orders");
    expect(modal).toHaveAttribute("data-prefill", JSON.stringify({ customer: "p1" }));
  });

  it("falls back to the default rendering when viewConfig is absent", async () => {
    renderTab();

    // Fallback panel header = child entity label from metadata (SLDS card title: "Label (count)").
    expect(await screen.findByText(/^Phiếu hỗ trợ/)).toBeInTheDocument();
    // Fallback rows use recordLabel — the ticket's VIP label shows as a plain row link.
    expect(await screen.findByText("Ticket B")).toBeInTheDocument();

    // No sort param for the unconfigured relationship; expand list still passed (DP-2.1).
    expect(vi.mocked(listChildRecords)).toHaveBeenCalledWith(
      "tickets", "customer", "p1", expect.any(Number), undefined, expect.any(Array),
    );
    // Exactly ONE table on the page — the unconfigured panel must NOT render as a table.
    expect(screen.getAllByRole("table")).toHaveLength(1);
    // BOTH 1-N panels show "+ Tạo": it now defaults ON (a related list should let you add a child
    // born linked to the parent). An admin opts out per-relationship with allowCreate: false.
    expect(screen.getAllByText("Tạo")).toHaveLength(2);
  });
});

describe("RelatedTab — forward relation tiles (#272)", () => {
  it("names a tile by the target table's isLabel field, never the UUID, and never flashes a guess first", async () => {
    vi.mocked(listAllRelationships).mockResolvedValue([]);
    vi.mocked(listRelationships).mockReset().mockResolvedValue([
      { id: "11111111-2222-3333-4444-555555555555", version: 1, data: { code: "BN-01", ho_ten: "Nguyễn Văn An" } } as unknown as ApiRecord,
    ]);
    vi.mocked(listFields).mockReset().mockResolvedValue([
      { name: "code", label: "Mã", fieldType: "text", isRequired: true },
      { name: "ho_ten", label: "Họ tên", fieldType: "text", isLabel: true },
    ] as never);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <RelatedTab
          entity="lich_hen"
          recordId="p1"
          allFields={[{ name: "benh_nhan", label: "Bệnh nhân", fieldType: "relation", options: { relatedEntity: "benh_nhan" } }] as never}
        />
      </QueryClientProvider>,
    );
    expect(await screen.findByText("Nguyễn Văn An")).toBeInTheDocument();
    expect(screen.queryByText("BN-01")).toBeNull();
  });

  it("#479: the relation's own labelField beats an inferred isLabel (server expand order)", async () => {
    vi.mocked(listAllRelationships).mockResolvedValue([]);
    vi.mocked(listRelationships).mockReset().mockResolvedValue([
      { id: "11111111-2222-3333-4444-555555555555", version: 1, data: { code: "BN-01", ho_ten: "Nguyễn Văn An" } } as unknown as ApiRecord,
    ]);
    vi.mocked(listFields).mockReset().mockResolvedValue([
      { name: "code", label: "Mã", fieldType: "text", isRequired: true },
      { name: "ho_ten", label: "Họ tên", fieldType: "text", isLabel: true },
    ] as never);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <RelatedTab
          entity="lich_hen"
          recordId="p1"
          allFields={[{ name: "benh_nhan", label: "Bệnh nhân", fieldType: "relation", options: { relatedEntity: "benh_nhan", labelField: "code" } }] as never}
        />
      </QueryClientProvider>,
    );
    expect(await screen.findByText("BN-01")).toBeInTheDocument();
    expect(screen.queryByText("Nguyễn Văn An")).toBeNull();
  });
});
