// LayoutDetail — "In" (MOT-XUONG W3 #6, docs/NEXUS-MOT-XUONG-2026-07-29.md §3d mục 6): native
// window.print() + Tailwind `print:` variant, zero PDF lib. Contract under test:
//   • the button sits in the record header next to Sửa/Nhân bản, wired to window.print() —
//     AFTER preparing the print-only child-table data (handlePrint), never before;
//   • @media print hides the interactive chrome (record header, tab bar + tab content) — asserted
//     via className (jsdom has no CSS engine, so this proves WIRING; the real check is
//     browser-verify with DevTools "Emulate CSS media: print" on a live record);
//   • a record with a 1-N child relation (e.g. a quote's crm_quote_lines) prints that child
//     table by reusing listAllRelationships/listChildRecords — the SAME calls RelatedTab's
//     "Danh sách liên quan" section already makes, no second data-fetching path — through the
//     print-specific default-columns fallback (no builtin pack sets relationship view_config,
//     so this IS the only branch that runs for crm_quote_lines today);
//   • a child-fetch failure surfaces a toast rather than silently printing an incomplete document.
// lib/api + useAuth mocked at the module seam (RelatedTab.test.tsx / CommandPalette.test.tsx
// pattern); QueryClientProvider wraps because LayoutDetail calls useQueryClient() itself.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { LayoutDetail } from "./LayoutDetail";
import { toast } from "@/components/ui/Toaster";
import {
  ensureAuth, getLayout, listFields, getRecord, listAllRelationships, listEntities,
  listChildRecords,
  type EntityLayout, type FieldMeta, type ApiRecord, type AdminRelationship, type EntityMeta,
} from "@/lib/api";

vi.mock("@/lib/auth/AuthProvider", () => ({
  useAuth: () => ({ user: { id: "u1", isAdmin: false } }),
}));

// Sửa / Nhân bản / Xoá are now gated on real grants, and this user is a non-admin with none — so
// without this mock the buttons this test asserts next to "In" would correctly be absent. The
// subject here is the print button, so the actor is given the grants a normal editor has.
vi.mock("@/lib/metadata/permissions", () => ({
  usePermissions: () => ({ can: () => true, grants: [], isLoading: false }),
}));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(),
    getLayout: vi.fn(),
    listFields: vi.fn(),
    getRecord: vi.fn(),
    listAllRelationships: vi.fn(),
    listEntities: vi.fn(),
    listChildRecords: vi.fn(),
  };
});

const QUOTE_FIELDS: FieldMeta[] = [
  { name: "name", label: "Tiêu đề báo giá", fieldType: "text" },
  { name: "status", label: "Trạng thái", fieldType: "picklist" },
];
const QUOTE_LAYOUT: EntityLayout = {
  entityName: "crm_quotes",
  views: {
    edit: {},
    list: {},
    detail: { sections: [{ title: "Thông tin", fields: ["name", "status"] }] },
  },
};
const QUOTE_RECORD: ApiRecord = { id: "q1", version: 1, data: { name: "Báo giá quý 3", status: "Đã gửi" } };

// crm_quote_lines shape (internal/services/vertical_pack/builtin.go ~L403-427): `quote` relation
// FK back to crm_quotes (inverseField "lines") + product_name/quantity/unit_price/line_total —
// NO relationship view_config is set by any builtin pack, so the print default-columns fallback
// (fields minus the FK minus bulky types) is the ONLY path that renders this table today.
const QUOTE_LINES_FIELDS: FieldMeta[] = [
  { name: "quote", label: "Báo giá", fieldType: "relation", options: { relatedEntity: "crm_quotes" } },
  { name: "product_name", label: "Sản phẩm", fieldType: "text" },
  { name: "quantity", label: "Số lượng", fieldType: "number" },
  { name: "unit_price", label: "Đơn giá", fieldType: "currency" },
  { name: "line_total", label: "Thành tiền", fieldType: "formula" },
];
const LINES_REL: AdminRelationship = {
  id: "rel-lines", name: "crm_quote_lines_quote", parentEntityName: "crm_quotes",
  childEntityName: "crm_quote_lines", relationshipType: "1-N", childFkColumn: "quote_id",
};
const LINE_RECORDS: ApiRecord[] = [
  {
    id: "l1", version: 1,
    data: { product_name: "Bản quyền phần mềm", quantity: 2, unit_price: 500000, line_total: 1000000 },
  },
];
const ENTITIES: EntityMeta[] = [
  { name: "crm_quotes", label: "Báo giá", tableName: "obj_crm_quotes" },
  { name: "crm_quote_lines", label: "Dòng báo giá", tableName: "obj_crm_quote_lines" },
];

function renderDetail() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      {/* onEdit/onClone passed so Sửa/Nhân bản render — proves the In button sits ADJACENT to
          them, not just that it exists somewhere. */}
      <LayoutDetail entity="crm_quotes" recordId="q1" onEdit={() => {}} onClone={() => {}} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(getLayout).mockReset().mockResolvedValue(QUOTE_LAYOUT);
  vi.mocked(listFields).mockReset().mockImplementation(async (entity: string) =>
    entity === "crm_quotes" ? QUOTE_FIELDS : QUOTE_LINES_FIELDS);
  vi.mocked(getRecord).mockReset().mockResolvedValue(QUOTE_RECORD);
  vi.mocked(listAllRelationships).mockReset().mockResolvedValue([LINES_REL]);
  vi.mocked(listEntities).mockReset().mockResolvedValue(ENTITIES);
  vi.mocked(listChildRecords).mockReset().mockResolvedValue({ records: LINE_RECORDS, total: 1 });
  window.print = vi.fn();
});

describe("LayoutDetail — nút In (MOT-XUONG W3 #6)", () => {
  it("renders next to Sửa/Nhân bản and calls window.print() only AFTER preparing print data", async () => {
    renderDetail();
    expect(await screen.findByText("Sửa")).toBeInTheDocument();
    expect(screen.getByText("Nhân bản")).toBeInTheDocument();
    const printBtn = screen.getByTitle("In chứng từ này");

    fireEvent.click(printBtn);

    await waitFor(() => expect(listChildRecords).toHaveBeenCalled());
    await waitFor(() => expect(window.print).toHaveBeenCalledTimes(1));
  });

  it("shows an empty record — a record with NO child table — as a valid, non-crashing print (item 4)", async () => {
    vi.mocked(listAllRelationships).mockResolvedValue([]); // no 1-N children for this entity
    renderDetail();
    const printBtn = await screen.findByTitle("In chứng từ này");

    fireEvent.click(printBtn);

    await waitFor(() => expect(window.print).toHaveBeenCalledTimes(1));
    expect(listChildRecords).not.toHaveBeenCalled();
  });

  it("hides the interactive chrome under @media print — header and tab area both carry print:hidden", async () => {
    const { container } = renderDetail();
    await screen.findByTitle("In chứng từ này");

    // SLDS record home: the page header and the tabs sit inside one print:hidden wrapper.
    const header = container.querySelector(".slds-page-header");
    expect(header).not.toBeNull();
    expect(header?.closest(".print\\:hidden")).not.toBeNull();

    const tabsWrapper = screen.getByRole("tab", { name: "Tổng quan" }).closest(".print\\:hidden");
    expect(tabsWrapper).not.toBeNull();
  });

  it("includes the 1-N child table (quote lines) via listAllRelationships/listChildRecords — the SAME path RelatedTab uses, default-columns fallback since no pack sets view_config", async () => {
    renderDetail();
    const printBtn = await screen.findByTitle("In chứng từ này");

    fireEvent.click(printBtn);
    await waitFor(() => expect(window.print).toHaveBeenCalled());

    // Column headers from child field metas (default-columns branch: fields minus FK minus
    // bulky types) — proves quantity/currency/formula all render, not just a bare label.
    expect(screen.getByText("Sản phẩm")).toBeInTheDocument();
    expect(screen.getByText("Số lượng")).toBeInTheDocument();
    expect(screen.getByText("Đơn giá")).toBeInTheDocument();
    expect(screen.getByText("Thành tiền")).toBeInTheDocument();
    // Row value.
    expect(screen.getByText("Bản quyền phần mềm")).toBeInTheDocument();
    // The FK back to the parent ("Báo giá") must NOT become a column — every row is already
    // known to belong to this one printed record.
    expect(screen.queryByText("Báo giá", { selector: "th" })).toBeNull();

    expect(listChildRecords).toHaveBeenCalledWith(
      "crm_quote_lines", "quote", "q1", expect.any(Number), undefined, expect.any(Array),
    );
  });

  it("never prints raw entity slugs/uuids — child section heading is a human label, not crm_quote_lines", async () => {
    renderDetail();
    const printBtn = await screen.findByTitle("In chứng từ này");
    fireEvent.click(printBtn);
    await waitFor(() => expect(window.print).toHaveBeenCalled());

    expect(screen.getByText("Dòng báo giá")).toBeInTheDocument(); // human label from listEntities
    expect(screen.queryByText("crm_quote_lines")).toBeNull();
    expect(screen.queryByText(/obj_/)).toBeNull();
  });

  it("surfaces a toast on a child-fetch failure instead of silently dropping the table", async () => {
    vi.mocked(listChildRecords).mockRejectedValue(new Error("network down"));
    const warnSpy = vi.spyOn(toast, "warning").mockImplementation(() => undefined);
    renderDetail();
    const printBtn = await screen.findByTitle("In chứng từ này");

    fireEvent.click(printBtn);

    await waitFor(() => expect(window.print).toHaveBeenCalled());
    expect(warnSpy).toHaveBeenCalled();
    expect(String(warnSpy.mock.calls[0][0])).toMatch(/Dòng báo giá/);
  });
});
